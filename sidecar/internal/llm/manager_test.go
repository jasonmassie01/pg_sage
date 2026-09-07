package llm

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func testClient(name string) *Client {
	return New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://" + name,
		APIKey:           "test-key",
		Model:            name,
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1000,
		CooldownSeconds:  10,
	}, noopLog)
}

func TestManager_GeneralOnly(t *testing.T) {
	gen := testClient("general")
	m := NewManager(gen, nil, false)

	if got := m.ForPurpose("index_optimization"); got != gen {
		t.Fatal("expected General when Optimizer is nil")
	}
}

func TestManager_DualModel(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, true)

	if got := m.ForPurpose("index_optimization"); got != opt {
		t.Fatal("expected Optimizer for index_optimization")
	}
	if got := m.ForPurpose("briefing"); got != gen {
		t.Fatal("expected General for briefing")
	}
}

func TestManager_ForPurpose_Unknown(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, false)

	if got := m.ForPurpose("something_else"); got != gen {
		t.Fatal("expected General for unknown purpose")
	}
}

func TestManager_NilOptimizer(t *testing.T) {
	gen := testClient("general")
	m := NewManager(gen, nil, true)

	for _, purpose := range []string{"index_optimization", "query_tuning", "briefing", ""} {
		if got := m.ForPurpose(purpose); got != gen {
			t.Fatalf("expected General for purpose %q", purpose)
		}
	}
}

func TestManager_QueryTuning_RoutesToOptimizer(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, true)

	if got := m.ForPurpose("query_tuning"); got != opt {
		t.Fatal("expected Optimizer for query_tuning")
	}
}

func TestTokenStatus_GeneralOnly(t *testing.T) {
	gen := testClient("general")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(500)
	m := NewManager(gen, nil, false)

	status := m.TokenStatus()
	if len(status) != 1 {
		t.Fatalf("expected 1 client status, got %d", len(status))
	}
	gs, ok := status["general"]
	if !ok {
		t.Fatal("expected 'general' key in status")
	}
	if gs.TokensUsed != 500 {
		t.Errorf("tokens_used = %d, want 500", gs.TokensUsed)
	}
	if gs.TokenBudget != 1000 {
		t.Errorf("token_budget = %d, want 1000", gs.TokenBudget)
	}
	if gs.Exhausted {
		t.Error("budget_exhausted should be false")
	}
	if gs.Model != "general" {
		t.Errorf("model = %q, want %q", gs.Model, "general")
	}
}

func TestTokenStatus_DualClient(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(200)
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(1500)
	m := NewManager(gen, opt, false)

	status := m.TokenStatus()
	if len(status) != 2 {
		t.Fatalf("expected 2 client statuses, got %d", len(status))
	}

	gs := status["general"]
	if gs.Exhausted {
		t.Error("general should not be exhausted")
	}

	os := status["optimizer"]
	if !os.Exhausted {
		t.Error("optimizer should be exhausted (1500 >= 1000)")
	}
	if os.TokensUsed != 1500 {
		t.Errorf("optimizer tokens_used = %d, want 1500", os.TokensUsed)
	}
}

func TestTokenStatus_ResetTimestamp(t *testing.T) {
	gen := testClient("general")
	m := NewManager(gen, nil, false)

	status := m.TokenStatus()
	gs := status["general"]
	if gs.ResetTimestamp == "" {
		t.Error("resets_at should not be empty")
	}
	// Parse the timestamp to verify format.
	parsed, err := time.Parse(time.RFC3339, gs.ResetTimestamp)
	if err != nil {
		t.Fatalf("invalid resets_at format: %v", err)
	}
	// Should be tomorrow or later.
	now := time.Now()
	if parsed.Before(now) {
		t.Errorf("resets_at %v should be after now %v",
			parsed, now)
	}
}

func TestResetBudgets_DualClient(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(5000)
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(3000)
	m := NewManager(gen, opt, false)

	if !gen.IsBudgetExhausted() || !opt.IsBudgetExhausted() {
		t.Fatal("precondition: both should be exhausted")
	}

	m.ResetBudgets()

	if gen.TokensUsedToday() != 0 {
		t.Errorf("general tokens = %d, want 0", gen.TokensUsedToday())
	}
	if opt.TokensUsedToday() != 0 {
		t.Errorf("optimizer tokens = %d, want 0", opt.TokensUsedToday())
	}
	if gen.IsBudgetExhausted() {
		t.Error("general should not be exhausted after reset")
	}
	if opt.IsBudgetExhausted() {
		t.Error("optimizer should not be exhausted after reset")
	}
}

func TestResetBudgets_NilOptimizer(t *testing.T) {
	gen := testClient("general")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(5000)
	m := NewManager(gen, nil, false)

	m.ResetBudgets() // should not panic

	if gen.TokensUsedToday() != 0 {
		t.Errorf("general tokens = %d, want 0", gen.TokensUsedToday())
	}
}

func TestTokenStatus_ClientDisabled(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          false,
		Endpoint:         "",
		APIKey:           "",
		Model:            "disabled-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1000,
		CooldownSeconds:  10,
	}
	gen := New(cfg, noopLog)
	m := NewManager(gen, nil, false)

	status := m.TokenStatus()
	gs := status["general"]
	if gs.Enabled {
		t.Error("enabled should be false for disabled client")
	}
}
