package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// observability-05: every Chat and tool call reports one Outcome to the
// client's observer, classified the way llm-02 classifies the error.

// observerFake is an OpenAI-compatible endpoint whose reply depends on a
// marker in the request: "rate" is a 429, "server" a 500, "slow" answers
// after 2 s, "empty" a blank completion, anything else a completion with
// usage prompt 7, completion 3, total 10. It never reaches a provider.
func observerFake(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Retry-After", "0")
		switch {
		case strings.Contains(body, "marker-rate"):
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		case strings.Contains(body, "marker-server"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		case strings.Contains(body, "marker-slow"):
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(usageChatJSON("late", 7, 3, 10))
		case strings.Contains(body, "marker-empty"):
			_, _ = w.Write(usageChatJSON("", 2, 0, 2))
		default:
			_, _ = w.Write(usageChatJSON("fine", 7, 3, 10))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// usageChatJSON is a completion with the full usage breakdown.
func usageChatJSON(content string, prompt, completion, total int) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]string{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": prompt,
			"completion_tokens": completion, "total_tokens": total},
	})
	return b
}

// outcomeLog collects the outcomes an observer receives.
type outcomeLog struct {
	mu  sync.Mutex
	all []Outcome
}

func (l *outcomeLog) observe(o Outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, o)
}

func (l *outcomeLog) byResult() map[Result]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[Result]int{}
	for _, o := range l.all {
		out[o.Result]++
	}
	return out
}

func (l *outcomeLog) only(t *testing.T) Outcome {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.all) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one", l.all)
	}
	return l.all[0]
}

func observedClient(t *testing.T, endpoint string) (*Client, *outcomeLog) {
	t.Helper()
	c := New(&config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: "k",
		Model: "test-model", TimeoutSeconds: 1, TokenBudgetDaily: 10_000_000,
		CooldownSeconds: 60}, noopLog)
	c.retryDelays = []time.Duration{0, 0, 0}
	log := &outcomeLog{}
	c.SetObserver(log.observe)
	return c, log
}

// Error propagation: a 429, a 500, a timeout, a blank completion and a
// success each add exactly one outcome of their own result.
func TestObserverCountsEachProviderResultOnce(t *testing.T) {
	c, log := observedClient(t, observerFake(t).URL)
	for _, marker := range []string{"rate", "server", "ok", "slow", "empty"} {
		_, _, _ = c.Chat(context.Background(), "system", "marker-"+marker, 50)
	}
	want := map[Result]int{ResultRateLimited: 1, ResultError: 1, ResultOK: 1,
		ResultTimeout: 1, ResultEmpty: 1}
	got := log.byResult()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("outcomes by result = %v, want %v", got, want)
	}
	for _, o := range log.all {
		switch o.Result {
		case ResultOK:
			if o.PromptTokens != 7 || o.CompletionTokens != 3 || o.Tokens != 10 {
				t.Errorf("ok outcome tokens = %+v, want prompt 7, completion 3, total 10", o)
			}
			// A loopback exchange can take under the clock's granularity
			// (0 on Windows); the timeout case proves the duration is timed.
			if o.Duration < 0 || o.Duration > time.Second {
				t.Errorf("ok outcome duration = %v, want 0-1 s", o.Duration)
			}
		case ResultTimeout:
			if o.Duration < 900*time.Millisecond || o.Tokens != 0 {
				t.Errorf("timeout outcome = %+v, want >= ~1 s and no tokens", o)
			}
		case ResultEmpty:
			if o.PromptTokens != 2 || o.Tokens != 2 {
				t.Errorf("empty outcome = %+v, want the 2 charged tokens kept", o)
			}
		}
	}
}

// State transitions: local refusals are outcomes too. An open breaker is
// circuit_open, a spent budget budget_refused, a cancelled caller
// cancelled; the breaker's own state is not changed by observing.
func TestObserverLocalRefusals(t *testing.T) {
	srv := observerFake(t)
	t.Run("circuit_open", func(t *testing.T) {
		c, log := observedClient(t, srv.URL)
		for range failureThreshold {
			c.recordFailure()
		}
		_, _, err := c.Chat(context.Background(), "s", "marker-ok", 50)
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("err = %v, want ErrCircuitOpen", err)
		}
		if o := log.only(t); o.Result != ResultCircuitOpen {
			t.Errorf("result = %q, want circuit_open", o.Result)
		}
	})
	t.Run("budget_refused", func(t *testing.T) {
		c, log := observedClient(t, srv.URL)
		c.Reconfigure(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
			Model: "test-model", TimeoutSeconds: 1, TokenBudgetDaily: 10,
			CooldownSeconds: 60})
		_, _, err := c.Chat(context.Background(), "s", "marker-ok", 500)
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("err = %v, want ErrBudgetExhausted", err)
		}
		if o := log.only(t); o.Result != ResultBudgetRefused || o.Tokens != 0 {
			t.Errorf("outcome = %+v, want budget_refused with no tokens", o)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		c, log := observedClient(t, srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		_, _, _ = c.Chat(ctx, "s", "marker-slow", 50)
		if o := log.only(t); o.Result != ResultCancelled {
			t.Errorf("result = %q, want cancelled", o.Result)
		}
		if got := StatusOf(c).CircuitState; got != "closed" {
			t.Errorf("breaker after a cancellation = %q, want closed", got)
		}
	})
}

