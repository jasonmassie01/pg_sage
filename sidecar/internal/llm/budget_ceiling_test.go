package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-03: (*Client).SetCeiling makes a policy cap (budgets.llm_tokens_daily)
// binding. The effective daily limit is the stricter of
// llm.token_budget_daily and the ceiling; an absent ceiling means config only.

// countingProvider answers every chat with usage tokens and counts calls.
func countingProvider(t *testing.T, usage int) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":%d}}`, usage)
		}))
	t.Cleanup(server.Close)
	return server.URL, &calls
}

func ceilingClient(url string, daily int) *llm.Client {
	return llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "test-key", Model: "test-model",
		TimeoutSeconds: 5, TokenBudgetDaily: daily,
	}, func(string, string, ...any) {})
}

func fixedCeiling(value int64, present bool) func() (int64, bool) {
	return func() (int64, bool) { return value, present }
}

func chatNeeding(c *llm.Client, maxTokens int, prompt string) error {
	_, _, err := c.Chat(context.Background(), "sys", prompt, maxTokens)
	return err
}

func TestBudgetCeiling_PolicyStricterThanConfigRefuses(t *testing.T) {
	url, calls := countingProvider(t, 10)
	c := ceilingClient(url, 500000)
	c.SetCeiling(fixedCeiling(100, true))

	err := chatNeeding(c, 101, "needs 101")
	if !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("101 under a 100 policy cap = %v, want ErrBudgetExhausted", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0 (refused before I/O)", calls.Load())
	}
	if err := chatNeeding(c, 100, "needs 100"); err != nil {
		t.Fatalf("100 under a 100 policy cap: %v", err)
	}
	if got := c.EffectiveTokenBudgetDaily(); got != 100 {
		t.Fatalf("effective budget = %d, want 100", got)
	}
}

func TestBudgetCeiling_ConfigStricterThanPolicyWins(t *testing.T) {
	url, _ := countingProvider(t, 10)
	c := ceilingClient(url, 50)
	c.SetCeiling(fixedCeiling(100, true))

	if err := chatNeeding(c, 51, "needs 51"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("51 under config 50 = %v, want ErrBudgetExhausted", err)
	}
	if err := chatNeeding(c, 50, "needs 50"); err != nil {
		t.Fatalf("50 under config 50: %v", err)
	}
	if got := c.EffectiveTokenBudgetDaily(); got != 50 {
		t.Fatalf("effective budget = %d, want 50", got)
	}
}

func TestBudgetCeiling_AbsentOrNilMeansConfigOnly(t *testing.T) {
	for name, ceiling := range map[string]func() (int64, bool){
		"absent": fixedCeiling(0, false),
		"nil":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			url, _ := countingProvider(t, 10)
			c := ceilingClient(url, 50)
			c.SetCeiling(ceiling)
			if err := chatNeeding(c, 51, "needs 51"); !errors.Is(err, llm.ErrBudgetExhausted) {
				t.Fatalf("51 under config 50 = %v, want ErrBudgetExhausted", err)
			}
			if err := chatNeeding(c, 50, "needs 50"); err != nil {
				t.Fatalf("50 under config 50: %v", err)
			}
			if got := c.EffectiveTokenBudgetDaily(); got != 50 {
				t.Fatalf("effective budget = %d, want config 50", got)
			}
		})
	}
}

// An unlimited config (0) takes the policy cap as its only limit.
func TestBudgetCeiling_UnlimitedConfigTakesPolicyCap(t *testing.T) {
	url, _ := countingProvider(t, 10)
	c := ceilingClient(url, 0)
	c.SetCeiling(fixedCeiling(100, true))
	if err := chatNeeding(c, 101, "needs 101"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("101 under a 100 cap with unlimited config = %v", err)
	}
	if err := chatNeeding(c, 100, "needs 100"); err != nil {
		t.Fatalf("100 under a 100 cap: %v", err)
	}
}

// A policy cap of 0 tokens refuses every call (fail closed), unlike the
// config's 0, which means unlimited.
func TestBudgetCeiling_ZeroPolicyCapRefusesAll(t *testing.T) {
	url, calls := countingProvider(t, 10)
	c := ceilingClient(url, 500000)
	c.SetCeiling(fixedCeiling(0, true))
	if err := chatNeeding(c, 1, "needs 1"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("1 under a 0 policy cap = %v, want ErrBudgetExhausted", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", calls.Load())
	}
	if !c.IsBudgetExhausted() {
		t.Fatal("a 0 policy cap must report the budget exhausted")
	}
}

// Spend that reaches the policy cap marks the client exhausted, and the
// status reports the effective budget, not the looser config.
func TestBudgetCeiling_ExhaustionAndStatusUseEffectiveLimit(t *testing.T) {
	url, _ := countingProvider(t, 100)
	c := ceilingClient(url, 500000)
	c.SetCeiling(fixedCeiling(100, true))
	if err := chatNeeding(c, 100, "spend 100"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !c.IsBudgetExhausted() {
		t.Fatal("100 used of a 100 policy cap must be exhausted")
	}
	status := llm.StatusOf(c)
	if status.TokenBudget != 100 || !status.Exhausted || status.TokensUsed != 100 {
		t.Fatalf("status = %+v, want budget 100, used 100, exhausted", status)
	}
	if got := c.TokenBudgetDaily(); got != 500000 {
		t.Fatalf("configured budget = %d, want 500000 unchanged", got)
	}
}

// The ceiling is read per request, so a lowered policy binds at once.
func TestBudgetCeiling_ReadOnEveryRequest(t *testing.T) {
	url, _ := countingProvider(t, 10)
	c := ceilingClient(url, 500000)
	var limit atomic.Int64
	limit.Store(1000)
	c.SetCeiling(func() (int64, bool) { return limit.Load(), true })
	if err := chatNeeding(c, 500, "first"); err != nil {
		t.Fatalf("500 under 1000: %v", err)
	}
	limit.Store(20)
	if err := chatNeeding(c, 15, "second"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("10 used + 15 under a lowered cap of 20 = %v", err)
	}
}
