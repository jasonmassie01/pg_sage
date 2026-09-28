package policy

import (
	"context"
	"testing"
	"time"
)

// Operator-approved requests (decision 2026-09-26): the human approval
// replaces tier, ramp, execution-mode and self-initiated usage checks, but
// hard stops, SQL validation, provider support, trust, the standing
// policy's change-class allowlist and its maintenance windows still apply.

func operatorRequest(risk RiskTier) ActionRequest {
	req := validIndexRequest()
	req.Contract.RiskTier = risk
	req.OperatorApproved = true
	return req
}

func TestOperatorApprovalBypassesTierRampModeAndUsage(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.TrustLevel = TrustAdvisory
	runtime.ExecutionMode = ExecutionManual
	runtime.Tier3Safe, runtime.Tier3Moderate = false, false
	runtime.RampStart = time.Time{}
	var calls []string
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true, calls: &calls,
		usage: LimitUsage{SelfInitiatedChangesInWindow: 10_000, TablesInWindow: 10_000}})

	for _, risk := range []RiskTier{RiskSafe, RiskModerate, RiskHigh} {
		got := gate.Authorize(context.Background(), operatorRequest(risk))
		assertDecision(t, got, VerdictExecute, ReasonOperatorApproved)
	}
	assertNotCalled(t, calls, "usage")
}

func TestOperatorApprovalKeepsHardStopsAndValidation(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskSafe)),
		VerdictBlocked, ReasonEmergencyStop)

	gate = newTestGate(t, gateFixture{})
	req := operatorRequest(RiskSafe)
	req.SQL = "DROP TABLE public.orders"
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictPark, ReasonNoTypedContract)

	runtime = providerRuntime("rds")
	gate = newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	req = operatorRequest(RiskSafe)
	req.Contract.ProviderSupport = []string{"postgres"}
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonProviderUnsupported)
}

func TestOperatorApprovalRespectsTrust(t *testing.T) {
	for trust, reason := range map[string]Reason{
		TrustObservation: ReasonObserveOnly,
		"mystery":        ReasonUnknownTrustLevel,
	} {
		runtime := newTestGateRuntime()
		runtime.TrustLevel = trust
		gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
		got := gate.Authorize(context.Background(), operatorRequest(RiskSafe))
		if got.Verdict == VerdictExecute || got.Reason != reason {
			t.Fatalf("trust %q: decision = %#v, want reason %s", trust, got, reason)
		}
	}
}

func TestOperatorApprovalRespectsChangeClassAllowlist(t *testing.T) {
	doc := UnattendedProfile()
	doc.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
	gate := newTestGate(t, gateFixture{policy: doc})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskSafe)),
		VerdictBlocked, ReasonChangeClassNotAllowed)
}

// Policy windows and a configured trust window bound moderate/high operator
// actions; an unconfigured trust window does not (legacy semantics kept).
func TestOperatorApprovalRespectsMaintenanceWindows(t *testing.T) {
	closedDoc := StaffedProfile() // weekdays 01:00-05:00
	// newTestGate's clock is Sunday 2026-07-26 02:00 UTC: outside it.
	gate := newTestGate(t, gateFixture{policy: closedDoc})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskModerate)),
		VerdictBlocked, ReasonOutsideMaintenanceWindow)
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskSafe)),
		VerdictExecute, ReasonOperatorApproved)

	runtime := newTestGateRuntime()
	runtime.WindowConfigured, runtime.InConfiguredWindow = true, false
	gate = newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskHigh)),
		VerdictBlocked, ReasonOutsideMaintenanceWindow)

	runtime.WindowConfigured, runtime.InConfiguredWindow = false, false
	gate = newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskHigh)),
		VerdictExecute, ReasonOperatorApproved)
}

func TestOperatorApprovalRejectsUnknownRisk(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest("mystery")),
		VerdictBlocked, ReasonUnknownRiskTier)
}

func TestAutonomousPathRejectsUnknownTrustLevel(t *testing.T) {
	for _, trust := range []string{"", "mystery"} {
		runtime := newTestGateRuntime()
		runtime.TrustLevel = trust
		gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
		assertDecision(t, gate.Authorize(context.Background(), validIndexRequest()),
			VerdictBlocked, ReasonUnknownTrustLevel)
	}
}
