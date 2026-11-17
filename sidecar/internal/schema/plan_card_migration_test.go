package schema

import (
	"strings"
	"testing"
)

// plan-03b: ddlPlanCardDeliveries records the plan cards sent to chat. No
// concurrent access test: the step is a constant string (the bootstrap
// lock serializes it; the DB test boots twice).

// Other T5 steps merged between them (evid-02c, ux-01b, lab-03a, lab-03b, lab-05a),
// so the pin is that the step follows ddlPlanApprovalModes, not that it
// is last; the final step is pinned in TestMigrationStatements_AreTheStepsInOrder.
func TestPlanCardStep_FollowsPlanApprovalModes(t *testing.T) {
	steps := trancheFiveMigrations()
	modes, card := -1, -1
	for i, step := range steps {
		switch step[0] {
		case "ddlPlanApprovalModes":
			modes = i
		case "ddlPlanCardDeliveries":
			card = i
		}
	}
	if modes < 0 || card <= modes || steps[card][1] != ddlPlanCardDeliveries {
		t.Fatalf("tranche five is %v, want ddlPlanCardDeliveries after "+
			"ddlPlanApprovalModes", steps)
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