// Nil/empty: a disabled client and the per-work-item cooldown are not
// requests and report nothing; a client without an observer works.
func TestObserverSkipsDisabledAndCooldown(t *testing.T) {
	srv := observerFake(t)
	c, log := observedClient(t, srv.URL)
	if _, _, err := c.Chat(context.Background(), "s", "marker-ok", 50); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, _, err := c.Chat(context.Background(), "s", "marker-ok", 50)
	if !errors.Is(err, ErrRequestCooldown) {
		t.Fatalf("repeat call err = %v, want ErrRequestCooldown", err)
	}
	c.Reconfigure(&config.LLMConfig{Enabled: false})
	if _, _, err := c.Chat(context.Background(), "s", "marker-other", 50); err == nil {
		t.Fatal("disabled client answered")
	}
	if _, err := c.ChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, ToolOptions{}); err == nil {
		t.Fatal("disabled client answered a tool call")
	}
	if got := log.byResult(); len(got) != 1 || got[ResultOK] != 1 {
		t.Errorf("outcomes = %v, want only the first call's ok", got)
	}
	plain := New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 1, CooldownSeconds: 60}, noopLog)
	plain.SetObserver(nil)
	if _, _, err := plain.Chat(context.Background(), "s", "marker-ok", 50); err != nil {
		t.Errorf("client without observer: %v", err)
	}
}

// Happy path and error propagation on the tool path: one outcome per
// ChatWithTools, with the provider's token breakdown; a 429 is
// rate_limited at once (no retry on the tool path).
func TestObserverToolCalls(t *testing.T) {
	c, log := observedClient(t, observerFake(t).URL)
	msgs := []Message{{Role: "user", Content: "marker-ok tools"}}
	res, err := c.ChatWithTools(context.Background(), msgs, nil, ToolOptions{MaxTokens: 50})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	o := log.only(t)
	if o.Result != ResultOK || o.PromptTokens != 7 || o.CompletionTokens != 3 ||
		o.Tokens != res.Tokens {
		t.Errorf("tool outcome = %+v (result tokens %d), want ok 7/3", o, res.Tokens)
	}
	_, err = c.ChatWithTools(context.Background(),
		[]Message{{Role: "user", Content: "marker-rate tools"}}, nil, ToolOptions{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if got := log.byResult(); got[ResultRateLimited] != 1 || len(got) != 2 {
		t.Errorf("outcomes = %v, want one ok and one rate_limited", got)
	}
}

// Concurrent access: 64 parallel calls report 64 outcomes, while the
// observer is replaced mid-flight without a lost or doubled outcome.
func TestObserverConcurrentChats(t *testing.T) {
	c, log := observedClient(t, observerFake(t).URL)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := c.Chat(context.Background(), "s",
				fmt.Sprintf("marker-ok %d", i), 50); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		}()
	}
	c.SetObserver(log.observe) // a concurrent replacement with the same sink
	wg.Wait()
	if got := log.byResult(); got[ResultOK] != 64 || len(got) != 1 {
		t.Fatalf("outcomes = %v, want 64 ok", got)
	}
}

// Invalid input and boundaries of the classifier: wrapped sentinels and
// classified provider failures map to their result; an unknown error is
// "error"; disabled and cooldown are not counted.
func TestOutcomeResultMapping(t *testing.T) {
	cases := []struct {
		err     error
		want    Result
		counted bool
	}{
		{nil, ResultOK, true},
		{fmt.Errorf("x: %w", ErrEmptyResponse), ResultEmpty, true},
		{fmt.Errorf("%w (last error: server)", ErrCircuitOpen), ResultCircuitOpen, true},
		{budgetError("daily token budget exhausted (5/5)"), ResultBudgetRefused, true},
		{&providerFailure{class: classRate, err: ErrRateLimited}, ResultRateLimited, true},
		{&providerFailure{class: classTimeout, err: context.DeadlineExceeded},
			ResultTimeout, true},
		{&providerFailure{class: classCanceled, err: context.Canceled}, ResultCancelled, true},
		{&providerFailure{class: classAuth, err: errors.New("401")}, ResultError, true},
		{&providerFailure{class: classCaller, err: errors.New("400")}, ResultError, true},
		{context.Canceled, ResultCancelled, true},
		{context.DeadlineExceeded, ResultTimeout, true},
		{ErrPromptTooLarge, ResultError, true},
		{errors.New("unknown"), ResultError, true},
		{ErrLLMDisabled, "", false},
		{fmt.Errorf("%w: reconfigured", ErrLLMDisabled), "", false},
		{fmt.Errorf("%w for work item", ErrRequestCooldown), "", false},
	}
	for _, tc := range cases {
		got, counted := outcomeResult(tc.err)
		if got != tc.want || counted != tc.counted {
			t.Errorf("outcomeResult(%v) = %q,%v; want %q,%v", tc.err, got, counted,
				tc.want, tc.counted)
		}
	}
	if len(Results()) != 8 {
		t.Errorf("Results() = %v, want the 8 llm-02 classes", Results())
	}
}

// Boundary: a reply that lands after a Reconfigure is discarded on both
// paths, and neither counts it: the chat path's error is the disabled
// sentinel, as the tool path's is, and keeps its message.
func TestObserverSkipsReplyDiscardedByReconfigure(t *testing.T) {
	if !errors.Is(errReconfiguredDuringRequest, ErrLLMDisabled) {
		t.Fatalf("%v does not wrap ErrLLMDisabled", errReconfiguredDuringRequest)
	}
	if msg := errReconfiguredDuringRequest.Error(); !strings.HasPrefix(msg,
		"LLM disabled or reconfigured during request") {
		t.Errorf("message = %q, want the previous text kept as its prefix", msg)
	}
	if result, counted := outcomeResult(errReconfiguredDuringRequest); counted {
		t.Errorf("discarded reply counted as %q", result)
	}
}
