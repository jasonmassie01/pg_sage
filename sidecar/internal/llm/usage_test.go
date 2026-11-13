package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-04: every call lands in an attributed usage accumulator, keyed by
// UTC day, database, client role, purpose and model, with the prompt,
// completion and reasoning split and an optional operator-supplied cost.

// usageFake answers every chat request with body.
func usageFake(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func usageClient(t *testing.T, endpoint string, acc *UsageAccumulator) *Client {
	t.Helper()
	c := New(&config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: "k",
		Model: "test-model", TimeoutSeconds: 2, TokenBudgetDaily: 10_000_000}, noopLog)
	c.retryDelays = []time.Duration{0, 0, 0}
	c.SetObserver(func(o Outcome) { acc.Record("orders", "general", o) })
	return c
}

func price(in, out float64) config.LLMPrice {
	return config.LLMPrice{InputPerM: &in, OutputPerM: &out}
}

func onlyKey(t *testing.T, acc *UsageAccumulator) (UsageKey, UsageCounts) {
	t.Helper()
	pending := acc.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want exactly one key", pending)
	}
	for k, v := range pending {
		return k, v
	}
	panic("unreachable")
}

func todayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Happy path: a provider reply of 1200 prompt and 300 completion tokens
// reaches the key's row split exactly, under the context's purpose.
func TestUsageProviderSplitReachesRowExactly(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	c := usageClient(t, usageFake(t, string(usageChatJSON("ok", 1200, 300, 1500))).URL, acc)
	if _, _, err := c.Chat(WithPurpose(context.Background(), "briefing"),
		"system", "user", 400); err != nil {
		t.Fatalf("chat: %v", err)
	}
	k, v := onlyKey(t, acc)
	want := UsageKey{Day: todayUTC(), Database: "orders", Role: "general",
		Purpose: "briefing", Model: "test-model"}
	if k != want {
		t.Fatalf("key = %+v, want %+v", k, want)
	}
	if v.PromptTokens != 1200 || v.CompletionTokens != 300 || v.ReasoningTokens != 0 ||
		v.Calls != 1 || v.Failures != 0 || v.Estimated {
		t.Fatalf("counts = %+v, want prompt 1200, completion 300, 1 call, provider", v)
	}
}

// Tool calls carry the reasoning split; a reasoning price, when set,
// prices those completion tokens instead of the output price.
func TestUsageToolReasoningSplitAndCost(t *testing.T) {
	in, out, reasoning := 1.0, 2.0, 4.0
	pricing := PricingFrom(map[string]config.LLMPrice{"test-model": {
		InputPerM: &in, OutputPerM: &out, ReasoningPerM: &reasoning}})
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, pricing, noopLog)
	body := `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,` +
		`"completion_tokens_details":{"reasoning_tokens":100}}}`
	c := usageClient(t, usageFake(t, body).URL, acc)
	_, err := c.ChatWithTools(WithPurpose(context.Background(), "query_tuning"),
		[]Message{{Role: "user", Content: "tune"}}, nil, ToolOptions{MaxTokens: 400})
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	k, v := onlyKey(t, acc)
	if k.Purpose != "query_tuning" || v.ReasoningTokens != 100 ||
		v.PromptTokens != 1200 || v.CompletionTokens != 300 {
		t.Fatalf("key %+v counts %+v, want query_tuning 1200/300/100", k, v)
	}
	wantCost := (1200*1.0 + 200*2.0 + 100*4.0) / 1e6
	if !v.Priced || math.Abs(v.CostUSD-wantCost) > 1e-12 {
		t.Fatalf("cost = %v priced=%v, want %v", v.CostUSD, v.Priced, wantCost)
	}
}

// Nil/empty: a reply without usage is estimated (source estimate) and
// split prompt/completion from the request text; an empty pricing map
// leaves the cost unpriced (NULL in the ledger).
func TestUsageMissingUsageIsEstimatedAndUnpriced(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(map[string]config.LLMPrice{}),
		noopLog)
	body := `{"choices":[{"message":{"content":"` + strings.Repeat("a", 400) +
		`"},"finish_reason":"stop"}]}`
	c := usageClient(t, usageFake(t, body).URL, acc)
	system, user := strings.Repeat("s", 400), strings.Repeat("u", 400)
	_, tokens, err := c.Chat(context.Background(), system, user, 400)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	k, v := onlyKey(t, acc)
	if k.Purpose != PurposeUnlabeled {
		t.Fatalf("purpose = %q, want %q for an unlabelled call", k.Purpose, PurposeUnlabeled)
	}
	if !v.Estimated || v.PromptTokens != int64(estimateTokens(system, user)) ||
		v.PromptTokens+v.CompletionTokens != int64(tokens) || v.CompletionTokens <= 0 {
		t.Fatalf("counts = %+v (charged %d), want an estimated split summing to the charge",
			v, tokens)
	}
	if v.Priced || v.CostUSD != 0 {
		t.Fatalf("cost = %v priced=%v, want unpriced with an empty pricing map",
			v.CostUSD, v.Priced)
	}
}

