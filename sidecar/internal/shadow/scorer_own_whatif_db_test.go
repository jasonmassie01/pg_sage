package shadow

import (
	"testing"
	"time"
)

// Integration (real Postgres), earned-03 review: an index create pg_sage
// built itself (its action past the pass's fact window) is no evidence
// at all. The what-if must not score it either: the real index is in the
// catalog, so the what-if of the same index finds no gain and would count
// an incorrect score as shadow evidence. It stays unscored at the horizon.
func TestScorerOwnIndexBeyondTheFactWindowIsNotWhatIfScored(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	t.Cleanup(func() { mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid) })
	s := NewStore(pool)
	o := fastOptions()
	sql := `CREATE INDEX CONCURRENTLY i_own_hypo ON public.shadow_hypo (a)`
	d := hypoShadow(t, s, pool, sql, qid, o.ScoreAfter+3*time.Hour)
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	busyActions(t, pool, factBatch+100, o.ScoreAfter+2*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_hypo_sage ON public.shadow_hypo (a)`)
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY shadow_hypo_sage ON public.shadow_hypo (a)`,
		"success", "improved", o.ScoreAfter+time.Hour)
	res := runOnce(t, newScorer(pool, o))
	assertPending(t, s, d.ID)
	assertNotApplied(t, s, d.ID)
	if res.Waiting != 1 || len(res.Scored) != 0 {
		t.Fatalf("result %+v, want the decision waiting and nothing scored", res)
	}
	unscoredAtHorizon(t, pool, s, d.ID)
}

// State transition, earned-03 review: a decision already marked applied
// outside pg_sage (by a pass before this check, or before pg_sage's own
// action was logged) whose change pg_sage's own action explains is not
// scored external when its verification decides; it stays unscored.
func TestScorerMarkedChangePgSageMadeIsNotScoredExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_marked")
	s := NewStore(pool)
	d := externalShadow(t, s, pool, "index_create",
		`CREATE INDEX CONCURRENTLY i_marked ON public.shadow_marked (a)`, 3*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_marked_sage ON public.shadow_marked (a)`)
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY shadow_marked_sage ON public.shadow_marked (a)`,
		"success", "improved", 150*time.Minute)
	busyActions(t, pool, factBatch+100, 170*time.Minute)
	// Marked applied, with a verification that would decide "improved".
	now := time.Now()
	detectedAt(t, pool, d.ID, 120*time.Minute, 90*time.Minute)
	seedSeries(t, pool, extQID, now.Add(-150*time.Minute), 30, 10, 100)
	seedSeries(t, pool, extQID, now.Add(-120*time.Minute), 60, 10, 20)
	res := runOnce(t, newScorer(pool, fastOptions()))
	assertPending(t, s, d.ID)
	if res.Waiting != 1 || len(res.Scored) != 0 {
		t.Fatalf("result %+v, want the decision waiting and nothing scored", res)
	}
	unscoredAtHorizon(t, pool, s, d.ID)
}

// Earned-03 review: pg_sage built the index after the decision and rolled
// it back (its action past the pass's action window), so the catalog has
// no index; that action already decided the decision, and the what-if
// must not count it as shadow evidence too.
func TestScorerOwnRolledBackIndexIsNotWhatIfScored(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	t.Cleanup(func() { mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid) })
	s := NewStore(pool)
	o := fastOptions()
	d := hypoShadow(t, s, pool, `CREATE INDEX CONCURRENTLY i_own_rb ON public.shadow_hypo (a)`,
		qid, o.ScoreAfter+3*time.Hour)
	other := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_other_rb ON public.shadow_hypo (a, b)`, qid,
		o.ScoreAfter+3*time.Hour)
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	busyActions(t, pool, factBatch+100, o.ScoreAfter+2*time.Hour)
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY shadow_hypo_rb ON public.shadow_hypo (a)`,
		"rolled_back", "regressed", o.ScoreAfter+time.Hour)
	res := runOnce(t, newScorer(pool, o))
	assertPending(t, s, d.ID)
	// The what-if still scores a decision pg_sage never acted on.
	assertScore(t, s, other.ID, ScoreCorrect, SourceHypoPG, true)
	if res.Scored[SourceHypoPG] != 1 {
		t.Fatalf("result %+v, want one what-if score", res)
	}
	unscoredAtHorizon(t, pool, s, d.ID)
}
