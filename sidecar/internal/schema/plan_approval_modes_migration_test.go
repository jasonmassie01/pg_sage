package schema

import (
	"strings"
	"testing"
)

// plan-03a: ddlPlanApprovalModes adds the approval mode and channel, the
// per-step approval table and the resume marker. No concurrent access
// test: the step is a constant string (the bootstrap lock serializes it;
// the DB test boots twice).

// evid-02c's ddlWorkloadPseudonymKey was merged after this step, so the
// pin is its place (right after ddlPlanOperatorSteps), not "last".
func TestPlanApprovalModesStep_FollowsPlanOperatorSteps(t *testing.T) {
	steps := trancheFiveMigrations()
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlPlanApprovalModes" {
			idx = i
		}
	}
	if idx < 1 || steps[idx][1] != ddlPlanApprovalModes ||
		steps[idx-1][0] != "ddlPlanOperatorSteps" {
		t.Fatalf("tranche five is %v, want ddlPlanOperatorSteps then ddlPlanApprovalModes",
			steps)
	}
}

func TestPlanApprovalModesStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'all'",
		"'all', 'up_to', 'step'", "'ui', 'api', 'chat', 'cli'",
		"ADD COLUMN IF NOT EXISTS up_to_ordinal integer",
		"conname = 'plan_approval_up_to_v1'",
		"ADD COLUMN IF NOT EXISTS resumed_at timestamptz",
		"CREATE TABLE IF NOT EXISTS sage.plan_step_approval (",
		"REFERENCES sage.plan_step (plan_id, id)", "ON DELETE CASCADE",
		"CREATE UNIQUE INDEX IF NOT EXISTS plan_step_approval_live",
		"WHERE voided_at IS NULL"} {
		if !strings.Contains(ddlPlanApprovalModes, want) {
			t.Errorf("ddlPlanApprovalModes lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "UPDATE sage.", "DROP ",
		"sage.action_queue", "sage.action_log", "sage.plan_event", "SET DATA TYPE",
		"RENAME"} {
		if strings.Contains(ddlPlanApprovalModes, bad) {
			t.Errorf("ddlPlanApprovalModes runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanApprovalModes) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
