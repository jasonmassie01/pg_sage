package shadow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Integration (real Postgres), earned-05 phase B: an autovacuum reloption
// set outside pg_sage to exactly the proposed value, or the same extended
// statistics object created outside pg_sage, is found in the catalog,
// marked applied, and verified over the same call-weighted windows as an
// index change (statistics with their row estimates too).

// configShadow records a class decision for sql targeting extQID on
// object, last proposed lastSeenAgo.
func configShadow(t *testing.T, s *Store, pool *pgxpool.Pool, class, family, object,
	sql string, lastSeenAgo time.Duration) Decision {
	t.Helper()
	d := sample(class, sql)
	d.Family, d.Object, d.Fingerprint = family, object, Fingerprint(class, object, d.Shape)
	d.Prediction.TargetQueryIDs = []int64{extQID}
	stored := record(t, s, d)
	mustExec(t, pool, `UPDATE sage.shadow_decision
		SET recorded_at = now() - make_interval(secs => $2) - interval '1 hour',
		    last_seen_at = now() - make_interval(secs => $2) WHERE id = $1`,
		stored.ID, lastSeenAgo.Seconds())
	return get(t, s, stored.ID)
}

// improvedAround seeds extQID 100 ms before and 20 ms after a change
// detected 90 minutes ago (pg_sage last saw its proposal 120 minutes ago).
func improvedAround(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	now := time.Now()
	detectedAt(t, pool, id, 120*time.Minute, 90*time.Minute)
	seedSeries(t, pool, extQID, now.Add(-150*time.Minute), 30, 10, 100)
	seedSeries(t, pool, extQID, now.Add(-120*time.Minute), 60, 10, 20)
}

// State transitions: pending, then applied_detected, then a final
// verdict, then counted.
func TestDetectReloptionSetToTheProposedValue(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_rel")
	mustExec(t, pool, `ALTER TABLE public.shadow_rel SET (autovacuum_vacuum_scale_factor = 0.1)`)
	s := NewStore(pool)
	d := configShadow(t, s, pool, "autovacuum_tuning", "tuning", "public.shadow_rel",
		`ALTER TABLE public.shadow_rel SET (autovacuum_vacuum_scale_factor = 0.05)`, 3*time.Hour)
	sc := newScorer(pool, fastOptions())
	runOnce(t, sc)
	assertNotApplied(t, s, d.ID) // another value is no match
	mustExec(t, pool, `ALTER TABLE public.shadow_rel SET (autovacuum_vacuum_scale_factor = 0.05)`)
	runOnce(t, sc)
	got := get(t, s, d.ID)
	if got.AppliedDetectedAt == nil || got.Status != StatusPending {
		t.Fatalf("detection: %+v", got)
	}
	improvedAround(t, pool, d.ID)
	runOnce(t, sc)
	assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
}

func TestDetectUnparsableReloptionIsUnscoredAtTheHorizon(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_rel_reset")
	s := NewStore(pool)
	o := fastOptions()
	d := configShadow(t, s, pool, "autovacuum_tuning", "tuning", "public.shadow_rel_reset",
		`ALTER TABLE public.shadow_rel_reset RESET (autovacuum_vacuum_scale_factor)`, time.Hour)
	runOnce(t, newScorer(pool, o))
	assertNotApplied(t, s, d.ID)
	age(t, pool, d.ID, o.Horizon+time.Minute)
	runOnce(t, newScorer(pool, o))
	assertScore(t, s, d.ID, ScoreUnscored, SourceNone, false)
}

// pg_sage's own ALTER TABLE since the decision is not external.
func TestDetectOwnReloptionIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_rel_own")
	s := NewStore(pool)
	d := configShadow(t, s, pool, "autovacuum_tuning", "tuning", "public.shadow_rel_own",
		`ALTER TABLE public.shadow_rel_own SET (autovacuum_vacuum_threshold = 50)`, time.Hour)
	mustExec(t, pool, `ALTER TABLE public.shadow_rel_own SET (autovacuum_vacuum_threshold = 50)`)
	executedAction(t, pool, "alter",
		`ALTER TABLE public.shadow_rel_own SET (autovacuum_vacuum_threshold = '50')`,
		"monitoring", "", time.Minute)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
}

func TestDetectStatisticsOfTheSameShape(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_stx")
	s := NewStore(pool)
	d := configShadow(t, s, pool, "statistics", "tuning", "public.shadow_stx",
		`CREATE STATISTICS public.sage_stx_shadow_stx ON a, b FROM public.shadow_stx`, 3*time.Hour)
	mustExec(t, pool, `CREATE STATISTICS public.mig_kinds (ndistinct) ON a, b
		FROM public.shadow_stx`)
	sc := newScorer(pool, fastOptions())
	runOnce(t, sc)
	assertNotApplied(t, s, d.ID) // fewer kinds is another object
	mustExec(t, pool, `CREATE STATISTICS public.mig_same ON b, a FROM public.shadow_stx`)
	runOnce(t, sc)
	if got := get(t, s, d.ID); got.AppliedDetectedAt == nil || got.Status != StatusPending {
		t.Fatalf("detection: %+v", got)
	}
	improvedAround(t, pool, d.ID)
	runOnce(t, sc)
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
	if _, ok := got.ScoreDetail["estimates"]; !ok {
		t.Fatalf("statistics detail %v carries no estimate judgement", got.ScoreDetail)
	}
}

func TestDetectStatisticsOnAGoneTableIsUnscored(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	o := fastOptions()
	d := configShadow(t, s, pool, "statistics", "tuning", "public.shadow_stx_gone",
		`CREATE STATISTICS public.sage_stx_gone ON a, b FROM public.shadow_stx_gone`,
		o.Horizon+time.Minute)
	runOnce(t, newScorer(pool, o))
	got := assertScore(t, s, d.ID, ScoreUnscored, SourceNone, false)
	if got.ScoreReason == "" || got.AppliedDetectedAt != nil {
		t.Fatalf("got %+v", got)
	}
}

// The read of pg_sage's own actions on a table since a decision walks the
// action time index in its generic plan (EXPLAIN (GENERIC_PLAN): PG16).
func TestDetectOwnTableActionsWalkTheTimeIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "scorer_detect_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:scorer_detect_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	plan := genericPlan(ctx, t, pool, ownTableActionsSQL)
	walked := plan.Has(func(n testdb.PlanNode) bool { return n.Index == "idx_action_log_time" })
	if !walked || plan.SeqScans("action_log") != 0 {
		t.Errorf("own table actions do not walk idx_action_log_time:\n%s", plan)
	}
}
