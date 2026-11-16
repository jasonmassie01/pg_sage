package schema

import (
	"context"
	"testing"
)

// plan-02d on a real server: bootstrap three times is clean, records
// ddlPlanOperatorSteps once, gives a stored step empty operator fields and
// refuses a landing that is not an object and an over-long command.
func TestPlanOperatorStepsMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plan_operator_steps") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanOperatorSteps'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanOperatorSteps: %d (%v), want 1", ledger, err)
	}
	var planID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('g', 'person', 'running', repeat('a', 64), 'x')
		RETURNING id`).Scan(&planID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	insert := func(ordinal int, landing, command string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind, metric,
			expected_direction, expected_source, deadline_minutes, on_improved, on_no_change,
			on_regressed, on_inconclusive, on_expired, landing, operator_command)
			VALUES ($1, $2, 'operator', 'readback', 'none', 'none', 5, 'keep', 'keep',
			        'halt_ask', 'halt_ask', 'halt_ask', $3::jsonb, $4)`, planID, ordinal,
			landing, command)
		return err
	}
	if err := insert(1, `{"setting": "work_mem", "value": "64MB"}`, "run it"); err != nil {
		t.Fatalf("insert an operator step: %v", err)
	}
	var command, revert string
	var landed, managed *string
	if err := pool.QueryRow(ctx, `SELECT operator_command, operator_revert,
		landed_at::text, managed_change_id::text FROM sage.plan_step WHERE plan_id = $1`,
		planID).Scan(&command, &revert, &landed, &managed); err != nil ||
		command != "run it" || revert != "" || landed != nil || managed != nil {
		t.Fatalf("stored step %q %q %v %v (%v), want the command and empty rest", command,
			revert, landed, managed, err)
	}
	if err := insert(2, `["work_mem"]`, "run it"); err == nil {
		t.Fatal("a landing that is not an object was stored")
	}
	long := make([]byte, 10001)
	for i := range long {
		long[i] = 'x'
	}
	if err := insert(3, `{}`, string(long)); err == nil {
		t.Fatal("an over-long operator command was stored")
	}
}
