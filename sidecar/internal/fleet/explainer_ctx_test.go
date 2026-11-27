package fleet

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

type explainCtxKey struct{}

// ctxRecordingGate remembers the context value each batch arrived with.
type ctxRecordingGate struct{ seen []any }

func (g *ctxRecordingGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictBlocked}
}

func (g *ctxRecordingGate) ExplainBatch(
	ctx context.Context, requests []policy.ActionRequest,
) []policy.Decision {
	g.seen = append(g.seen, ctx.Value(explainCtxKey{}))
	out := make([]policy.Decision, len(requests))
	for i := range out {
		out[i] = policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized}
	}
	return out
}

func analyzeContracts(t *testing.T) []executor.ActionContract {
	t.Helper()
	contract, ok := executor.ContractForActionType("analyze_table")
	if !ok {
		t.Fatal("missing analyze_table contract")
	}
	return []executor.ActionContract{contract}
}

// The caller's context reaches the gate, so a closed request stops the
// policy reads instead of running them under context.Background.
func TestExecutorFamilyExplainerPassesCallerContext(t *testing.T) {
	exec := gateExecutor(t, "autonomous")
	gate := &ctxRecordingGate{}
	exec.WithPolicyGate(gate)
	ctx := context.WithValue(context.Background(), explainCtxKey{}, "request-7")

	got := ExecutorFamilyExplainer(exec)(ctx, analyzeContracts(t), false)

	if len(gate.seen) != 1 || gate.seen[0] != "request-7" {
		t.Fatalf("gate saw ctx values %v, want the caller's request-7", gate.seen)
	}
	if len(got) != 1 || got[0].Decision != executor.PolicyDecisionExecute {
		t.Fatalf("decisions = %+v, want execute", got)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got = ExecutorFamilyExplainer(exec)(cancelled, analyzeContracts(t), false)
	if len(gate.seen) != 1 || got[0].BlockedReason != string(policy.ReasonPolicyUnavailable) {
		t.Fatalf("cancelled = %+v after %d batches, want policy_unavailable without a batch",
			got[0], len(gate.seen))
	}
}

// The stopped-instance and nil-instance branches still fail closed and
// never consult the gate.
func TestInstanceFamilyExplainerStoppedAndNilFailClosed(t *testing.T) {
	exec := gateExecutor(t, "autonomous")
	gate := &ctxRecordingGate{}
	exec.WithPolicyGate(gate)
	stopped := &DatabaseInstance{Name: "prod", Executor: exec, Stopped: true}

	got := InstanceFamilyExplainer(stopped)(context.Background(), analyzeContracts(t), false)
	if got[0].Decision != executor.PolicyDecisionBlocked ||
		got[0].BlockedReason != "emergency stop is active" || len(gate.seen) != 0 {
		t.Fatalf("stopped = %+v (%d batches), want blocked emergency stop", got[0], len(gate.seen))
	}
	got = InstanceFamilyExplainer(nil)(context.Background(), analyzeContracts(t), false)
	if got[0].Decision != executor.PolicyDecisionBlocked ||
		got[0].BlockedReason != "standing policy unavailable" {
		t.Fatalf("nil instance = %+v, want standing policy unavailable", got[0])
	}
}
