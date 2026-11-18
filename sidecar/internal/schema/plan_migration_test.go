package schema

import (
	"strings"
	"testing"
)

// plan-01a: the plan tables are one T5 step after lab-02's, created only
// with IF NOT EXISTS under lock_timeout; no existing table is touched. No
// concurrent access test: the step is a constant string (the bootstrap
// lock serializes it; see the DB test).

func TestPlansStep_FollowsTheBacktestStep(t *testing.T) {
	steps := trancheFiveMigrations()
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlPlans" && step[1] == ddlPlans {
			idx = i
		}
	}
	if idx < 1 || steps[idx-1][0] != "ddlVerifierBacktest" {
		t.Fatalf("ddlPlans at %d of tranche five, want after ddlVerifierBacktest", idx)
	}
	var naming []string
	for _, s := range migrationSteps() {
		if strings.Contains(s.sql, "sage.plan_step") {
			naming = append(naming, s.name)
		}
	}
	// plan-02a's ddlPlanStepRun adds the engine's run columns; plan-02b's
	// ddlPlanStepApproval widens the state vocabulary; plan-02d's
	// ddlPlanOperatorSteps adds the operator step columns; plan-03a's
	// ddlPlanApprovalModes adds resumed_at and sage.plan_step_approval;
	// review-03b's ddlReviewPlans adds sage.plan_step_origin.
	want := "ddlPlans,ddlPlanStepRun,ddlPlanStepApproval,ddlPlanOperatorSteps," +
		"ddlPlanApprovalModes,ddlReviewPlans"
	if strings.Join(naming, ",") != want {
		t.Fatalf("steps naming sage.plan_step = %v, want %s", naming, want)
	}
}

func TestPlansStep_IsIdempotentBoundedAndTouchesNoChainedTable(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.plan (",
		"CREATE TABLE IF NOT EXISTS sage.plan_step (",
		"CREATE TABLE IF NOT EXISTS sage.plan_step_edge (",
		"CREATE TABLE IF NOT EXISTS sage.plan_approval (",
		"CREATE UNIQUE INDEX IF NOT EXISTS plan_approval_live",
		"'generator', 'model', 'person', 'agent'",
		"'revert_to_checkpoint'", "'awaiting_operator'", "ON DELETE CASCADE",
		"BETWEEN 1 AND 43200", "BETWEEN 1 AND 500"} {
		if !strings.Contains(ddlPlans, want) {
			t.Errorf("ddlPlans lacks %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE FROM", "ALTER TABLE",
		"sage.action_queue", "sage.action_log", "sage.decision"} {
		if strings.Contains(ddlPlans, bad) {
			t.Errorf("ddlPlans runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlans) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
