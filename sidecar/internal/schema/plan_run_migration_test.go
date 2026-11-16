package schema

import (
	"strings"
	"testing"
)

// plan-02a: the engine's run columns are one T5 step after ddlPlanEvents,
// added only with ADD COLUMN IF NOT EXISTS under lock_timeout; nothing is
// dropped and no ledger table is touched. No concurrent access test: the
// step is a constant string (the bootstrap lock serializes it).

func TestPlanStepRunStep_IsTheLastT5StepAfterPlanEvents(t *testing.T) {
	steps := trancheFiveMigrations()
	last := steps[len(steps)-1]
	if last[0] != "ddlPlanStepRun" || last[1] != ddlPlanStepRun ||
		steps[len(steps)-2][0] != "ddlPlanEvents" {
		t.Fatalf("tranche five ends %v, want ddlPlanEvents then ddlPlanStepRun", steps)
	}
}

func TestPlanStepRunStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS claimed_by text",
		"ADD COLUMN IF NOT EXISTS claimed_until timestamptz",
		"ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 0",
		"ADD COLUMN IF NOT EXISTS executed_at timestamptz",
		"ADD COLUMN IF NOT EXISTS pause_reason text NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS plan_runnable"} {
		if !strings.Contains(ddlPlanStepRun, want) {
			t.Errorf("ddlPlanStepRun lacks %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE FROM", "sage.action_queue",
		"sage.action_log", "sage.plan_event", "SET DATA TYPE", "RENAME"} {
		if strings.Contains(ddlPlanStepRun, bad) {
			t.Errorf("ddlPlanStepRun runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanStepRun) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
