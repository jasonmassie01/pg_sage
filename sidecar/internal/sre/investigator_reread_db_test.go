package sre

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Regression (found by the sre-bench investigator arm): a model that
// re-reads a probe after the fault has moved on must not drop or move
// the graph's conclusive root by re-diagnosis. Only the contest path,
// under earned authority, may change a conclusive root.

// rereadGone re-runs lock_graph after the chain has cleared.
func rereadGone(runner *scriptedRunner) fakeReply {
	reread := call(ToolRunProbe, probeArgs(probes.LockGraph))
	return func(w http.ResponseWriter, body string) {
		runner.mu.Lock()
		runner.scripts[probes.LockGraph] = []probes.Result{rows(probes.LockGraph)}
		runner.mu.Unlock()
		reread(w, body)
	}
}

func TestInvestigator_ReadsCannotDropAConclusiveRoot(t *testing.T) {
	for name, final := range map[string][]fakeReply{
		"inconclusive final": {submitFixed(invFinal{Outcome: "inconclusive"})},
		"no final":           {call(ToolGraphState, `{}`), call(ToolFacts, `{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, budgetLimits())
			runner := idleChainRunner()
			m := newFakeModel(t, append([]fakeReply{rereadGone(runner)}, final...)...)
			c, _ := investigatorCoordinator(t, ctx, st, runner, m.client(), invOptions{})
			inv := startAndRun(t, ctx, c, lockTrigger("inv-reread"))
			run := transcriptOf(t, inv)
			if steps := stepsOf(run, ToolRunProbe); len(steps) != 1 ||
				steps[0].EvidenceID == "" {
				t.Fatalf("re-read steps = %+v, want one lock_graph run", steps)
			}
			assertIdleRoot(t, st, inv)
		})
	}
}