// A priced model gets prompt x input + completion x output per million.
func TestUsagePricedCost(t *testing.T) {
	pricing := PricingFrom(map[string]config.LLMPrice{"test-model": price(0.15, 0.60)})
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, pricing, noopLog)
	c := usageClient(t, usageFake(t, string(usageChatJSON("ok", 1200, 300, 1500))).URL, acc)
	for i := 0; i < 2; i++ {
		_, _, _ = c.Chat(WithPurpose(context.Background(), "explain"), "s", fmt.Sprint("u", i),
			400)
	}
	_, v := onlyKey(t, acc)
	want := 2 * (1200*0.15 + 300*0.60) / 1e6
	if !v.Priced || math.Abs(v.CostUSD-want) > 1e-12 || v.Calls != 2 {
		t.Fatalf("counts = %+v, want 2 calls costing %v", v, want)
	}
}

// Error propagation: a provider failure is a call and a failure, with no
// tokens.
func TestUsageCountsFailures(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	c := usageClient(t, observerFake(t).URL, acc)
	_, _, err := c.Chat(WithPurpose(context.Background(), "rca_tier2"), "s", "marker-server", 50)
	if err == nil {
		t.Fatal("want the 500 to fail the call")
	}
	_, v := onlyKey(t, acc)
	if v.Calls != 1 || v.Failures != 1 || v.PromptTokens != 0 || v.CompletionTokens != 0 {
		t.Fatalf("counts = %+v, want 1 call, 1 failure, no tokens", v)
	}
}

// Boundary: calls either side of UTC midnight land on two day keys.
func TestUsageMidnightSplitsIntoTwoDays(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	before := time.Date(2026, 11, 12, 23, 59, 59, 0, time.UTC)
	clock := before
	acc.now = func() time.Time { return clock }
	o := Outcome{Result: ResultOK, Purpose: "briefing", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, Tokens: 15}
	acc.Record("orders", "general", o)
	clock = before.Add(2 * time.Second)
	acc.Record("orders", "general", o)
	var flushed map[UsageKey]UsageCounts
	if err := acc.Flush(context.Background(), func(_ context.Context,
		d map[UsageKey]UsageCounts) error {
		flushed = d
		return nil
	}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(flushed) != 2 {
		t.Fatalf("flushed %d keys, want 2 (one per UTC day): %+v", len(flushed), flushed)
	}
	for _, day := range []time.Time{time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)} {
		k := UsageKey{Day: day, Database: "orders", Role: "general", Purpose: "briefing",
			Model: "m"}
		if v := flushed[k]; v.Calls != 1 || v.PromptTokens != 10 {
			t.Errorf("day %s counts = %+v, want 1 call, 10 prompt", day.Format(time.DateOnly), v)
		}
	}
	if len(acc.Pending()) != 0 {
		t.Fatalf("pending after a successful flush = %+v, want empty", acc.Pending())
	}
}

// Boundary: the key after the cap goes to purpose overflow; existing keys
// keep counting.
func TestUsageOverflowAfterMaxKeys(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	for i := 0; i < DefaultUsageMaxKeys; i++ {
		acc.Record("orders", "general", Outcome{Result: ResultOK,
			Purpose: fmt.Sprintf("p%05d", i), Model: "m", Tokens: 1, PromptTokens: 1})
	}
	acc.Record("orders", "general", Outcome{Result: ResultOK, Purpose: "late",
		Model: "m", Tokens: 3, PromptTokens: 3})
	acc.Record("orders", "general", Outcome{Result: ResultOK, Purpose: "p00000",
		Model: "m", Tokens: 1, PromptTokens: 1})
	pending := acc.Pending()
	if len(pending) != DefaultUsageMaxKeys+1 {
		t.Fatalf("%d keys, want %d (the cap plus overflow)", len(pending),
			DefaultUsageMaxKeys+1)
	}
	day := todayUTC()
	over := pending[UsageKey{Day: day, Database: "orders", Role: "general",
		Purpose: PurposeOverflow, Model: "m"}]
	if over.Calls != 1 || over.PromptTokens != 3 {
		t.Fatalf("overflow = %+v, want the 10,001st key's call", over)
	}
	if _, ok := pending[UsageKey{Day: day, Database: "orders", Role: "general",
		Purpose: "late", Model: "m"}]; ok {
		t.Fatal("the key past the cap was stored under its own purpose")
	}
	if first := pending[UsageKey{Day: day, Database: "orders", Role: "general",
		Purpose: "p00000", Model: "m"}]; first.Calls != 2 {
		t.Fatalf("existing key calls = %d, want 2", first.Calls)
	}
}

