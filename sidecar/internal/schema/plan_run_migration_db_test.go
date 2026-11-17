package schema

import (
	"context"
	"testing"
)

// plan-02a on a real server: bootstrap twice (and a third time) is clean,
// records ddlPlanStepRun once and gives a stored step its run columns with
// their defaults.
func TestPlanStepRunMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plan_step_run") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanStepRun'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanStepRun: %d (%v), want 1", ledger, err)
	}
	var planID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('g', 'person', 'proposed', repeat('a', 64), 'x')
		RETURNING id`).Scan(&planID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	var pause string
	if err := pool.QueryRow(ctx, `SELECT pause_reason FROM sage.plan WHERE id = $1`,
		planID).Scan(&pause); err != nil || pause != "" {
		t.Fatalf("pause_reason %q (%v), want empty", pause, err)
	}
	var attempt int
	var claimedBy *string
	err := pool.QueryRow(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind, metric,
		expected_direction, expected_source, deadline_minutes, on_improved, on_no_change,
		on_regressed, on_inconclusive, on_expired)
		VALUES ($1, 1, 'check', 'readback', 'none', 'none', 5, 'keep', 'keep',
		        'revert_step', 'halt_ask', 'halt_ask')
		RETURNING attempt, claimed_by`, planID).Scan(&attempt, &claimedBy)
	if err != nil || attempt != 0 || claimedBy != nil {
		t.Fatalf("new step attempt %d claimed_by %v (%v), want 0 and NULL", attempt,
			claimedBy, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.plan_step SET attempt = -1
		WHERE plan_id = $1`, planID); err == nil {
		t.Fatal("a negative attempt was stored")
	}
}
