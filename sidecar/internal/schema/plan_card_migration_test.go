package schema

import (
	"strings"
	"testing"
)

// plan-03b: ddlPlanCardDeliveries records the plan cards sent to chat. No
// concurrent access test: the step is a constant string (the bootstrap
// lock serializes it; the DB test boots twice).

// The T5 chains merged steps after ddlPlanApprovalModes, and ux-02b's
// ddlBriefingsDigestHash follows this one, so the pin is "a T5 step after
// ddlPlanApprovalModes", not "right after it" or "last".
func TestPlanCardStep_IsAT5StepAfterPlanApprovalModes(t *testing.T) {
	steps := trancheFiveMigrations()
	card, modes := -1, -1
	for i, step := range steps {
		switch step[0] {
		case "ddlPlanApprovalModes":
			modes = i
		case "ddlPlanCardDeliveries":
			card = i
		}
	}
	if card < 0 || steps[card][1] != ddlPlanCardDeliveries || modes < 0 || modes >= card {
		t.Fatalf("tranche five is %v, want ddlPlanApprovalModes before "+
			"ddlPlanCardDeliveries", steps)
	}
}

func TestPlanCardStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.plan_card_deliveries (",
		"token_sha256     bytea NOT NULL UNIQUE", "plan_hash ~ '^[0-9a-f]{64}$'",
		"'approve_all', 'approve_up_to'", "'reject' = ANY (offered_modes)",
		"up_to_ordinal    integer CHECK (up_to_ordinal BETWEEN 1 AND 50)",
		"CHECK ((used_at IS NULL) = (decision IS NULL))",
		"CHECK (decision IS NULL OR decision = ANY (offered_modes))",
		"CREATE INDEX IF NOT EXISTS idx_plan_card_created"} {
		if !strings.Contains(ddlPlanCardDeliveries, want) {
			t.Errorf("ddlPlanCardDeliveries lacks %q", want)
		}
	}
	for _, bad := range []string{"TRUNCATE", "DELETE FROM", "UPDATE sage.", "DROP ",
		"ALTER TABLE", "sage.plan_approval", "sage.plan_step", "RENAME"} {
		if strings.Contains(ddlPlanCardDeliveries, bad) {
			t.Errorf("ddlPlanCardDeliveries runs or names %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlPlanCardDeliveries) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
