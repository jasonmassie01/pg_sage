package schema

import (
	"context"
	"testing"
)

// market-08b: sage.budget_shadow gains the real statement-timeout cancels
// counted from the server log (actual_cancels), where they came from
// (cancel_source) and whether the count is a lower bound. No concurrent
// test: Bootstrap holds its advisory lock, so one migration runs per
// database at a time.

func TestBudgetShadowCancelsMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "budgetcancels")
	ctx := context.Background()
	for run := range 2 { // the second run is a no-op
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
		if _, err := pool.Exec(ctx, ddlBudgetShadowActualCancels); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	// A row written as market-08 wrote it reads the defaults.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow
		(database_name, budget, day, would_cancel)
		VALUES ('db', 'app', current_date, 2)`); err != nil {
		t.Fatalf("insert shadow row: %v", err)
	}
	var actual int64
	var source string
	var lower bool
	if err := pool.QueryRow(ctx, `SELECT actual_cancels, cancel_source, cancels_lower_bound
		FROM sage.budget_shadow WHERE budget = 'app'`).Scan(&actual, &source, &lower); err != nil {
		t.Fatalf("read shadow row: %v", err)
	}
	if actual != 0 || source != "none" || lower {
		t.Fatalf("defaults = %d / %q / %v, want 0 / none / false", actual, source, lower)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.budget_shadow SET actual_cancels = 30,
		cancel_source = 'log' WHERE budget = 'app'`); err != nil {
		t.Fatalf("record cancels from the log: %v", err)
	}
}

// Invalid input: a negative count and an unknown source are refused.
func TestBudgetShadowCancelsMigration_ChecksRefuseBadValues(t *testing.T) {
	pool := freshSchemaPool(t, "budgetcancelscheck")
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow (database_name, budget, day,
		actual_cancels) VALUES ('db', 'neg', current_date, -1)`); err == nil {
		t.Fatal("a negative actual_cancels was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow (database_name, budget, day,
		cancel_source) VALUES ('db', 'src', current_date, 'pg_stat_activity')`); err == nil {
		t.Fatal("an unknown cancel_source was accepted")
	}
}
