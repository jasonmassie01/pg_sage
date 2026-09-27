package executor

import (
	"context"
	"testing"
	"time"
)

// Family readiness (fleet overview, cases) is the gate's verdict for each
// action family, evaluated in one batch, with nothing recorded.
func TestExplainFamiliesUsesGateBatch(t *testing.T) {
	recorded := 0
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	exec.WithPolicyGate(explainTestGate(time.Now(), &recorded))
	contracts := []ActionContract{}
	for _, actionType := range []string{"analyze_table", "cancel_backend"} {
		contract, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("missing contract %s", actionType)
		}
		contracts = append(contracts, contract)
	}

	got := exec.ExplainFamilies(context.Background(), contracts, false)

	if len(got) != 2 || got[0].Decision != PolicyDecisionExecute ||
		got[1].Decision != PolicyDecisionQueueApproval {
		t.Fatalf("family decisions = %+v, want execute then queue_for_approval", got)
	}
	if len(got[0].Guardrails) == 0 {
		t.Fatal("family decision lost the contract's descriptive guardrails")
	}
	if recorded != 0 {
		t.Fatalf("family readiness recorded %d decisions", recorded)
	}
}

func TestExplainFamiliesFailsClosedWithoutGate(t *testing.T) {
	exec := New(nil, autonomousTestConfig(), nil, time.Time{}, noopExecLog)
	contract, _ := ContractForActionType("analyze_table")
	got := exec.ExplainFamilies(context.Background(), []ActionContract{contract}, false)
	if len(got) != 1 || got[0].Decision != PolicyDecisionBlocked ||
		got[0].BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("without gate = %+v, want blocked %q", got, reasonNoStandingPolicy)
	}
	if got := exec.ExplainFamilies(context.Background(), nil, false); len(got) != 0 {
		t.Fatalf("empty family list = %+v", got)
	}
}
