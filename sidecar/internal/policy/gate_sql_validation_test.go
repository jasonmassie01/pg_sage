package policy

import (
	"context"
	"strings"
	"testing"
)

// Decision 2026-09-27: a build without the parse-tree SQL layer (no cgo)
// must not mutate unattended. Mutating actions it would have executed on its
// own are queued for operator approval instead; read-only diagnostics and
// operator-approved actions are unaffected.

func degradedRuntime() RuntimeState {
	runtime := newTestGateRuntime()
	runtime.SQLValidationDegraded = true
	return runtime
}

func TestDegradedSQLValidationQueuesAutonomousMutation(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: degradedRuntime(), runtimeSet: true})
	got := gate.Authorize(context.Background(), validIndexRequest())
	assertDecision(t, got, VerdictQueueApproval, ReasonSQLValidationDegraded)
	if !strings.Contains(got.Detail, "cgo") {
		t.Errorf("detail %q does not say how to fix it", got.Detail)
	}
}

func TestDegradedSQLValidationQueuesEveryMutatingTier(t *testing.T) {
	for _, risk := range []RiskTier{RiskSafe, RiskModerate} {
		runtime := degradedRuntime()
		runtime.TrustLevel = TrustAutonomous
		gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
		req := validIndexRequest()
		req.Contract.RiskTier = risk
		got := gate.Authorize(context.Background(), req)
		if got.Verdict == VerdictExecute {
			t.Errorf("risk %s executed unattended without parse-tree validation", risk)
		}
	}
}

func TestDegradedSQLValidationAllowsReadOnly(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: degradedRuntime(), runtimeSet: true})
	req := validIndexRequest()
	req.Contract.RiskTier = RiskReadOnly
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictExecute, ReasonAuthorized)
}

func TestDegradedSQLValidationAllowsOperatorApproval(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: degradedRuntime(), runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), operatorRequest(RiskSafe)),
		VerdictExecute, ReasonOperatorApproved)
}

// Degradation never loosens: an action already blocked stays blocked for
// its original reason.
func TestDegradedSQLValidationKeepsEarlierRefusals(t *testing.T) {
	runtime := degradedRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), validIndexRequest()),
		VerdictBlocked, ReasonEmergencyStop)
}

func TestHealthySQLValidationStillExecutes(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: newTestGateRuntime(), runtimeSet: true})
	assertDecision(t, gate.Authorize(context.Background(), validIndexRequest()),
		VerdictExecute, ReasonAuthorized)
}
