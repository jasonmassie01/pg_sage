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

// plan-02d's and plan-03a's steps follow it, so this pins its place
// right after ddlPlanStepRun rather than at the end.
func TestPlanStepApprovalStep_FollowsPlanStepRun(t *testing.T) {
	steps := trancheFiveMigrations()
	for i, s := range steps {
		if s[0] == "ddlPlanStepApproval" {
			if s[1] != ddlPlanStepApproval || i == 0 || steps[i-1][0] != "ddlPlanStepRun" {
				t.Fatalf("tranche five %v, want ddlPlanStepRun then ddlPlanStepApproval", steps)
			}
			return
		}
	}
	t.Fatalf("tranche five %v has no ddlPlanStepApproval", steps)
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
