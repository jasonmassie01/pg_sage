package schema

import (
	"strings"
	"testing"
)

// plan-01b: the plan event and model record tables are one T5 step after
// ddlPlans, created only with IF NOT EXISTS under lock_timeout; no existing
// table is touched. No concurrent access test: the step is a constant
// string (the bootstrap lock serializes it; see the DB test).

func TestPlanEventsStep_FollowsThePlansStep(t *testing.T) {
	steps := trancheFiveMigrations()
	for i := 1; i < len(steps); i++ {
		if steps[i][0] == "ddlPlanEvents" {
			if steps[i][1] != ddlPlanEvents || steps[i-1][0] != "ddlPlans" {
				t.Fatalf("tranche five %v: want ddlPlanEvents right after ddlPlans", steps)
			}
			return
		}
	}
	t.Fatalf("tranche five %v lacks ddlPlanEvents", steps)
}

func TestPlanEventsStep_IsIdempotentBoundedAndAppendOnly(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.plan_event (",
		"CREATE TABLE IF NOT EXISTS sage.plan_model_record (",
		"'created', 'proposed', 'edited', 'approved', 'voided', 'rejected'",
		"'step_started', 'step_executed', 'verdict', 'step_reverted', 'halted'",
		"'resumed', 'completed', 'cancelled'", "octet_length(raw_output) <= 1048576",
		"plan_audit_append_only", "BEFORE UPDATE"} {
		if !strings.Contains(ddlPlanEvents, want) {
			t.Errorf("ddlPlanEvents lacks %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE FROM", "ALTER TABLE",
		"sage.action_queue", "sage.action_log", "sage.decision", "REFERENCES"} {
		if strings.Contains(ddlPlanEvents, bad) {
			t.Errorf("ddlPlanEvents runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanEvents) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
