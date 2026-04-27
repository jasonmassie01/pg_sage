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
	if len(report.Proof) != 2 {
		t.Fatalf("Proof len = %d, want 2", len(report.Proof))
	}
	if report.Proof[0].CaseID != "case-1" {
		t.Fatalf("Proof[0].CaseID = %q, want case-1",
			report.Proof[0].CaseID)
	}
	if report.Proof[0].EstimatedToilMins != 15 {
		t.Fatalf("Proof[0].EstimatedToilMins = %d, want 15",
			report.Proof[0].EstimatedToilMins)
	}
	if report.Proof[1].BlockedReason != "requires approval" {
		t.Fatalf("Proof[1].BlockedReason = %q, want requires approval",
			report.Proof[1].BlockedReason)
	}
}
