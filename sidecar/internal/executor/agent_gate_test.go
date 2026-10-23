package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// §6.2.1 wiring: cmd injects agent governance into the standing gate with
// WithAgentDecider; the decision ledger records the principal (§6.2.3).

type levelDecider struct {
	mu    sync.Mutex
	level int
	calls int
}

func (d *levelDecider) Decide(context.Context, policy.ActionRequest) (
	policy.Decision, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return policy.Decision{}, d.level, false
}

func agentStandingExecutor(t *testing.T) *Executor {
	t.Helper()
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec
}

func TestStandingGateConsultsAgentDecider(t *testing.T) {
	exec := agentStandingExecutor(t)
	decider := &levelDecider{level: 1}
	exec.WithAgentDecider(decider) // after the gate is installed: still used
	ctx := policy.WithPrincipalRef(context.Background(),
		policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", Tool: "sre_propose_action"})
	got := exec.EvaluateCustodianProposal(ctx, freezeProposal())
	if got.Decision != PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAgentProposalRecorded) ||
		decider.calls != 1 {
		t.Fatalf("decision = %+v, decider calls = %d", got, decider.calls)
	}
	// pg_sage's own proposal is untouched by agent governance.
	got = exec.EvaluateCustodianProposal(context.Background(), freezeProposal())
	if got.Decision != PolicyDecisionExecute || decider.calls != 1 {
		t.Fatalf("own decision = %+v, decider calls = %d", got, decider.calls)
	}
}

func TestStandingGateWithoutDeciderCapsAgentsAtApproval(t *testing.T) {
	exec := agentStandingExecutor(t)
	ctx := policy.WithPrincipalRef(context.Background(), policy.PrincipalRef{})
	got := exec.EvaluateCustodianProposal(ctx, freezeProposal())
	if got.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("decision = %+v, want queue_approval with governance off", got)
	}
}

func TestLedgerInputRecordsPrincipal(t *testing.T) {
	req := policy.ActionRequest{Feature: "index", ArtifactHash: "sha256:abc",
		Principal: &policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", TaskID: "t-1"}}
	in := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictQueueApproval})
	if in.PrincipalID != "agp_aaaaaaaaaaaaaaaaaaaa" || in.TaskID != "t-1" ||
		in.ArtifactHash != "sha256:abc" {
		t.Fatalf("ledger input = %+v, want principal, task and artifact hash", in)
	}
	other := ledgerInput(nil, 1, policy.ActionRequest{Feature: "index",
		Principal: &policy.PrincipalRef{ID: "agp_bbbbbbbbbbbbbbbbbbbb"}},
		policy.Decision{Verdict: policy.VerdictQueueApproval})
	if in.Fingerprint == other.Fingerprint {
		t.Fatal("two agents' withheld decisions must not fold into one row")
	}
	own := ledgerInput(nil, 1, policy.ActionRequest{Feature: "index"},
		policy.Decision{Verdict: policy.VerdictQueueApproval})
	if own.PrincipalID != "" || own.TaskID != "" {
		t.Fatalf("pg_sage's own decision = %+v, want no principal", own)
	}
}
