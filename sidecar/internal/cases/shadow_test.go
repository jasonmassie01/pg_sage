package cases

import (
	"testing"
	"time"
)

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

func TestShadowReportPrefersDurableActionHistory(t *testing.T) {
	proposedAt := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	report := BuildShadowReport([]Case{
		NewCase(CaseInput{
			IdentityKey:  "case-queued",
			DatabaseName: "prod",
			Title:        "queued analyze",
			Severity:     SeverityWarning,
			ActionCandidates: []ActionCandidate{{
				ActionType: "analyze_table",
				RiskTier:   "safe",
			}},
		}).withActions([]CaseAction{{
			ID:                "queue:7",
			Type:              "analyze_table",
			RiskTier:          "safe",
			Status:            "pending",
			LifecycleState:    "blocked",
			PolicyDecision:    "queue_for_approval",
			ShadowToilMinutes: 15,
			ProposedAt:        &proposedAt,
		}}),
		NewCase(CaseInput{
			IdentityKey:  "case-executed",
			DatabaseName: "prod",
			Title:        "executed analyze",
			Severity:     SeverityWarning,
		}).withActions([]CaseAction{{
			ID:                 "log:88",
			Type:               "analyze",
			RiskTier:           "safe",
			Status:             "success",
			LifecycleState:     "executed",
			VerificationStatus: "verified",
		}}),
	})

	if report.TotalCases != 2 {
		t.Fatalf("TotalCases = %d, want 2", report.TotalCases)
	}
	if report.WouldAutoResolve != 1 {
		t.Fatalf("WouldAutoResolve = %d, want executed action counted",
			report.WouldAutoResolve)
	}
	if report.RequiresApproval != 1 {
		t.Fatalf("RequiresApproval = %d, want queued action counted",
			report.RequiresApproval)
	}
	if len(report.Proof) != 2 {
		t.Fatalf("Proof len = %d, want durable action rows only",
			len(report.Proof))
	}
	if report.Proof[0].CaseID != "case-queued" ||
		report.Proof[0].PolicyDecision != "queue_for_approval" {
		t.Fatalf("queued proof = %#v", report.Proof[0])
	}
	if report.Proof[0].ActionID != "queue:7" ||
		report.Proof[0].LifecycleState != "blocked" ||
		report.Proof[0].ProposedAt != "2026-04-27T12:00:00Z" {
		t.Fatalf("queued proof provenance = %#v", report.Proof[0])
	}
	if report.Proof[1].CaseID != "case-executed" ||
		report.Proof[1].EstimatedToilMins != 15 {
		t.Fatalf("executed proof = %#v", report.Proof[1])
	}
}

func (c Case) withActions(actions []CaseAction) Case {
	c.Actions = actions
	return c
}
