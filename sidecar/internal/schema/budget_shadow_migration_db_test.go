package schema

import (
	"context"
	"testing"
)

// market-08: sage.budget_shadow (the warn-mode ledger) and
// sage.workload_role_daily (per-role statement time for proposals). No
// concurrent-writer test: Bootstrap holds its advisory lock, so one
// migration runs per database at a time.

func TestBudgetShadowMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "budgetshadow")
	ctx := context.Background()
	for run := range 2 { // the second run is a no-op
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
		if _, err := pool.Exec(ctx, ddlBudgetShadow); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow
		(database_name, budget, day, would_cancel, would_reject, protected_cancels, worst_ms)
		VALUES ('db', 'app', current_date, 1, 0, 0, 7000)`); err != nil {
		t.Fatalf("insert shadow row: %v", err)
	}
	var cancels int64
	var firstSeen bool
	if err := pool.QueryRow(ctx, `SELECT would_cancel, first_seen <= now()
		FROM sage.budget_shadow WHERE budget = 'app'`).Scan(&cancels, &firstSeen); err != nil {
		t.Fatalf("read shadow row: %v", err)
	}
	if cancels != 1 || !firstSeen {
		t.Fatalf("shadow row = %d / first_seen ok %v", cancels, firstSeen)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow (database_name, budget, day)
		VALUES ('db', 'app', current_date)`)
	if err == nil {
		t.Fatal("a second row for one (database, budget, day) was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.budget_shadow (database_name, budget, day,
		would_cancel) VALUES ('db', 'neg', current_date, -1)`); err == nil {
		t.Fatal("a negative would_cancel was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.workload_role_daily
		(database_name, role_name, day, p99_estimate_ms, max_exec_ms, calls)
		VALUES ('db', 'app', current_date, 2000, 2500, 10)`); err != nil {
		t.Fatalf("insert role row: %v", err)
	}
}
