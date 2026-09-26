package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// captureServer records every request body and answers with content.
func captureServer(
	t *testing.T, content string, withUsage bool,
) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			resp := map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]string{"content": content},
					"finish_reason": "stop",
				}},
			}
			if withUsage {
				resp["usage"] = map[string]int{"total_tokens": 100}
			}
			_ = json.NewEncoder(w).Encode(resp)
		}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func reviewClient(url string, jsonMode bool) *Client {
	return New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "k", Model: "m",
		TimeoutSeconds: 5, JSONMode: jsonMode,
	}, noopLog)
}

// G3-B11: json_mode must only send response_format for prompts that ask
// for JSON. Prose prompts (briefing) must not get json_object mode.
func TestChat_JSONModeOnlyForJSONPrompts(t *testing.T) {
	srv, bodies := captureServer(t, "ok", true)
	c := reviewClient(srv.URL, true)
	ctx := context.Background()
	if _, _, err := c.Chat(ctx, "Write a markdown briefing.", "u1", 50); err != nil {
		t.Fatalf("prose chat: %v", err)
	}
	if _, _, err := c.Chat(ctx, "Respond with ONLY a JSON array.", "u2", 50); err != nil {
		t.Fatalf("json chat: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(*bodies))
	}
	if _, ok := (*bodies)[0]["response_format"]; ok {
		t.Error("prose prompt sent response_format")
	}
	rf, ok := (*bodies)[1]["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("json prompt response_format = %v, want json_object", rf)
	}
}

// G3-B10: blank content is surfaced as ErrEmptyResponse, and it must not
// count toward the circuit breaker (it is not a transport failure).
func TestChat_EmptyContentReturnsErrEmptyResponse(t *testing.T) {
	srv, _ := captureServer(t, "   ", true)
	c := reviewClient(srv.URL, false)
	for i := 0; i < 3; i++ {
		_, tokens, err := c.Chat(context.Background(), "s", "u"+strings.Repeat("x", i), 50)
		if !errors.Is(err, ErrEmptyResponse) {
			t.Fatalf("call %d err = %v, want ErrEmptyResponse", i, err)
		}
		if tokens != 100 {
			t.Errorf("call %d tokens = %d, want 100 (spend is still counted)", i, tokens)
		}
	}
	if c.IsCircuitOpen() {
		t.Error("empty responses opened the circuit breaker")
	}
	if got := c.TokensUsedToday(); got != 300 {
		t.Errorf("TokensUsedToday = %d, want 300", got)
	}
}

type allocationBudget struct {
	mu         sync.Mutex
	allocation int
	used       int
}

func (b *allocationBudget) CanSpend(tokens int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used+tokens <= b.allocation
}

func (b *allocationBudget) Spend(tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used += tokens
}

// G3-B19: a per-database allocation smaller than max_tokens must still
// admit a call; usage is reconciled to the provider-reported total.
func TestChat_ExternalBudgetSmallerThanMaxTokensAdmitsCall(t *testing.T) {
	srv, _ := captureServer(t, "[]", true)
	c := reviewClient(srv.URL, false)
	budget := &allocationBudget{allocation: 10000}
	c.SetBudget(budget)
	if _, _, err := c.Chat(context.Background(), "s", "u", 20000); err != nil {
		t.Fatalf("Chat with 10k allocation and 20k max_tokens: %v", err)
	}
	if budget.used != 100 {
		t.Errorf("external used = %d, want 100 (reconciled to actual)", budget.used)
	}
}

// G3-B19 boundary: an allocation that cannot even hold the prompt must
// still reject the call before any provider I/O.
func TestChat_ExternalBudgetTooSmallForPromptRejects(t *testing.T) {
	srv, bodies := captureServer(t, "[]", true)
	c := reviewClient(srv.URL, false)
	c.SetBudget(&allocationBudget{allocation: 10})
	_, _, err := c.Chat(context.Background(), "s", strings.Repeat("u", 400), 20000)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("err = %v, want per-database budget exhausted", err)
	}
	if len(*bodies) != 0 {
		t.Errorf("provider was called %d times, want 0", len(*bodies))
	}
}

// G3-B25: a provider that omits usage must not make calls free.
func TestChat_MissingUsageEstimatesTokens(t *testing.T) {
	srv, _ := captureServer(t, strings.Repeat("a", 400), false)
	c := reviewClient(srv.URL, false)
	c.cfg.TokenBudgetDaily = 1000000
	if _, _, err := c.Chat(context.Background(), "s", strings.Repeat("b", 400), 50); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := c.TokensUsedToday(); got < 200 {
		t.Errorf("TokensUsedToday = %d, want >= 200 (chars/4 estimate)", got)
	}
}

// G3-B25: the budget day is the UTC calendar day, not local YearDay.
func TestBudgetDay_IsUTCAndYearAware(t *testing.T) {
	plus5 := time.FixedZone("plus5", 5*3600)
	local := time.Date(2026, 1, 1, 1, 0, 0, 0, plus5) // 2025-12-31T20:00Z
	utc := time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC)
	if budgetDay(local) != budgetDay(utc) {
		t.Errorf("budgetDay differs for the same instant: %d vs %d",
			budgetDay(local), budgetDay(utc))
	}
	nextYear := time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC)
	if budgetDay(utc) == budgetDay(nextYear) {
		t.Error("budgetDay collides across years")
	}
}

// G3-B25: reasoning models are detected beyond gemini/o-series.
func TestIsThinkingModel_Reasoners(t *testing.T) {
	for _, m := range []string{"deepseek-r1", "deepseek-reasoner", "qwq-32b", "o4-mini"} {
		if !isThinkingModel(m) {
			t.Errorf("isThinkingModel(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"gpt-4o", "gpt-4o-mini", "llama-3.1-70b"} {
		if isThinkingModel(m) {
			t.Errorf("isThinkingModel(%q) = true, want false", m)
		}
	}
}

// G3-B22: expired per-prompt throttle entries are evicted.
func TestThrottle_EvictsExpiredEntries(t *testing.T) {
	c := New(&config.LLMConfig{}, noopLog)
	old := time.Now().Add(-time.Hour)
	for i := 0; i < 50; i++ {
		c.lastCalls[strings.Repeat("k", i+1)] = old
	}
	if err := c.acquireThrottle("fresh", 60); err != nil {
		t.Fatalf("acquireThrottle: %v", err)
	}
	c.releaseThrottle("fresh", true)
	if len(c.lastCalls) != 1 {
		t.Errorf("lastCalls len = %d, want 1 (expired entries evicted)", len(c.lastCalls))
	}
}
