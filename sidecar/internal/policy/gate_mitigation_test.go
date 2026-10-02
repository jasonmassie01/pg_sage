package policy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Sage SRE M5: a human-approved, mitigation-only backend cancel is an
// incident mitigation, not maintenance. Maintenance windows do not bind
// it; every other gate does (hard stops, trust, change classes, replica
// role). Nothing else gains the exemption.

func mitigationGate(t *testing.T, doc Document, runtime RuntimeState) Gate {
	t.Helper()
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return runtime, nil
		},
		ValidateSQL: func(sql string) error {
			if !strings.HasPrefix(sql, "SELECT pg_cancel_backend(") &&
				!strings.HasPrefix(sql, "SELECT pg_terminate_backend(") &&
				!strings.HasPrefix(sql, "VACUUM ") {
				return context.Canceled
			}
			return nil
		},
		Policy: func(context.Context, ActionRequest) (Document, error) { return doc, nil },
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			return LimitUsage{}, nil
		},
		RecordDecision: func(context.Context, ActionRequest, Decision) (string, error) {
			return "decision-1", nil
		},
		// Sunday 02:00 UTC: outside the staffed weekday window.
		Now: func() time.Time { return time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC) },
	})
}

func advisoryRuntime() RuntimeState {
	return RuntimeState{ExecutorEnabled: true, TrustLevel: TrustAdvisory,
		ExecutionMode: ExecutionApproval, WindowConfigured: true, InConfiguredWindow: false}
}

func cancelRequest(class RollbackClass, approved bool) ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "cancel_backend", RiskTier: RiskModerate,
			RollbackClass: class},
		SQL: "SELECT pg_cancel_backend(4242)", TargetObjs: []string{"pid:4242"},
		Feature: string(ChangeBackendSignal), OperatorApproved: approved,
	}
}

func TestApprovedMitigationIgnoresMaintenanceWindows(t *testing.T) {
	gate := mitigationGate(t, StaffedProfile(), advisoryRuntime())
	assertDecision(t, gate.Authorize(context.Background(),
		cancelRequest(RollbackMitigationOnly, true)), VerdictExecute, ReasonOperatorApproved)
}

func TestWindowExemptionIsNarrow(t *testing.T) {
	gate := mitigationGate(t, StaffedProfile(), advisoryRuntime())
	terminate := cancelRequest(RollbackNotReversible, true)
	terminate.Contract.ActionType = "terminate_backend"
	terminate.Contract.RiskTier = RiskHigh
	terminate.SQL = "SELECT pg_terminate_backend(4242)"
	assertDecision(t, gate.Authorize(context.Background(), terminate),
		VerdictBlocked, ReasonOutsideMaintenanceWindow)

	notMitigation := cancelRequest(RollbackNotReversible, true)
	assertDecision(t, gate.Authorize(context.Background(), notMitigation),
		VerdictBlocked, ReasonOutsideMaintenanceWindow)

	vacuum := ActionRequest{Contract: &ActionContract{ActionType: "vacuum_table",
		RiskTier: RiskModerate, RollbackClass: RollbackMitigationOnly},
		SQL: "VACUUM public.orders", Feature: string(ChangeVacuum), OperatorApproved: true}
	assertDecision(t, gate.Authorize(context.Background(), vacuum),
		VerdictBlocked, ReasonOutsideMaintenanceWindow)
}

func TestUnapprovedMitigationStillNeedsAHuman(t *testing.T) {
	runtime := advisoryRuntime()
	runtime.ExecutionMode, runtime.Tier3Moderate = ExecutionAuto, true
	runtime.TrustLevel = TrustAutonomous
	gate := mitigationGate(t, UnattendedProfile(), runtime)
	assertDecision(t, gate.Authorize(context.Background(),
		cancelRequest(RollbackMitigationOnly, false)), VerdictQueueApproval,
		ReasonApprovalRequired)
}

func TestApprovedMitigationKeepsEveryOtherGate(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime func(*RuntimeState)
		doc     func(*Document)
		reason  Reason
	}{
		"emergency stop": {runtime: func(r *RuntimeState) { r.EmergencyStop = true },
			reason: ReasonEmergencyStop},
		"executor disabled": {runtime: func(r *RuntimeState) { r.ExecutorEnabled = false },
			reason: ReasonExecutorDisabled},
		"replica": {runtime: func(r *RuntimeState) { r.IsReplica = true },
			reason: ReasonReplicaMutation},
		"observation trust": {runtime: func(r *RuntimeState) { r.TrustLevel = TrustObservation },
			reason: ReasonObserveOnly},
		"class not allowed": {doc: func(d *Document) {
			d.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
		}, reason: ReasonChangeClassNotAllowed},
	} {
		runtime, doc := advisoryRuntime(), StaffedProfile()
		if tc.runtime != nil {
			tc.runtime(&runtime)
		}
		if tc.doc != nil {
			tc.doc(&doc)
		}
		got := mitigationGate(t, doc, runtime).Authorize(context.Background(),
			cancelRequest(RollbackMitigationOnly, true))
		if got.Verdict == VerdictExecute || got.Reason != tc.reason {
			t.Errorf("%s: decision = %#v, want reason %s", name, got, tc.reason)
		}
	}
}

func TestMitigationOnlyIsAKnownRollbackClass(t *testing.T) {
	c := ActionContract{ActionType: "cancel_backend", RiskTier: RiskModerate,
		RollbackClass: RollbackMitigationOnly}
	if err := ValidateContract(c); err != nil {
		t.Fatalf("mitigation_only rejected: %v", err)
	}
	c.RollbackClass = "mitigation-ish"
	if err := ValidateContract(c); err == nil {
		t.Fatal("unknown rollback class accepted")
	}
}
