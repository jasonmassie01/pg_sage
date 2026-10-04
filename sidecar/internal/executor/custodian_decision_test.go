package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// SubmitCustodianProposalDecision runs a custodian proposal exactly like
// SubmitCustodianProposal and also reports what the gate decided, so a
// caller that asked for the proposal (the Postgres-specialist contract)
// can be told the verdict and its reason.

type decisionGate struct {
	decision policy.Decision
	requests []policy.ActionRequest
}

func (g *decisionGate) Authorize(_ context.Context, r policy.ActionRequest) policy.Decision {
	g.requests = append(g.requests, r)
	return g.decision
}

func TestSubmitCustodianProposalDecision_ReportsTheVerdict(t *testing.T) {
	cases := []struct {
		verdict  policy.Verdict
		decision string
	}{{policy.VerdictQueueApproval, PolicyDecisionQueueApproval},
		{policy.VerdictBlocked, PolicyDecisionBlocked},
		{policy.VerdictPark, PolicyDecisionParked}}
	for _, c := range cases {
		gate := &decisionGate{decision: policy.Decision{Verdict: c.verdict,
			RiskTier: policy.RiskSafe, Reason: policy.ReasonAutonomyLevel, Detail: "at L1"}}
		e := New(nil, &config.Config{}, time.Time{}, noopExecLog)
		e.WithPolicyGate(gate)
		got, err := e.SubmitCustodianProposalDecision(context.Background(), freezeProposal())
		if !errors.Is(err, ErrCustodianProposalWithheld) {
			t.Fatalf("%s: err %v", c.verdict, err)
		}
		if got.Decision.Decision != c.decision || got.Executed || got.HandedOff ||
			got.Decision.BlockedReason != string(policy.ReasonAutonomyLevel) {
			t.Fatalf("%s: %+v", c.verdict, got)
		}
		if len(gate.requests) != 1 || gate.requests[0].OperatorApproved ||
			gate.requests[0].Rollback || gate.requests[0].OwnerDeclared {
			t.Fatalf("%s: requests %+v", c.verdict, gate.requests)
		}
	}
}

func TestSubmitCustodianProposalDecision_NoGateFailsClosed(t *testing.T) {
	e := New(nil, &config.Config{}, time.Time{}, noopExecLog)
	got, err := e.SubmitCustodianProposalDecision(context.Background(), freezeProposal())
	if !errors.Is(err, ErrCustodianProposalWithheld) || got.Decision.Decision !=
		PolicyDecisionBlocked || got.Executed {
		t.Fatalf("no gate: %+v %v", got, err)
	}
}

func TestSubmitCustodianProposalDecision_HandoffIsReported(t *testing.T) {
	exec, pool := handoffExecutor(t, 2)
	clearHandoffs(t, pool, freezeHandoffKey)
	got, err := exec.SubmitCustodianProposalDecision(context.Background(), freezeProposal())
	if err != nil {
		t.Fatal(err)
	}
	if !got.HandedOff || got.Executed || got.Decision.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("L2 handoff: %+v", got)
	}
	if n := pendingHandoffs(t, pool, freezeHandoffKey); n != 1 {
		t.Fatalf("pending handoffs = %d", n)
	}
}

func TestCustodianContract_ExportsTheTypedContract(t *testing.T) {
	c, ok := CustodianContract("VACUUM (FREEZE) public.orders")
	if !ok || c.ActionType == "" || c.RollbackClass == "" || len(c.SuccessCriteria) == 0 {
		t.Fatalf("freeze contract %+v %t", c, ok)
	}
	if _, ok := CustodianContract("DROP TABLE public.orders"); ok {
		t.Fatal("SQL the executor refuses has no contract")
	}
}
