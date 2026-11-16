package schema

import (
	"strings"
	"testing"
)

// plan-02b: ddlPlanStepApproval widens sage.plan_step.state with
// awaiting_approval (a step approve-all never covers) and adds
// waiting_since (when a step first waited on a plan cap or a peak window).
// No concurrent access test: the step is a constant string (the bootstrap
// lock serializes it).

func TestPlanStepApprovalStep_IsTheLastT5StepAfterPlanStepRun(t *testing.T) {
	steps := trancheFiveMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlPlanStepApproval" || last[1] != ddlPlanStepApproval ||
		steps[len(steps)-2][0] != "ddlPlanStepRun" {
		t.Fatalf("tranche five ends %v, want ddlPlanStepRun then ddlPlanStepApproval", steps)
	}
}

func TestPlanStepApprovalStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS waiting_since timestamptz",
		"conname = 'plan_step_state_v2'", "DROP CONSTRAINT IF EXISTS plan_step_state_check",
		"'awaiting_approval'", "'awaiting_operator'", "'pending'", "'halted'"} {
		if !strings.Contains(ddlPlanStepApproval, want) {
			t.Errorf("ddlPlanStepApproval lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "sage.action_queue",
		"sage.action_log", "sage.plan_event", "SET DATA TYPE", "RENAME", "DROP TABLE",
		"DROP COLUMN"} {
		if strings.Contains(ddlPlanStepApproval, bad) {
			t.Errorf("ddlPlanStepApproval runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanStepApproval) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
