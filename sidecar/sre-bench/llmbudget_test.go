package srebench

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Roadmap 2.4: a live model only runs behind PG_SAGE_LIVE_LLM=1 and hard
// caps (requests, tokens, wall time and an estimated spend from the
// configured prices). The model tap admits each call against the caps
// before it leaves the process and settles it with the provider's
// reported usage; once a cap is reached every further call is refused
// (fail closed) and the run is marked exhausted.

// liveVars are the variables of a capped live run.
func liveVars(extra map[string]string) map[string]string {
	m := map[string]string{EnvLiveLLM: "1", EnvLLMURL: "https://llm.example/v1",
		EnvLLMModel: "gpt-4o-mini", EnvLLMKey: "sk-secret", EnvLLMMaxRequests: "300",
		EnvLLMMaxTokens: "2000000", EnvLLMMaxWall: "45m", EnvLLMMaxSpend: "2.50",
		EnvLLMPriceIn: "0.15", EnvLLMPriceOut: "0.60"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLLMConfigFromEnv_LiveNeedsTheOptInAndEveryCap(t *testing.T) {
	got, err := LLMConfigFromEnv(env(liveVars(nil)))
	if err != nil || got.Mode != LLMLive || got.budget == nil {
		t.Fatalf("capped live config = %+v (%v)", got, err)
	}
	want := BudgetCaps{MaxRequests: 300, MaxTokens: 2_000_000, MaxWall: 45 * time.Minute,
		MaxSpendUSD: 2.5, InputUSDPerMTok: 0.15, OutputUSDPerMTok: 0.60}
	if got.Caps != want {
		t.Fatalf("caps = %+v, want %+v", got.Caps, want)
	}
	for name, c := range map[string]struct {
		vars map[string]string
		want string
	}{
		"no opt-in":       {map[string]string{EnvLiveLLM: ""}, EnvLiveLLM},
		"opt-in not 1":    {map[string]string{EnvLiveLLM: "true"}, EnvLiveLLM},
		"no request cap":  {map[string]string{EnvLLMMaxRequests: ""}, EnvLLMMaxRequests},
		"zero requests":   {map[string]string{EnvLLMMaxRequests: "0"}, EnvLLMMaxRequests},
		"no token cap":    {map[string]string{EnvLLMMaxTokens: ""}, EnvLLMMaxTokens},
		"negative tokens": {map[string]string{EnvLLMMaxTokens: "-5"}, EnvLLMMaxTokens},
		"no wall cap":     {map[string]string{EnvLLMMaxWall: ""}, EnvLLMMaxWall},
		"bad wall":        {map[string]string{EnvLLMMaxWall: "forever"}, EnvLLMMaxWall},
		"zero wall":       {map[string]string{EnvLLMMaxWall: "0s"}, EnvLLMMaxWall},
		"no spend cap":    {map[string]string{EnvLLMMaxSpend: ""}, EnvLLMMaxSpend},
		"bad spend":       {map[string]string{EnvLLMMaxSpend: "lots"}, EnvLLMMaxSpend},
		"no input price":  {map[string]string{EnvLLMPriceIn: ""}, EnvLLMPriceIn},
		"negative price":  {map[string]string{EnvLLMPriceOut: "-1"}, EnvLLMPriceOut},
		"NaN price":       {map[string]string{EnvLLMPriceOut: "NaN"}, EnvLLMPriceOut},
	} {
		_, err := LLMConfigFromEnv(env(liveVars(c.vars)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("%s: the error leaks the key", name)
		}
	}
	if _, err := LLMConfigFromEnv(env(map[string]string{EnvLLMMaxRequests: "10"})); err == nil {
		t.Error("caps without a live endpoint were accepted")
	}
	free, err := LLMConfigFromEnv(env(liveVars(map[string]string{EnvLLMPriceIn: "0",
		EnvLLMPriceOut: "0"})))
	if err != nil || free.Caps.InputUSDPerMTok != 0 {
		t.Fatalf("a local model may be free: %+v (%v)", free.Caps, err)
	}
}

// budgetClock is a settable clock for budgets.
type budgetClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *budgetClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func caps() BudgetCaps {
	return BudgetCaps{MaxRequests: 3, MaxTokens: 10_000, MaxWall: time.Minute,
		MaxSpendUSD: 1, InputUSDPerMTok: 0.15, OutputUSDPerMTok: 0.60}
}

func TestBudget_RequestCapIsExact(t *testing.T) {
	b := NewBudget(caps(), (&budgetClock{now: time.Unix(0, 0)}).Now)
	for i := 0; i < 3; i++ {
		res, err := b.Admit(400, 100)
		if err != nil {
			t.Fatalf("call %d refused: %v", i+1, err)
		}
		b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 100, CompletionTokens: 50})
	}
	if _, err := b.Admit(400, 100); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 4: err = %v, want ErrBudgetExhausted", err)
	}
	rec := b.Record()
	if !rec.Exhausted || !strings.Contains(rec.Reason, "requests") || rec.Requests != 3 ||
		rec.Tokens != 450 {
		t.Fatalf("record = %+v", rec)
	}
	if _, err := b.Admit(1, 1); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal("an exhausted budget stays exhausted")
	}
}

