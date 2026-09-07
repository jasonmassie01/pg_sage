package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

type exhaustedBudget struct{}

func (exhaustedBudget) CanSpend(int) bool { return false }
func (exhaustedBudget) Spend(int)         {}

// TestClient_FleetBudgetBlocksWhenExhausted verifies the per-database
// Budgeter (F5) blocks a call before any HTTP request when the database
// allocation is exhausted. (nil-budget behavior is covered by the rest
// of the suite, which constructs clients without a budget.)
func TestClient_FleetBudgetBlocksWhenExhausted(t *testing.T) {
	c := New(&config.LLMConfig{
		Enabled: true, Endpoint: "http://127.0.0.1:1", APIKey: "x",
		Model: "m", TimeoutSeconds: 1,
	}, func(string, string, ...any) {})
	c.SetBudget(exhaustedBudget{})

	_, _, err := c.Chat(context.Background(), "sys", "user", 100)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected per-database budget error, got %v", err)
	}
}
