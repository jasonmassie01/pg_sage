package schema

import (
	"context"
	"testing"
)

// plan-02b on a real server: bootstrap three times is clean, records
// ddlPlanStepApproval once, leaves one state constraint that accepts
// awaiting_approval and still refuses an unknown state.
func TestPlanStepApprovalMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plan_step_approval") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanStepApproval'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanStepApproval: %d (%v), want 1", ledger, err)
	}
	var names []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(conname ORDER BY conname)
		FROM pg_constraint WHERE conrelid = 'sage.plan_step'::regclass
		AND pg_get_constraintdef(oid) LIKE '%state%awaiting%'`).Scan(&names); err != nil ||
		len(names) != 1 || names[0] != "plan_step_state_v2" {
		t.Fatalf("state constraints %v (%v), want only plan_step_state_v2", names, err)
	}
	var planID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('g', 'person', 'running', repeat('a', 64), 'x')
		RETURNING id`).Scan(&planID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	insert := func(state string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind, metric,
			expected_direction, expected_source, deadline_minutes, on_improved, on_no_change,
			on_regressed, on_inconclusive, on_expired, state)
			VALUES ($1, (SELECT count(*) + 1 FROM sage.plan_step WHERE plan_id = $1),
			        'check', 'readback', 'none', 'none', 5, 'keep', 'keep', 'revert_step',
			        'halt_ask', 'halt_ask', $2)`, planID, state)
		return err
	}
	for _, state := range []string{"awaiting_approval", "awaiting_operator", "pending"} {
		if err := insert(state); err != nil {
			t.Fatalf("insert a %s step: %v", state, err)
		}
	}
	if err := insert("awaiting_lunch"); err == nil {
		t.Fatal("an unknown step state was stored")
	}
	var since *string
	if err := pool.QueryRow(ctx, `SELECT waiting_since::text FROM sage.plan_step
		WHERE plan_id = $1 LIMIT 1`, planID).Scan(&since); err != nil || since != nil {
		t.Fatalf("waiting_since %v (%v), want NULL", since, err)
	}
}