func TestBudget_TokenCapCountsTheEstimateBeforeTheCall(t *testing.T) {
	b := NewBudget(caps(), (&budgetClock{now: time.Unix(0, 0)}).Now)
	// 36,000 bytes is about 9,000 prompt tokens; with 2,000 answer tokens
	// the call could pass the 10,000-token cap: refused before sending.
	if _, err := b.Admit(36_000, 2_000); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("an over-cap estimate: err = %v", err)
	}
	if rec := b.Record(); !strings.Contains(rec.Reason, "tokens") || rec.Requests != 0 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestBudget_SettledUsageReplacesTheEstimate(t *testing.T) {
	b := NewBudget(caps(), (&budgetClock{now: time.Unix(0, 0)}).Now)
	res, err := b.Admit(4_000, 1_000) // estimate 2,000 tokens
	if err != nil {
		t.Fatal(err)
	}
	b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 900, CompletionTokens: 100})
	if rec := b.Record(); rec.Tokens != 1_000 || rec.Exhausted {
		t.Fatalf("after settling: %+v", rec)
	}
	// A reply without usage counts the estimate (fail closed); an error
	// reply without usage counts nothing.
	res, _ = b.Admit(4_000, 1_000)
	b.Settle(res, http.StatusOK, TapUsage{})
	res, _ = b.Admit(4_000, 1_000)
	b.Settle(res, http.StatusTooManyRequests, TapUsage{})
	if rec := b.Record(); rec.Tokens != 3_000 || rec.Requests != 3 {
		t.Fatalf("estimate counted for an unreported reply: %+v", rec)
	}
}

func TestBudget_ActualUsageOverTheCapExhaustsIt(t *testing.T) {
	b := NewBudget(caps(), (&budgetClock{now: time.Unix(0, 0)}).Now)
	res, err := b.Admit(400, 100)
	if err != nil {
		t.Fatal(err)
	}
	b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 9_000, CompletionTokens: 1_500})
	rec := b.Record()
	if !rec.Exhausted || !strings.Contains(rec.Reason, "tokens") {
		t.Fatalf("usage over the cap = %+v", rec)
	}
}

func TestBudget_SpendCap(t *testing.T) {
	c := caps()
	c.MaxTokens, c.MaxSpendUSD, c.InputUSDPerMTok, c.OutputUSDPerMTok = 1e9, 0.01, 1, 4
	b := NewBudget(c, (&budgetClock{now: time.Unix(0, 0)}).Now)
	res, err := b.Admit(4_000, 1_000) // estimate 1,000*1 + 1,000*4 per million = $0.005
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 1_000, CompletionTokens: 1_000,
		ReasoningTokens: 200})
	rec := b.Record()
	if !near(rec.SpendUSD, 0.0058) {
		t.Fatalf("spend = %v, want 0.0058 (reasoning billed as output)", rec.SpendUSD)
	}
	if _, err := b.Admit(4_000, 1_000); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a call past the spend cap: err = %v", err)
	}
	if rec := b.Record(); !strings.Contains(rec.Reason, "spend") {
		t.Fatalf("reason = %q", rec.Reason)
	}
}

