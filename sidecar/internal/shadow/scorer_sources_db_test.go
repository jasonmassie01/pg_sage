package shadow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Integration (real Postgres): the sources that make shadow evidence the
// trust ledger counts. (b) a change applied outside pg_sage (a migration)
// is found in the catalog and verified with the same call-weighted
// statistics as pg_sage's own actions, over query_store windows around
// when it appeared; (c) HypoPG what-if for an index create whose targeted
// queries still run.

const extQID = int64(9100101)

// externalShadow records a shadow decision for sql targeting extQID that
// pg_sage last proposed lastSeenAgo.
func externalShadow(t *testing.T, s *Store, pool *pgxpool.Pool, class, sql string,
	lastSeenAgo time.Duration) Decision {
	t.Helper()
	d := sample(class, sql)
	d.Prediction.TargetQueryIDs = []int64{extQID}
	stored := record(t, s, d)
	mustExec(t, pool, `UPDATE sage.shadow_decision
		SET recorded_at = now() - make_interval(secs => $2) - interval '1 hour',
		    last_seen_at = now() - make_interval(secs => $2) WHERE id = $1`,
		stored.ID, lastSeenAgo.Seconds())
	return stored
}

// detectedAt moves the detection of an applied change: pg_sage last saw
// its proposal appliedAfterAgo, and found the change applied detectedAgo.
func detectedAt(t *testing.T, pool *pgxpool.Pool, id int64, appliedAfterAgo,
	detectedAgo time.Duration) {
	t.Helper()
	mustExec(t, pool, `UPDATE sage.shadow_decision
		SET applied_after = now() - make_interval(secs => $2),
		    applied_detected_at = now() - make_interval(secs => $3) WHERE id = $1`,
		id, appliedAfterAgo.Seconds(), detectedAgo.Seconds())
}

func TestScorerExternalIndexCreateIsVerified(t *testing.T) {
	pool, _ := testPool(t)
	mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_ext`)
	mustExec(t, pool, `CREATE TABLE public.shadow_ext (a int, b int)`)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_ext`) })
	s := NewStore(pool)
	d := externalShadow(t, s, pool, "index_create",
		`CREATE INDEX CONCURRENTLY i_shadow_ext ON public.shadow_ext (a)`, 3*time.Hour)
	sc := newScorer(pool, fastOptions())
	runOnce(t, sc)
	if got := get(t, s, d.ID); got.AppliedDetectedAt != nil || got.Status != StatusPending {
		t.Fatalf("nothing applied yet: %+v", got)
	}
	// A migration creates the same index under its own name.
	mustExec(t, pool, `CREATE INDEX shadow_ext_by_migration ON public.shadow_ext (a)`)
	runOnce(t, sc)
	got := get(t, s, d.ID)
	if got.AppliedDetectedAt == nil || got.AppliedAfter == nil ||
		!got.AppliedAfter.Equal(got.LastSeenAt) || got.Status != StatusPending {
		t.Fatalf("detection: %+v", got)
	}
	// The verification windows around the change: 100 ms before, 20 ms after.
	now := time.Now()
	detectedAt(t, pool, d.ID, 120*time.Minute, 90*time.Minute)
	seedSeries(t, pool, extQID, now.Add(-150*time.Minute), 30, 10, 100)
	seedSeries(t, pool, extQID, now.Add(-120*time.Minute), 60, 10, 20)
	runOnce(t, sc)
	scored := assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
	if scored.ScoreDetail["verdict"] != verify.OutcomeImproved {
		t.Fatalf("score detail %v must carry the verification verdict", scored.ScoreDetail)
	}
}

func TestScorerExternalChangeWaitsForItsWindow(t *testing.T) {
	pool, _ := testPool(t)
	mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_wait`)
	mustExec(t, pool, `CREATE TABLE public.shadow_wait (a int)`)
	mustExec(t, pool, `CREATE INDEX shadow_wait_mig ON public.shadow_wait (a)`)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_wait`) })
	s := NewStore(pool)
	d := externalShadow(t, s, pool, "index_create",
		`CREATE INDEX CONCURRENTLY i_w ON public.shadow_wait (a)`, 2*time.Hour)
	o := fastOptions()
	detectedAt(t, pool, d.ID, 40*time.Minute, 10*time.Minute) // after window not elapsed
	seedSeries(t, pool, extQID, time.Now().Add(-70*time.Minute), 70, 10, 100)
	res := runOnce(t, newScorer(pool, o))
	assertPending(t, s, d.ID)
	if res.Waiting != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestScorerExternalDropHeldIsCorrectForHygiene(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	// The index pg_sage wanted to drop no longer exists (a migration
	// dropped it); reads held through the drop window.
	d := externalShadow(t, s, pool, "index_drop",
		`DROP INDEX CONCURRENTLY public.shadow_gone_idx`, 3*time.Hour)
	o := fastOptions()
	now := time.Now()
	detectedAt(t, pool, d.ID, 120*time.Minute, 90*time.Minute)
	seedSeries(t, pool, extQID, now.Add(-150*time.Minute), 150, 10, 50)
	runOnce(t, newScorer(pool, o))
	assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
}

func TestScorerExternalDropRegressionIsIncorrect(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	d := externalShadow(t, s, pool, "index_drop",
		`DROP INDEX CONCURRENTLY public.shadow_gone_too`, 3*time.Hour)
	now := time.Now()
	detectedAt(t, pool, d.ID, 120*time.Minute, 90*time.Minute)
	seedSeries(t, pool, extQID, now.Add(-150*time.Minute), 30, 10, 20)
	seedSeries(t, pool, extQID, now.Add(-120*time.Minute), 120, 10, 200)
	runOnce(t, newScorer(pool, fastOptions()))
	assertScore(t, s, d.ID, ScoreIncorrect, SourceExternal, true)
}

