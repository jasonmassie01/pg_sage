package shadow

import (
	"testing"
	"time"
)

// A shadow index create of an access method HypoPG cannot model
// (indexes-06) is not what-if scored: it stays pending, spends none of the
// pass's budget, and logs no what-if failure.
func TestScorerWhatIfSkipsMethodsHypoPGCannotModel(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	t.Cleanup(func() { mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid) })
	s := NewStore(pool)
	o := fastOptions()
	o.HypoPGBudget = 1
	gin := hypoShadow(t, s, pool, `CREATE INDEX CONCURRENTLY i_gin ON public.shadow_hypo `+
		`USING gin (a)`, qid, o.ScoreAfter+2*time.Hour)
	btree := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_bt ON public.shadow_hypo (a)`, qid, o.ScoreAfter+time.Hour)
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	res := runOnce(t, newScorer(pool, o))
	if res.Scored[SourceHypoPG] != 1 {
		t.Fatalf("result %+v: the GIN create must not spend the what-if budget", res)
	}
	assertPending(t, s, gin.ID)
	assertScore(t, s, btree.ID, ScoreCorrect, SourceHypoPG, true)
}
