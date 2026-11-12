package policy

import (
	"strings"
	"testing"
)

// earned-07: a cap that is not a downgrade signal (a self-initiated L3
// granted on another PostgreSQL major, run at L2) hands off to approval
// and says why in the decision; a downgrade still reads as one.
func TestAutonomyNoteNamesANonDowngradeCap(t *testing.T) {
	f := newAutonomyFixture(2)
	f.limiter.limit = AutonomyLimit{Level: 2, Granted: 3,
		Reasons: []string{"server_major_changed:16->17"}}
	d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
	if !strings.Contains(d.Detail, "autonomy L2 (granted L3") ||
		!strings.Contains(d.Detail, "; capped: server_major_changed:16->17") ||
		strings.Contains(d.Detail, "downgraded") {
		t.Fatalf("detail = %q, want the major cap named as a cap", d.Detail)
	}

	f = newAutonomyFixture(2)
	f.limiter.limit = AutonomyLimit{Level: 2, Granted: 3, Downgraded: true,
		Reasons: []string{"error_budget_fast_burn"}}
	d = f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyDowngraded)
	if !strings.Contains(d.Detail, "; downgraded: error_budget_fast_burn") ||
		strings.Contains(d.Detail, "capped:") {
		t.Fatalf("detail = %q, want the downgrade named", d.Detail)
	}

	// Nil/empty: no reasons add nothing.
	f = newAutonomyFixture(2)
	d = f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	if strings.Contains(d.Detail, "capped:") || strings.Contains(d.Detail, "downgraded") {
		t.Fatalf("detail = %q, want no cap or downgrade", d.Detail)
	}
}
