package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Dogfood lifeos, 2026-10-03: every parked candidate got two gate
// decisions per executor cycle (decision 395082: created and repeated
// 13 ms apart; the parked events index: 50 repeats in 24 cycles).
// processFinding stopped only on blocked and observe-only verdicts, so a
// park fell through to Apply, whose first authorization evaluated the
// same candidate again.

// A parked verdict ends the candidate's cycle after one authorization.
func TestParkedCandidateIsAuthorizedOncePerCycle(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictPark)
	gate := &countingGate{inner: fixedGate{verdict: policy.VerdictPark}}
	fx.exec.WithPolicyGate(gate)

	fx.propose(t)
	fx.exec.RunCycle(fx.ctx, false)

	if gate.count() != 1 {
		t.Fatalf("a parked candidate consulted the gate %d times in one cycle, want 1",
			gate.count())
	}
	if fx.actionRows(t) != 0 {
		t.Fatal("a parked candidate was executed")
	}
}

// An executed candidate is authorized once before its waits and once
// after them, never a third time.
func TestExecutedCandidateIsAuthorizedThenReauthorized(t *testing.T) {
	fx := newRecFixture(t, autovacuumProbe2, policy.VerdictExecute)
	gate := &countingGate{inner: fixedGate{verdict: policy.VerdictExecute,
		decisionID: recordCustodianDecision(t, fx.ctx, fx.pool, "rec_cycle", fx.table)}}
	fx.exec.WithPolicyGate(gate)

	fx.propose(t)
	fx.exec.RunCycle(fx.ctx, false)

	if gate.count() != 2 {
		t.Fatalf("an executed candidate consulted the gate %d times, want 2 "+
			"(authorization and re-authorization)", gate.count())
	}
	if fx.actionRows(t) != 1 {
		t.Fatalf("action rows = %d, want the candidate executed once", fx.actionRows(t))
	}
}

// Through the real standing gate and ledger, cycle after cycle: a candidate
// parked by its budget is evaluated exactly once per cycle.
func TestParkedCandidateRecordsOneEvaluationPerCycle(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosLegacyPolicy()
	doc.BlastRadius.MaxTablesPerWindow = 0 // every new table parks
	fx := newStaleFixtureOn(t, pool, ctx, "autonomous", verifiedDetail(), doc)
	for cycle := 1; cycle <= 3; cycle++ {
		fx.exec.RunCycle(ctx, false)
		var rows, repeats int
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(repeat_count), 0)
			FROM sage.decision WHERE target_objects = $1::jsonb`,
			`["`+fx.f.ObjectIdentifier+`"]`).Scan(&rows, &repeats); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || repeats != cycle {
			t.Fatalf("after cycle %d: %d decision rows seen %d times, want 1 row seen "+
				"%d times (one evaluation per cycle)", cycle, rows, repeats, cycle)
		}
	}
	if fx.indexExists(t, fx.index()) {
		t.Fatal("the parked index was built")
	}
}
