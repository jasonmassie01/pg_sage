package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// batchCallGate answers every family with execute and counts the batches.
type batchCallGate struct{ batches int }

func (g *batchCallGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized}
}

func (g *batchCallGate) ExplainBatch(
	_ context.Context, requests []policy.ActionRequest,
) []policy.Decision {
	g.batches++
	out := make([]policy.Decision, len(requests))
	for i := range out {
		out[i] = policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized}
	}
	return out
}

// A cancelled caller (a closed dashboard tab) gets fail-closed
// policy_unavailable verdicts and the gate's readers never run.
func TestExplainFamiliesCancelledContextFailsClosed(t *testing.T) {
	gate := &batchCallGate{}
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(gate)
	analyze, _ := ContractForActionType("analyze_table")
	index, _ := ContractForActionType("create_index_concurrently")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := exec.ExplainFamilies(ctx, []ActionContract{analyze, index}, false)

	if gate.batches != 0 {
		t.Fatalf("gate batches after cancel = %d, want 0", gate.batches)
	}
	if len(got) != 2 {
		t.Fatalf("decisions = %d, want 2", len(got))
	}
	for i, contract := range []ActionContract{analyze, index} {
		d := got[i]
		if d.Decision != PolicyDecisionBlocked ||
			d.BlockedReason != string(policy.ReasonPolicyUnavailable) ||
			d.RiskTier != contract.BaseRiskTier || len(d.Guardrails) != len(contract.Guardrails) {
			t.Fatalf("%s = %+v, want blocked policy_unavailable at %s", contract.ActionType, d,
				contract.BaseRiskTier)
		}
		if d.Detail == "" {
			t.Fatalf("%s detail is empty, want the cancellation named", contract.ActionType)
		}
	}
}

// A deadline that already passed fails closed the same way; a live context
// still reaches the gate.
func TestExplainFamiliesExpiredDeadlineFailsClosed(t *testing.T) {
	gate := &batchCallGate{}
	exec := New(nil, autonomousTestConfig(), time.Time{}, noopExecLog)
	exec.WithPolicyGate(gate)
	analyze, _ := ContractForActionType("analyze_table")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx err = %v", ctx.Err())
	}

	expired := exec.ExplainFamilies(ctx, []ActionContract{analyze}, false)
	live := exec.ExplainFamilies(context.Background(), []ActionContract{analyze}, false)

	if expired[0].BlockedReason != string(policy.ReasonPolicyUnavailable) {
		t.Fatalf("expired = %+v, want policy_unavailable", expired[0])
	}
	if gate.batches != 1 || live[0].Decision != PolicyDecisionExecute {
		t.Fatalf("live = %+v after %d batches, want execute from one batch", live[0],
			gate.batches)
	}
}
