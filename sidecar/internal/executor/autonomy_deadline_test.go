package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Coordinator decision 2026-10-02: a red wraparound freeze (a critical
// XID deadline the standing policy lets override) runs whatever the
// ledger says, and is still stopped by the emergency stop. The executor
// reports the operator's bound so the ledger can carry over the autonomy
// the configuration already grants.

func redFreezeProposal() CustodianProposal {
	p := freezeProposal()
	p.Deadline = &policy.DeadlineContext{Kind: policy.DeadlineXID,
		Urgency: policy.UrgencyCritical, HardAt: time.Now().Add(6 * time.Hour)}
	return p
}

func deadlineExecutor(limit policy.AutonomyLimit, stopped bool) *Executor {
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return stopped }
	exec.SetExecutionMode("auto")
	exec.WithAutonomy(&countingLimiter{limit: limit})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec
}

func TestRedWraparoundFreezeRunsAtL1AndWhileDowngraded(t *testing.T) {
	for name, limit := range map[string]policy.AutonomyLimit{
		"L1": {Level: 1, Granted: 1},
		"burning budget": {Level: 1, Granted: 3, Downgraded: true,
			Reasons: []string{"error_budget_fast_burn"}},
	} {
		got := deadlineExecutor(limit, false).EvaluateCustodianProposal(
			context.Background(), redFreezeProposal())
		if got.Decision != PolicyDecisionExecute {
			t.Errorf("%s: red wraparound freeze = %+v", name, got)
		}
		amber := deadlineExecutor(limit, false).EvaluateCustodianProposal(
			context.Background(), freezeProposal())
		if amber.Decision == PolicyDecisionExecute {
			t.Errorf("%s: a freeze without a mandatory deadline executed: %+v", name, amber)
		}
	}
}

func TestRedWraparoundFreezeIsStoppedByTheEmergencyStop(t *testing.T) {
	got := deadlineExecutor(policy.AutonomyLimit{Level: 3, Granted: 3}, true).
		EvaluateCustodianProposal(context.Background(), redFreezeProposal())
	if got.Decision != PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonEmergencyStop) {
		t.Fatalf("red freeze under e-stop = %+v", got)
	}
}

func TestOperatorBoundReportsTheConfiguration(t *testing.T) {
	exec := New(nil, autonomousConfig(), time.Now(), noopExecLog)
	exec.SetExecutionMode("auto")
	b := exec.OperatorBound()
	if !b.ExecutorEnabled || b.ExecutionMode != policy.ExecutionAuto ||
		b.TrustLevel != policy.TrustAutonomous || !b.Tier3Safe || !b.Tier3Moderate {
		t.Fatalf("bound = %+v", b)
	}
	exec.SetExecutionMode("approval")
	if exec.OperatorBound().ExecutionMode != policy.ExecutionApproval {
		t.Fatal("the bound does not follow the execution mode")
	}
	if (*Executor)(nil).OperatorBound().ExecutorEnabled {
		t.Fatal("a nil executor reports an enabled bound")
	}
}
