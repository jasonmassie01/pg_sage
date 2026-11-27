package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// plan-03a on a real server: bootstrap three times is clean and records
// ddlPlanApprovalModes once; an old-style approval reads as mode all; the
// mode checks refuse an unknown mode, up_to without an ordinal and an
// unknown channel; a step approval cannot name another plan's step and at
// most one is live per step.
func TestPlanApprovalModesMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plan_approval_modes") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanApprovalModes'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanApprovalModes: %d (%v), want 1", ledger, err)
	}
	var cons int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conname = 'plan_approval_up_to_v1'`).Scan(&cons); err != nil || cons != 1 {
		t.Fatalf("plan_approval_up_to_v1 constraints: %d (%v), want 1", cons, err)
	}
	planA, stepA := modesPlan(t, ctx, pool, "a")
	planB, stepB := modesPlan(t, ctx, pool, "b")
	var mode string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan_approval (plan_id, plan_hash,
		approver_user_id) VALUES ($1, repeat('a', 64), 7) RETURNING mode`,
		planA).Scan(&mode); err != nil || mode != "all" {
		t.Fatalf("an approval without a mode reads %q (%v), want all", mode, err)
	}
	refused := map[string]string{
		"unknown mode": `INSERT INTO sage.plan_approval (plan_id, plan_hash,
			approver_user_id, mode) VALUES ($1, repeat('b', 64), 7, 'most')`,
		"up_to without ordinal": `INSERT INTO sage.plan_approval (plan_id, plan_hash,
			approver_user_id, mode) VALUES ($1, repeat('b', 64), 7, 'up_to')`,
		"ordinal without up_to": `INSERT INTO sage.plan_approval (plan_id, plan_hash,
			approver_user_id, mode, up_to_ordinal) VALUES ($1, repeat('b', 64), 7, 'all', 2)`,
		"unknown channel": `INSERT INTO sage.plan_approval (plan_id, plan_hash,
			approver_user_id, via) VALUES ($1, repeat('b', 64), 7, 'fax')`,
	}
	for name, sql := range refused {
		if _, err := pool.Exec(ctx, sql, planB); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	checkStepApprovals(t, ctx, pool, planA, stepA, stepB)
}

// checkStepApprovals: at most one live approval per step, never of
// another plan's step; a new step's resumed_at is NULL.
func checkStepApprovals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, planA,
	stepA, stepB int64) {
	t.Helper()
	stepApproval := `INSERT INTO sage.plan_step_approval (plan_id, step_id, plan_hash,
		approver_user_id, via) VALUES ($1, $2, repeat('a', 64), 7, 'cli')`
	if _, err := pool.Exec(ctx, stepApproval, planA, stepA); err != nil {
		t.Fatalf("step approval: %v", err)
	}
	if _, err := pool.Exec(ctx, stepApproval, planA, stepA); err == nil ||
		!strings.Contains(err.Error(), "plan_step_approval_live") {
		t.Fatalf("a second live approval of one step: %v, want plan_step_approval_live", err)
	}
	if _, err := pool.Exec(ctx, stepApproval, planA, stepB); err == nil {
		t.Fatal("a step approval named another plan's step")
	}
	var resumed *string
	if err := pool.QueryRow(ctx, `SELECT resumed_at::text FROM sage.plan_step
		WHERE id = $1`, stepA).Scan(&resumed); err != nil || resumed != nil {
		t.Fatalf("resumed_at %v (%v), want NULL", resumed, err)
	}
}

// modesPlan stores a plan with one check step and returns both ids.
func modesPlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	label string) (int64, int64) {
	t.Helper()
	var planID, stepID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ($1, 'person', 'proposed', repeat('a', 64), 'x')
		RETURNING id`, "goal "+label).Scan(&planID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind,
		metric, expected_direction, expected_source, deadline_minutes, on_improved,
		on_no_change, on_regressed, on_inconclusive, on_expired)
		VALUES ($1, 1, 'check', 'readback', 'none', 'none', 5, 'keep', 'keep',
		        'revert_step', 'halt_ask', 'halt_ask') RETURNING id`,
		planID).Scan(&stepID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	return planID, stepID
}