func TestBudget_WallTimeCap(t *testing.T) {
	clk := &budgetClock{now: time.Unix(0, 0)}
	b := NewBudget(caps(), clk.Now)
	if _, err := b.Admit(10, 10); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	if _, err := b.Admit(10, 10); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a call at the wall cap: err = %v", err)
	}
	if rec := b.Record(); !strings.Contains(rec.Reason, "wall") || rec.WallSeconds != 60 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestBudget_ConcurrentAdmitsNeverPassTheCap(t *testing.T) {
	c := caps()
	c.MaxRequests = 25
	b := NewBudget(c, (&budgetClock{now: time.Unix(0, 0)}).Now)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := b.Admit(4, 1); err == nil {
				ok.Add(1)
				b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 1, CompletionTokens: 1})
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 25 || b.Record().Requests != 25 {
		t.Fatalf("%d admitted, record %+v; want exactly 25", ok.Load(), b.Record())
	}
}

func TestBudget_NilBudgetAdmitsEverything(t *testing.T) {
	var b *Budget
	res, err := b.Admit(1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b.Settle(res, http.StatusOK, TapUsage{PromptTokens: 5})
	if rec := b.Record(); rec.Exhausted || rec.Requests != 0 {
		t.Fatalf("nil budget record = %+v", rec)
	}
}

func TestModelTap_RefusesCallsPastTheBudget(t *testing.T) {
	var upstreamCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":100,` +
			`"completion_tokens":20}}`))
	}))
	t.Cleanup(up.Close)
	c := caps()
	c.MaxRequests = 2
	tap := NewModelTap(up.URL)
	tap.budget = NewBudget(c, time.Now)
	t.Cleanup(tap.Close)
	codes := []int{}
	for i := 0; i < 4; i++ {
		resp, body := chat(t, tap.URL(), "hello")
		codes = append(codes, resp.StatusCode)
		if resp.StatusCode == http.StatusServiceUnavailable &&
			!strings.Contains(body, "budget") {
			t.Fatalf("refusal body %q must say the budget is spent", body)
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 503 || codes[3] != 503 ||
		upstreamCalls.Load() != 2 {
		t.Fatalf("codes %v, upstream calls %d", codes, upstreamCalls.Load())
	}
	rec := tap.budget.Record()
	if rec.Requests != 2 || rec.Tokens != 240 || !rec.Exhausted {
		t.Fatalf("record = %+v", rec)
	}
	if u := tap.Usage(); u.HTTPErrors != 2 || u.Calls != 4 {
		t.Fatalf("tap usage = %+v", u)
	}
}

func TestMaxOutputOf(t *testing.T) {
	for body, want := range map[string]int{
		`{"max_tokens": 700}`:                             700,
		`{"max_completion_tokens": 900}`:                  900,
		`{"max_tokens": 10, "max_completion_tokens": 20}`: 20,
		`{}`:                 defaultMaxOutput,
		`not json`:           defaultMaxOutput,
		`{"max_tokens": -4}`: defaultMaxOutput,
		`{"max_completion_tokens": 99999999999999999999999}`: defaultMaxOutput,
	} {
		if got := maxOutputOf([]byte(body)); got != want {
			t.Errorf("maxOutputOf(%s) = %d, want %d", body, got, want)
		}
	}
}

func TestBudgetRecord_JSONNamesCapsNotSecrets(t *testing.T) {
	cfg, err := LLMConfigFromEnv(env(liveVars(nil)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg.budget.Record())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"max_requests":300`, `"max_spend_usd":2.5`,
		`"exhausted":false`, `"spend_usd_estimate":`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("budget record %s lacks %s", raw, want)
		}
	}
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(string(raw), "llm.example") {
		t.Fatalf("the budget record names the key or endpoint: %s", raw)
	}
}
