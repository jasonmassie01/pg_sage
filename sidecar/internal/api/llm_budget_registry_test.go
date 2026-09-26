package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// fakeLLMBudgets stands in for the sidecar's client registry, which knows
// the general, optimizer and per-database clients.
type fakeLLMBudgets struct {
	status map[string]llm.ClientStatus
	resets int
}

func (f *fakeLLMBudgets) TokenStatus() map[string]llm.ClientStatus {
	return f.status
}

func (f *fakeLLMBudgets) ResetBudgets() {
	f.resets++
	for name, s := range f.status {
		s.TokensUsed, s.Exhausted = 0, false
		f.status[name] = s
	}
}

func budgetRouter(
	budgets LLMBudgetRegistry, mgr *llm.Manager,
) http.Handler {
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, testAdminUser()))
		})
	}
	return NewRouterFullRuntime(nil, config.DefaultConfig(), nil, nil, nil,
		mgr, &RuntimeDeps{LLMBudgets: budgets}, inject)
}

type llmStatusBody struct {
	Clients      map[string]llm.ClientStatus `json:"clients"`
	AnyExhausted bool                        `json:"any_exhausted"`
	Reset        bool                        `json:"reset"`
}

func decodeLLMStatus(t *testing.T, w *httptest.ResponseRecorder) llmStatusBody {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var body llmStatusBody
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// G3-B14: the status endpoint used to see only the shared general client,
// so an exhausted per-database or optimizer client was invisible.
func TestLLMStatusCoversEveryRegisteredClient(t *testing.T) {
	budgets := &fakeLLMBudgets{status: map[string]llm.ClientStatus{
		"general":          {TokensUsed: 10, TokenBudget: 100},
		"orders/general":   {TokensUsed: 500, TokenBudget: 500, Exhausted: true},
		"orders/optimizer": {TokensUsed: 3, TokenBudget: 500},
	}}
	shared := llm.NewManager(llm.New(&config.LLMConfig{}, nil), nil, false)
	handler := budgetRouter(budgets, shared)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/llm/status", nil))
	body := decodeLLMStatus(t, w)

	if len(body.Clients) != 3 {
		t.Fatalf("clients = %v, want the 3 registered clients", body.Clients)
	}
	if got := body.Clients["orders/general"].TokensUsed; got != 500 {
		t.Fatalf("orders/general tokens = %d, want 500", got)
	}
	if !body.AnyExhausted {
		t.Fatal("any_exhausted = false while a per-database client is exhausted")
	}
}

// G3-B14: reset must reach every client, not just the shared manager.
func TestLLMBudgetResetReachesEveryRegisteredClient(t *testing.T) {
	budgets := &fakeLLMBudgets{status: map[string]llm.ClientStatus{
		"orders/general": {TokensUsed: 500, TokenBudget: 500, Exhausted: true},
	}}
	handler := budgetRouter(budgets, nil)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/api/v1/llm/budget/reset", nil))
	body := decodeLLMStatus(t, w)

	if budgets.resets != 1 {
		t.Fatalf("registry resets = %d, want 1", budgets.resets)
	}
	if !body.Reset || body.Clients["orders/general"].TokensUsed != 0 {
		t.Fatalf("reset response = %+v, want reset clients", body)
	}
}

// Without a registry (embedders, tests) the shared manager still answers,
// and a missing LLM stays a valid empty state rather than an error.
func TestLLMStatusFallsBackToManagerAndEmptyState(t *testing.T) {
	general := llm.New(&config.LLMConfig{TokenBudgetDaily: 42}, nil)
	w := httptest.NewRecorder()
	budgetRouter(nil, llm.NewManager(general, nil, false)).ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/api/v1/llm/status", nil))
	body := decodeLLMStatus(t, w)
	if got := body.Clients["general"].TokenBudget; got != 42 {
		t.Fatalf("general budget = %d, want 42", got)
	}

	empty := httptest.NewRecorder()
	budgetRouter(nil, nil).ServeHTTP(
		empty, httptest.NewRequest(http.MethodGet, "/api/v1/llm/status", nil))
	if empty.Code != http.StatusOK {
		t.Fatalf("no-LLM status = %d, want 200", empty.Code)
	}
	reset := httptest.NewRecorder()
	budgetRouter(nil, nil).ServeHTTP(reset,
		httptest.NewRequest(http.MethodPost, "/api/v1/llm/budget/reset", nil))
	if reset.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-LLM reset = %d, want 503", reset.Code)
	}
}
