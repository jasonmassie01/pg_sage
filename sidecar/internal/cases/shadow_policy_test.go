package cases

import (
	"testing"
	"time"
)

// SURF-13: a safe, unblocked candidate whose attached policy decision
// is not "execute", or whose evidence has expired, must not be counted
// as auto-resolved.
func TestShadowReportHonoursPolicyDecisionAndExpiry(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	candidate := func(decision string, expires *time.Time) ActionCandidate {
		c := ActionCandidate{
			ActionType: "analyze_table", RiskTier: "safe", ExpiresAt: expires,
		}
		if decision != "" {
			c.PolicyDecision = &ActionPolicyDecision{Decision: decision}
		}
		return c
	}
	mk := func(key string, c ActionCandidate) Case {
		return NewCase(CaseInput{
			IdentityKey: key, DatabaseName: "prod", Title: key,
			Severity: SeverityWarning,
			Evidence: []Evidence{{Type: "finding", Summary: key}},
			ActionCandidates: []ActionCandidate{c},
		})
	}
	report := BuildShadowReport([]Case{
		mk("queued", candidate("queue_for_approval", &future)),
		mk("observe", candidate("observe_only", nil)),
		mk("expired", candidate("execute", &past)),
		mk("eligible", candidate("execute", &future)),
	})
	if report.WouldAutoResolve != 1 {
		t.Fatalf("WouldAutoResolve = %d, want 1", report.WouldAutoResolve)
	}
	if report.RequiresApproval != 3 {
		t.Fatalf("RequiresApproval = %d, want 3", report.RequiresApproval)
	}
	if report.EstimatedToilMins != 15 {
		t.Fatalf("EstimatedToilMins = %d, want 15", report.EstimatedToilMins)
	}
	for _, p := range report.Proof {
		if p.CaseID == "" {
			t.Fatalf("proof without case id: %+v", p)
		}
	}
}
