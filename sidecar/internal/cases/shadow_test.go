package cases

import "testing"

func TestShadowReportCountsAutoSafeCandidates(t *testing.T) {
	report := BuildShadowReport([]Case{
		NewCase(CaseInput{
			IdentityKey:  "case-1",
			DatabaseName: "prod",
			Title:        "stale stats",
			Severity:     SeverityWarning,
			Evidence:     []Evidence{{Type: "finding", Summary: "stale"}},
			ActionCandidates: []ActionCandidate{{
				ActionType: "analyze_table",
				RiskTier:   "safe",
			}},
		}),
		NewCase(CaseInput{
			IdentityKey:  "case-2",
			DatabaseName: "prod",
			Title:        "needs DDL",
			Severity:     SeverityWarning,
			Evidence:     []Evidence{{Type: "finding", Summary: "ddl"}},
			ActionCandidates: []ActionCandidate{{
				ActionType:    "ddl_preflight",
				RiskTier:      "high",
				BlockedReason: "requires approval",
			}},
		}),
	})

	if report.TotalCases != 2 {
		t.Fatalf("TotalCases = %d", report.TotalCases)
	}
	if report.WouldAutoResolve != 1 {
		t.Fatalf("WouldAutoResolve = %d", report.WouldAutoResolve)
	}
	if report.RequiresApproval != 1 {
		t.Fatalf("RequiresApproval = %d", report.RequiresApproval)
	}
}