// Error propagation: a failed flush keeps the deltas, logs once per
// streak, and the next successful flush carries everything.
func TestUsageFailedFlushRetainsDeltasAndLogsOnce(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logFn := func(_ string, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), logFn)
	o := Outcome{Result: ResultOK, Purpose: "briefing", Model: "m",
		PromptTokens: 100, CompletionTokens: 20, Tokens: 120}
	failing := func(context.Context, map[UsageKey]UsageCounts) error {
		return errors.New(`relation "sage.llm_usage" does not exist`)
	}
	acc.Record("orders", "general", o)
	for i := 0; i < 2; i++ {
		if err := acc.Flush(context.Background(), failing); err == nil {
			t.Fatal("flush into a failing sink returned nil")
		}
		acc.Record("orders", "general", o)
	}
	mu.Lock()
	failures := len(lines)
	mu.Unlock()
	if failures != 1 || !strings.Contains(lines[0], "does not exist") {
		t.Fatalf("log lines after two failed flushes = %q, want exactly one naming the error",
			lines)
	}
	var got UsageCounts
	if err := acc.Flush(context.Background(), func(_ context.Context,
		d map[UsageKey]UsageCounts) error {
		for _, v := range d {
			got = v
		}
		return nil
	}); err != nil {
		t.Fatalf("recovered flush: %v", err)
	}
	if got.Calls != 3 || got.PromptTokens != 300 || got.CompletionTokens != 60 {
		t.Fatalf("recovered flush = %+v, want the 3 retained calls (300/60)", got)
	}
}

// Nil/empty: an empty accumulator never calls the sink; a nil one ignores
// records; negative token counts are clamped, never subtracted.
func TestUsageEmptyNilAndNegative(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, nil, noopLog)
	called := false
	if err := acc.Flush(context.Background(), func(context.Context,
		map[UsageKey]UsageCounts) error {
		called = true
		return nil
	}); err != nil || called {
		t.Fatalf("empty flush: err %v, sink called %v; want neither", err, called)
	}
	var none *UsageAccumulator
	none.Record("orders", "general", Outcome{Result: ResultOK, Tokens: 5})
	if err := none.Flush(context.Background(), nil); err != nil {
		t.Fatalf("nil accumulator flush: %v", err)
	}
	acc.Record("", "general", Outcome{Result: ResultOK, Purpose: "explain", Model: "m",
		PromptTokens: -5, CompletionTokens: 4, Tokens: 4})
	_, v := onlyKey(t, acc)
	if v.PromptTokens != 0 || v.CompletionTokens != 4 || v.Priced {
		t.Fatalf("counts = %+v, want prompt clamped to 0, completion 4, unpriced", v)
	}
}

// Boundary: a key is priced only while every call in it was, whichever
// order the priced and unpriced calls arrive in.
func TestUsageMixedPricingUnderOneKeyIsUnpriced(t *testing.T) {
	for _, order := range [][]bool{{true, false}, {false, true}} {
		i := 0
		prices := func(string) (config.LLMPrice, bool) {
			priced := order[i]
			i++
			return price(1, 1), priced
		}
		acc := NewUsageAccumulator(DefaultUsageMaxKeys, prices, noopLog)
		for range order {
			acc.Record("orders", "general", Outcome{Result: ResultOK, Purpose: "explain",
				Model: "m", PromptTokens: 10, CompletionTokens: 5, Tokens: 15})
		}
		_, v := onlyKey(t, acc)
		if v.Priced || v.Calls != 2 {
			t.Fatalf("order %v: counts = %+v, want 2 calls and unpriced", order, v)
		}
	}
}

