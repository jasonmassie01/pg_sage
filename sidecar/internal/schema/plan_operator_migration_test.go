package schema

import (
	"strings"
	"testing"
)

// plan-02d: ddlPlanOperatorSteps adds an operator step's command, revert,
// landing check, managed change link and landing time to sage.plan_step.
// No concurrent access test: the step is a constant string (the bootstrap
// lock serializes it).

func TestPlanOperatorStepsStep_IsTheLastT5StepAfterPlanStepApproval(t *testing.T) {
	steps := trancheFiveMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlPlanOperatorSteps" || last[1] != ddlPlanOperatorSteps ||
		steps[len(steps)-2][0] != "ddlPlanStepApproval" {
		t.Fatalf("tranche five ends %v, want ddlPlanStepApproval then ddlPlanOperatorSteps",
			steps)
	}
}

func TestPlanOperatorStepsStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS operator_command text NOT NULL DEFAULT ''",
		"ADD COLUMN IF NOT EXISTS operator_revert text NOT NULL DEFAULT ''",
		"ADD COLUMN IF NOT EXISTS landing jsonb",
		"ADD COLUMN IF NOT EXISTS managed_change_id bigint",
		"ADD COLUMN IF NOT EXISTS landed_at timestamptz"} {
		if !strings.Contains(ddlPlanOperatorSteps, want) {
			t.Errorf("ddlPlanOperatorSteps lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "UPDATE ", "sage.action_queue",
		"sage.action_log", "sage.plan_event", "SET DATA TYPE", "RENAME", "DROP"} {
		if strings.Contains(ddlPlanOperatorSteps, bad) {
			t.Errorf("ddlPlanOperatorSteps runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanOperatorSteps) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
