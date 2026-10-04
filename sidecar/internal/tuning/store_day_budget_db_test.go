package tuning

import (
	"context"
	"testing"
	"time"
)

// The daily budget is durable: a new store (a restarted process) reads what
// the previous one charged, per UTC day.
func TestPostgresStore_DayBudgetIsDurable(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	day := time.Date(2031, 3, 4, 23, 30, 0, 0, time.UTC)
	mustExec(t, pool, "DELETE FROM sage.tuning_budget_day WHERE utc_day IN ($1, $2)",
		day, day.Add(24*time.Hour))
	first := pgStore(t, pool)
	if err := first.ChargeDayBudget(ctx, day, 400, 2); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if err := first.ChargeDayBudget(ctx, day.Add(40*time.Minute), 100, 1); err != nil {
		t.Fatalf("charge after midnight UTC: %v", err)
	}
	restarted := pgStore(t, pool)
	tokens, requests, err := restarted.DayBudgetUsed(ctx, day.Add(-time.Hour))
	if err != nil || tokens != 400 || requests != 2 {
		t.Fatalf("day = %d tokens %d requests %v", tokens, requests, err)
	}
	tokens, _, err = restarted.DayBudgetUsed(ctx, day.Add(24*time.Hour))
	if err != nil || tokens != 100 {
		t.Fatalf("next UTC day = %d %v", tokens, err)
	}
	if err := restarted.ChargeDayBudget(ctx, day, -1, 0); err == nil {
		t.Fatal("a negative charge is refused")
	}
}