// Boundary: tokens the provider charged beyond its prompt/completion
// split (a model's thinking tokens, for one) count as completion, so the
// ledger never shows less than the budget charged.
func TestUsageChargedBeyondSplitCountsAsCompletion(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys,
		PricingFrom(map[string]config.LLMPrice{"m": price(1, 2)}), noopLog)
	acc.Record("orders", "general", Outcome{Result: ResultOK, Purpose: "explain",
		Model: "m", PromptTokens: 100, CompletionTokens: 50, Tokens: 180})
	_, v := onlyKey(t, acc)
	if v.PromptTokens != 100 || v.CompletionTokens != 80 || v.Estimated {
		t.Fatalf("counts = %+v, want prompt 100, completion 80, provider source", v)
	}
	if want := (100*1 + 80*2) / 1e6; !v.Priced || math.Abs(v.CostUSD-want) > 1e-12 {
		t.Fatalf("cost = %v priced=%v, want %v", v.CostUSD, v.Priced, want)
	}
}

// Concurrency: 64 goroutines x 100 calls, with flushes racing them, total
// exactly 6400 calls.
func TestUsageConcurrentRecordsAndFlushes(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	var mu sync.Mutex
	var flushedCalls int64
	sink := func(_ context.Context, d map[UsageKey]UsageCounts) error {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range d {
			flushedCalls += v.Calls
		}
		return nil
	}
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				acc.Record(fmt.Sprint("db", g%4), "general", Outcome{Result: ResultOK,
					Purpose: "briefing", Model: "m", PromptTokens: 1, Tokens: 1})
				if i%25 == 0 {
					_ = acc.Flush(context.Background(), sink)
				}
			}
		}(g)
	}
	wg.Wait()
	if err := acc.Flush(context.Background(), sink); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	if flushedCalls != 6400 {
		t.Fatalf("flushed calls = %d, want exactly 6400", flushedCalls)
	}
}

// State transitions: a purpose label is set, kept or refused.
func TestPurposeContextLabels(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"none", ctx, PurposeUnlabeled},
		{"set", WithPurpose(ctx, "briefing"), "briefing"},
		{"override", WithPurpose(WithPurpose(ctx, "agent_loop"), "ask"), "ask"},
		{"default keeps caller", WithDefaultPurpose(WithPurpose(ctx, "ask"), "agent_loop"),
			"ask"},
		{"default fills", WithDefaultPurpose(ctx, "agent_loop"), "agent_loop"},
		{"reserved total", WithPurpose(ctx, "_total"), PurposeUnlabeled},
		{"uppercase", WithPurpose(ctx, "Briefing"), PurposeUnlabeled},
		{"empty", WithPurpose(ctx, ""), PurposeUnlabeled},
		{"too long", WithPurpose(ctx, "a"+strings.Repeat("b", 63)), PurposeUnlabeled},
		{"invalid keeps caller", WithPurpose(WithPurpose(ctx, "ask"), "bad purpose"), "ask"},
	}
	for _, c := range cases {
		if got := PurposeOf(c.ctx); got != c.want {
			t.Errorf("%s: PurposeOf = %q, want %q", c.name, got, c.want)
		}
	}
	if !ValidPurpose("a"+strings.Repeat("b", 62)) || ValidPurpose("1abc") {
		t.Error("ValidPurpose boundary: 63 chars must pass, a leading digit must not")
	}
}

// Manager.ChatForPurpose labels the call with its routing purpose unless
// the caller already did.
func TestManagerChatForPurposeLabelsTheCall(t *testing.T) {
	acc := NewUsageAccumulator(DefaultUsageMaxKeys, PricingFrom(nil), noopLog)
	c := usageClient(t, usageFake(t, string(usageChatJSON("ok", 5, 1, 6))).URL, acc)
	m := NewManager(c, nil, false)
	if _, _, err := m.ChatForPurpose(context.Background(), "advisor", "s", "u1", 50); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, _, err := m.ChatForPurpose(WithPurpose(context.Background(), "briefing"),
		"advisor", "s", "u2", 50); err != nil {
		t.Fatalf("chat: %v", err)
	}
	got := map[string]int64{}
	for k, v := range acc.Pending() {
		got[k.Purpose] += v.Calls
	}
	if got["advisor"] != 1 || got["briefing"] != 1 || len(got) != 2 {
		t.Fatalf("calls by purpose = %v, want advisor 1, briefing 1", got)
	}
}

// The outcome names the client's model and purpose.
func TestOutcomeCarriesModelAndPurpose(t *testing.T) {
	c, log := observedClient(t, observerFake(t).URL)
	_, _, _ = c.Chat(WithPurpose(context.Background(), "facts"), "s", "marker-ok", 50)
	o := log.only(t)
	if o.Model != "test-model" || o.Purpose != "facts" || o.Estimated {
		t.Fatalf("outcome = %+v, want model test-model, purpose facts, provider usage", o)
	}
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), "marker-ok") {
		t.Fatalf("outcome leaks prompt text: %s", raw)
	}
}
