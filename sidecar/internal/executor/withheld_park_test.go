package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// Dogfood lifeos 1.8.3: the CREATE INDEX withheld because its rollback
// drops another index was re-attempted every cycle (actions 6385, 6399):
// a new execute decision and another failed action_log row each time,
// although nothing about the finding had changed. A withhold that depends
// only on the finding's content is recorded once and the finding is
// parked until its content changes.

func TestUnverifiableCreateIsParkedAcrossCycles(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), staleRollback)
	for range 3 {
		fx.exec.RunCycle(fx.ctx, false)
	}
	a := fx.onlyAction(t)
	if a.outcome != "failed" {
		t.Fatalf("action = %+v, want the single withheld record", a)
	}
	if got := fx.decisionVerdicts(t)["execute/authorized"]; got != 1 {
		t.Fatalf("execute decisions = %d (%v), want 1: a parked finding is not "+
			"re-authorized every cycle", got, fx.decisionVerdicts(t))
	}
	if fx.indexExists(t, fx.index()) {
		t.Fatal("the withheld CREATE INDEX ran")
	}
}

// A new revision with a rollback that drops the created index is new
// content: the park lifts and the build runs once.
func TestParkedCreateRunsAfterContentChanges(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), staleRollback)
	fx.exec.RunCycle(fx.ctx, false)
	fixed := fx.f
	fixed.RollbackSQL = ownRollback(fx.index())
	res, err := fx.recs.Propose(fx.ctx, analyzer.RecommendationProposal(fx.database, fixed))
	if err != nil || res.Outcome != recommendation.OutcomeRevised {
		t.Fatalf("revise recommendation: %+v, %v", res, err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if !fx.indexExists(t, fx.index()) {
		t.Fatalf("index not built after the rollback was fixed; decisions %v",
			fx.decisionVerdicts(t))
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 2 {
		t.Fatalf("actions = %d, want the withheld record and one build", n)
	}
}
