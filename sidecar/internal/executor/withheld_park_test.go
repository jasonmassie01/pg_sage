package executor

import (
	"strings"
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
	fx.exec.RunCycle(fx.ctx, false)
	// One attempt writes the gate's decision plus Apply's re-authorizations.
	first := fx.decisionVerdicts(t)["execute/authorized"]
	for range 2 {
		fx.exec.RunCycle(fx.ctx, false)
	}
	a := fx.onlyAction(t)
	if a.outcome != "failed" {
		t.Fatalf("action = %+v, want the single withheld record", a)
	}
	if got := fx.decisionVerdicts(t)["execute/authorized"]; first == 0 || got != first {
		t.Fatalf("execute decisions = %d after the first cycle, %d after three: a "+
			"parked finding is not re-authorized every cycle", first, got)
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

// executor-05: a build withheld because its index name is taken is parked
// while the name exists, and the park lifts once that index is gone.
func TestNameTakenParkLiftsWhenTheIndexIsGone(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), ownRollback)
	taken := "CREATE INDEX " + fx.index() + " ON public." + fx.table + " (id)"
	if _, err := fx.pool.Exec(fx.ctx, taken); err != nil {
		t.Fatalf("take the name: %v", err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 {
		t.Fatalf("actions = %d while the name is taken, want one withheld record", n)
	}
	if _, err := fx.pool.Exec(fx.ctx, "DROP INDEX public."+fx.index()); err != nil {
		t.Fatalf("free the name: %v", err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 2 {
		t.Fatalf("actions = %d after the name was freed, want the record and a build", n)
	}
	var def string
	if err := fx.pool.QueryRow(fx.ctx, "SELECT pg_get_indexdef(to_regclass($1))",
		"public."+fx.index()).Scan(&def); err != nil {
		t.Fatalf("read the built index: %v", err)
	}
	if !strings.Contains(def, "(status)") {
		t.Fatalf("index definition = %q, want the finding's build", def)
	}
}
