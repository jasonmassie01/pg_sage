package policy

import (
	"context"
	"testing"
)

// executor-08: a request whose statement changes another object than the
// one it names is blocked as target_mismatch, with the two names in the
// detail; a person's approval cannot lift it, and the shadow Explain
// answers the same.
func TestTargetMismatchBlocks(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	ctx := context.Background()
	control := validIndexRequest()
	assertDecision(t, gate.Authorize(ctx, control), VerdictExecute, ReasonAuthorized)

	req := validIndexRequest()
	req.TargetMismatch = "finding names public.a; the statement changes public.orders"
	got := gate.Authorize(ctx, req)
	assertDecision(t, got, VerdictBlocked, ReasonTargetMismatch)
	if got.Detail != req.TargetMismatch {
		t.Errorf("detail = %q, want %q", got.Detail, req.TargetMismatch)
	}
	if got.RiskTier != RiskSafe {
		t.Errorf("risk tier = %q, want the contract's %q", got.RiskTier, RiskSafe)
	}
	explainer, ok := gate.(Explainer)
	if !ok {
		t.Fatal("the standing gate does not implement Explainer")
	}
	if explained := explainer.Explain(ctx, req); explained.Verdict != VerdictBlocked ||
		explained.Reason != ReasonTargetMismatch {
		t.Errorf("Explain = %s/%s, want blocked/target_mismatch", explained.Verdict,
			explained.Reason)
	}

	approved := operatorRequest(RiskSafe)
	approved.TargetMismatch = req.TargetMismatch
	assertDecision(t, gate.Authorize(ctx, approved), VerdictBlocked, ReasonTargetMismatch)
}

// A request without a contract is no_typed_contract first: the mismatch
// is only meaningful for a typed statement.
func TestTargetMismatchAfterContractCheck(t *testing.T) {
	gate := newTestGate(t, gateFixture{})
	req := validIndexRequest()
	req.Contract = nil
	req.TargetMismatch = "finding names public.a; the statement changes public.orders"
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictPark, ReasonNoTypedContract)
}

// The emergency stop still wins over the mismatch (hard stops come first).
func TestTargetMismatchUnderEmergencyStop(t *testing.T) {
	runtime := newTestGateRuntime()
	runtime.EmergencyStop = true
	gate := newTestGate(t, gateFixture{runtime: runtime, runtimeSet: true})
	req := validIndexRequest()
	req.TargetMismatch = "finding names public.a; the statement changes public.orders"
	assertDecision(t, gate.Authorize(context.Background(), req),
		VerdictBlocked, ReasonEmergencyStop)
}