// hypoFixture installs HypoPG and pg_stat_statements in the fixture
// database and a table whose filter column has no index, then runs the
// target query so pg_stat_statements knows it. It returns its queryid.
func hypoFixture(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	for _, ext := range []string{"pg_stat_statements", "hypopg"} {
		if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			t.Skipf("%s is not available on this server (%v): the what-if source is "+
				"not verified here", ext, err)
		}
	}
	mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_hypo`)
	mustExec(t, pool, `CREATE TABLE public.shadow_hypo (a int, b int)`)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public.shadow_hypo`) })
	mustExec(t, pool, `INSERT INTO public.shadow_hypo
		SELECT i, i % 3 FROM generate_series(1, 20000) i`)
	mustExec(t, pool, `ANALYZE public.shadow_hypo`)
	for i := 0; i < 5; i++ {
		mustExec(t, pool, `SELECT count(*) FROM public.shadow_hypo WHERE a = $1`, i)
	}
	var qid int64
	if err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE '%shadow_hypo WHERE a = $1%' LIMIT 1`).Scan(&qid); err != nil {
		t.Skipf("pg_stat_statements did not record the target query (%v); "+
			"is it in shared_preload_libraries?", err)
	}
	mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid)
	return qid
}

// hypoShadow is a shadow index create targeting qid, recorded ago.
func hypoShadow(t *testing.T, s *Store, pool *pgxpool.Pool, sql string, qid int64,
	ago time.Duration) Decision {
	t.Helper()
	d := sample("index_create", sql)
	d.Prediction.TargetQueryIDs = []int64{qid}
	stored := record(t, s, d)
	age(t, pool, stored.ID, ago)
	return stored
}

func TestScorerWhatIfScoresIndexCreates(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	t.Cleanup(func() { mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid) })
	s := NewStore(pool)
	o := fastOptions()
	good := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_hypo_a ON public.shadow_hypo (a)`, qid, o.ScoreAfter+time.Hour)
	useless := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_hypo_b ON public.shadow_hypo (b)`, qid, o.ScoreAfter+time.Hour)
	// The targeted query still runs after the decisions were recorded.
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	res := runOnce(t, newScorer(pool, o))
	g := assertScore(t, s, good.ID, ScoreCorrect, SourceHypoPG, true)
	if pct, ok := g.ScoreDetail["improvement_pct"].(float64); !ok || pct < o.HypoPGMinPct {
		t.Fatalf("what-if detail %v", g.ScoreDetail)
	}
	assertScore(t, s, useless.ID, ScoreIncorrect, SourceHypoPG, true)
	if res.Scored[SourceHypoPG] != 2 {
		t.Fatalf("result %+v", res)
	}
}

func TestScorerWhatIfWaitsWhileTheTargetsAreIdle(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	s := NewStore(pool)
	o := fastOptions()
	d := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_idle ON public.shadow_hypo (a)`, qid, o.ScoreAfter+time.Hour)
	// The query ran only before the decision was recorded.
	seedSeries(t, pool, qid, time.Now().Add(-o.ScoreAfter-3*time.Hour), 20, 5, 10)
	runOnce(t, newScorer(pool, o))
	assertPending(t, s, d.ID)
}

func TestScorerWhatIfIsBudgetedPerPass(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	t.Cleanup(func() { mustExec(t, pool, `DELETE FROM sage.query_store WHERE queryid = $1`, qid) })
	s := NewStore(pool)
	o := fastOptions()
	o.HypoPGBudget = 1
	a := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_b1 ON public.shadow_hypo (a)`, qid, o.ScoreAfter+2*time.Hour)
	b := hypoShadow(t, s, pool,
		`CREATE INDEX CONCURRENTLY i_b2 ON public.shadow_hypo (a, b)`, qid, o.ScoreAfter+time.Hour)
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	res := runOnce(t, newScorer(pool, o))
	if res.Scored[SourceHypoPG] != 1 {
		t.Fatalf("first pass %+v, want one what-if", res)
	}
	// Oldest first.
	assertScore(t, s, a.ID, ScoreCorrect, SourceHypoPG, true)
	assertPending(t, s, b.ID)
	runOnce(t, newScorer(pool, o))
	assertScore(t, s, b.ID, ScoreCorrect, SourceHypoPG, true)
}

// Precedence against a live what-if: an applied change outranks it.
func TestScorerAppliedOutranksAPassingWhatIf(t *testing.T) {
	pool, _ := testPool(t)
	qid := hypoFixture(t, pool)
	s := NewStore(pool)
	o := fastOptions()
	sql := `CREATE INDEX CONCURRENTLY i_prec ON public.shadow_hypo (a)`
	d := hypoShadow(t, s, pool, sql, qid, o.ScoreAfter+time.Hour)
	seedSeries(t, pool, qid, time.Now().Add(-30*time.Minute), 20, 5, 10)
	act := executedAction(t, pool, "create_index", sql, "rolled_back", "regressed",
		30*time.Minute)
	runOnce(t, newScorer(pool, o))
	got := assertScore(t, s, d.ID, ScoreIncorrect, SourceApplied, false)
	if got.RefActionLogID != act {
		t.Fatalf("ref %d, want %d", got.RefActionLogID, act)
	}
}
