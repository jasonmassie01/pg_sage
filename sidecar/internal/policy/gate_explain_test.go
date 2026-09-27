package policy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The provider-support check lived only in the legacy executor engine
// (G4-I01), so the gate authorized actions a provider cannot run.
func TestGateBlocksUnsupportedProvider(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: providerRuntime("rds"), runtimeSet: true})
	req := validIndexRequest()
	req.Contract.ProviderSupport = []string{"postgres", "cloud-sql"}

	decision := gate.Authorize(context.Background(), req)

	assertDecision(t, decision, VerdictBlocked, ReasonProviderUnsupported)
	if !strings.Contains(decision.Detail, "rds") {
		t.Fatalf("detail %q does not name the provider", decision.Detail)
	}
}

func TestGateProviderSupportMatching(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		support        []string
	}{
		{"no restriction", "rds", nil},
		{"case-insensitive", "Cloud-SQL", []string{"cloud-sql"}},
		{"empty provider is self-managed postgres", "", []string{"postgres"}},
		{"self-managed is postgres", "self-managed", []string{"postgres"}},
	} {
		gate := newTestGate(t, gateFixture{runtime: providerRuntime(tc.provider), runtimeSet: true})
		req := validIndexRequest()
		req.Contract.ProviderSupport = tc.support
		decision := gate.Authorize(context.Background(), req)
		if decision.Verdict != VerdictExecute {
			t.Errorf("%s: decision = %#v, want execute", tc.name, decision)
		}
	}
}

// Hard stops outrank provider support so the operator sees the real cause.
func TestGateHardStopPrecedesProviderCheck(t *testing.T) {
	runtime := providerRuntime("rds")
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	req := validIndexRequest()
	req.Contract.ProviderSupport = []string{"postgres"}

	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonEmergencyStop)
}

// Explain evaluates exactly like Authorize but records nothing, so UI
// readiness can show the gate's verdict without writing ledger rows.
func TestGateExplainMatchesAuthorizeWithoutRecording(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	req := validIndexRequest()

	explained := mustExplainer(t, gate).Explain(context.Background(), req)

	if explained.Verdict != VerdictExecute || explained.Reason != ReasonAuthorized {
		t.Fatalf("explain = %#v, want execute/authorized", explained)
	}
	if explained.EvidenceID != "" || explained.DecisionID != 0 {
		t.Fatalf("explain carried ledger identifiers: %#v", explained)
	}
	assertNotCalled(t, calls, "evidence")
	authorized := gate.Authorize(context.Background(), req)
	if authorized.Verdict != explained.Verdict || authorized.Reason != explained.Reason {
		t.Fatalf("authorize %#v disagrees with explain %#v", authorized, explained)
	}
}

func TestGateExplainQueuesApprovalGuardrail(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := validIndexRequest()
	req.Contract.Guardrails = []Guardrail{GuardrailApprovalRequired}

	got := mustExplainer(t, gate).Explain(context.Background(), req)
	if got.Verdict != VerdictQueueApproval || got.Reason != ReasonApprovalRequired {
		t.Fatalf("explain = %#v, want queue_approval/approval_required", got)
	}
}

// Action-family readiness has no concrete SQL. ExplainFamily skips SQL
// validation in Explain only; Authorize must never honor it, or a caller
// could execute unvalidated SQL by setting the flag.
func TestGateExplainFamilySkipsSQLOnlyWhenExplaining(t *testing.T) {
	var calls []string
	gate := newTestGate(t, gateFixture{calls: &calls})
	req := validIndexRequest()
	req.SQL = ""
	req.ExplainFamily = true

	explained := mustExplainer(t, gate).Explain(context.Background(), req)
	if explained.Verdict != VerdictExecute {
		t.Fatalf("family explain = %#v, want execute", explained)
	}
	assertNotCalled(t, calls, "validate_sql")

	req.SQL = "DROP TABLE public.orders"
	authorized := gate.Authorize(context.Background(), req)
	if authorized.Verdict != VerdictPark || authorized.Reason != ReasonNoTypedContract {
		t.Fatalf("Authorize honored ExplainFamily: %#v", authorized)
	}
}

func TestGateExplainFailsClosedWithoutRuntime(t *testing.T) {
	gate := NewGate(GateConfig{})
	got := mustExplainer(t, gate).Explain(context.Background(), validIndexRequest())
	if got.Verdict != VerdictBlocked || got.Reason != ReasonPolicyUnavailable {
		t.Fatalf("explain without runtime = %#v, want blocked/policy_unavailable", got)
	}
}

func providerRuntime(provider string) RuntimeState {
	runtime := newTestGateRuntime()
	runtime.Provider = provider
	return runtime
}

func newTestGateRuntime() RuntimeState {
	return RuntimeState{
		ExecutorEnabled: true, TrustLevel: TrustAutonomous, ExecutionMode: ExecutionAuto,
		Tier3Safe: true, Tier3Moderate: true, InConfiguredWindow: true,
		// newTestGate's clock is 2026-07-26; the ramp is long satisfied.
		RampStart: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
}

func mustExplainer(t *testing.T, gate Gate) Explainer {
	t.Helper()
	explainer, ok := gate.(Explainer)
	if !ok {
		t.Fatalf("%T does not implement policy.Explainer", gate)
	}
	return explainer
}
