package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Roadmap 2.3: the executor's standing gate consults the confirmed facts
// of its database, whichever order the binder and the gate are installed.

type stubFactBinder struct {
	bindings []policy.FactBinding
	calls    int
}

func (b *stubFactBinder) Bind(context.Context, policy.ActionRequest) ([]policy.FactBinding,
	error) {
	b.calls++
	return b.bindings, nil
}

func factBoundExecutor(binderFirst bool, binder policy.FactBinder) *Executor {
	exec := New(nil, autonomousConfig(), time.Now().Add(-60*24*time.Hour), noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	if binderFirst {
		exec.WithFactBinder(binder)
		exec.EnableStandingPolicyDocument(doc, nil)
	} else {
		exec.EnableStandingPolicyDocument(doc, nil)
		exec.WithFactBinder(binder)
	}
	return exec
}

func TestExecutorGateConsultsConfirmedFacts(t *testing.T) {
	for _, binderFirst := range []bool{true, false} {
		binder := &stubFactBinder{bindings: []policy.FactBinding{{FactID: 4,
			Type: "test_fixture", Subject: "test_*", Route: "excluded",
			Summary: "schemas test_* are test fixtures", ConfirmedBy: "op",
			ConfirmedAt: time.Now()}}}
		got := factBoundExecutor(binderFirst, binder).EvaluateCustodianProposal(
			context.Background(), freezeProposal())
		if got.Decision != PolicyDecisionBlocked ||
			got.BlockedReason != string(policy.ReasonBoundByFact) || binder.calls != 1 {
			t.Fatalf("binderFirst=%v: %+v (binder calls %d)", binderFirst, got, binder.calls)
		}
	}
}

func TestExecutorWithoutFactsOrBindingsIsUnchanged(t *testing.T) {
	plain := factBoundExecutor(true, nil).EvaluateCustodianProposal(context.Background(),
		freezeProposal())
	empty := factBoundExecutor(true, &stubFactBinder{}).EvaluateCustodianProposal(
		context.Background(), freezeProposal())
	if plain.Decision != empty.Decision || plain.BlockedReason != empty.BlockedReason {
		t.Fatalf("no binder %+v, no bindings %+v", plain, empty)
	}
	if plain.BlockedReason == string(policy.ReasonBoundByFact) {
		t.Fatalf("bound without facts: %+v", plain)
	}
}
