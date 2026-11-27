package shadow

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Integration (real Postgres), earned-05: a manual VACUUM or ANALYZE run
// outside pg_sage after a shadow decision was recorded is external
// evidence, judged by the decision's recorded baseline against the metric
// read in the pass that first sees it. pg_sage's own runs, autovacuum, a
// gone relation, a statistics reset or a catalog it may not read are not.

// mntTable creates public.name with rows rows, dropped after the test.
func mntTable(t *testing.T, pool *pgxpool.Pool, name string, rows int) {
	t.Helper()
	mustExec(t, pool, `DROP TABLE IF EXISTS public.`+name)
	mustExec(t, pool, `CREATE TABLE public.`+name+` (a int, b int)
		WITH (autovacuum_enabled = false)`)
	mustExec(t, pool, `INSERT INTO public.`+name+` SELECT g, g % 7
		FROM generate_series(1, $1) g`, rows)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public.`+name) })
	waitCounter(t, pool, "public."+name, "n_tup_ins", rows)
}

// waitCounter waits until counter of table reached n: PG14's collector
// may otherwise count the writes after a VACUUM or ANALYZE that follows.
func waitCounter(t *testing.T, pool *pgxpool.Pool, table, counter string, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got int
		if err := pool.QueryRow(context.Background(), `SELECT COALESCE(`+counter+`, 0)
			FROM pg_stat_user_tables WHERE relid = to_regclass($1)`, table).Scan(&got); err != nil {
			t.Fatalf("read %s of %s: %v", counter, table, err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s of %s is %d, never reached %d", counter, table, got, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mntShadow records a class (vacuum or analyze) decision on object with
// its rule prediction and baseline (nil: none), recorded ago.
func mntShadow(t *testing.T, s *Store, pool *pgxpool.Pool, class, object string,
	baseline *float64, ago time.Duration) Decision {
	t.Helper()
	sql, metric := "VACUUM "+object, verify.MetricDeadTuples
	if class == "analyze" {
		sql, metric = "ANALYZE "+object, verify.MetricModsSinceAnalyze
	}
	d := sample(class, sql)
	d.Object, d.Fingerprint = object, Fingerprint(class, object, d.Shape)
	expected := -100.0
	d.Prediction = verify.Prediction{Class: class, Method: verify.MethodRule, Metric: metric,
		ExpectedChangePct: &expected, Baseline: baseline, Source: "rule"}
	stored := record(t, s, d)
	age(t, pool, stored.ID, ago)
	return get(t, s, stored.ID)
}

// waitStat waits until column (last_vacuum, last_analyze) of table shows a
// run: PG14 reports through the statistics collector, asynchronously.
func waitStat(t *testing.T, pool *pgxpool.Pool, table, column string) time.Time {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var at *time.Time
		if err := pool.QueryRow(context.Background(), `SELECT `+column+`
			FROM pg_stat_user_tables WHERE relid = to_regclass($1)`, table).Scan(&at); err != nil {
			t.Fatalf("read %s of %s: %v", column, table, err)
		}
		if at != nil {
			return *at
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s of %s never showed the run", column, table)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// justNow records a maintenance decision now: on PG14 a database's
// statistics start (pg_stat_database.stats_reset) when its first backend
// reports, so a decision dated before the fixture database existed would
// predate its statistics and be unscorable.
const justNow = time.Duration(0)

// settled is a scorer that takes a maintenance run as soon as it shows.
func settled(pool *pgxpool.Pool, o Options) *Scorer {
	sc := newScorer(pool, o)
	sc.settle = 0
	return sc
}

func ptr(v float64) *float64 { return &v }

func TestDetectAnalyzeOutsideIsCountedExternal(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_an", 5000)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_an", ptr(5000), justNow)
	sc := settled(pool, fastOptions())
	runOnce(t, sc)
	assertPending(t, s, d.ID)
	if got := get(t, s, d.ID); got.AppliedDetectedAt != nil {
		t.Fatalf("nothing ran yet, but the decision is marked applied: %+v", got)
	}
	mustExec(t, pool, `ANALYZE public.shadow_mnt_an`) // an operator in psql
	waitStat(t, pool, "public.shadow_mnt_an", "last_analyze")
	res := runOnce(t, sc)
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
	if got.AppliedDetectedAt == nil || got.ScoreDetail["verdict"] != verify.OutcomeImproved ||
		got.ScoreDetail["before"] != 5000.0 || got.ScoreDetail["after"] != 0.0 {
		t.Fatalf("detail %v, applied_detected_at %v", got.ScoreDetail, got.AppliedDetectedAt)
	}
	if res.Scored[SourceExternal] != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestDetectVacuumOutsideIsCountedExternal(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_vac", 2000)
	mustExec(t, pool, `DELETE FROM public.shadow_mnt_vac WHERE a <= 1500`)
	waitCounter(t, pool, "public.shadow_mnt_vac", "n_tup_del", 1500)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "vacuum", "public.shadow_mnt_vac", ptr(1500), justNow)
	mustExec(t, pool, `VACUUM public.shadow_mnt_vac`)
	waitStat(t, pool, "public.shadow_mnt_vac", "last_vacuum")
	runOnce(t, settled(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
	if got.ScoreDetail["metric"] != verify.MetricDeadTuples {
		t.Fatalf("detail %v", got.ScoreDetail)
	}
}

// A run that just finished may be pg_sage's own, not yet in its action
// log: it is taken only once the settle delay has passed.
func TestDetectWaitsOutTheSettleDelay(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_settle", 100)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_settle", ptr(100), justNow)
	mustExec(t, pool, `ANALYZE public.shadow_mnt_settle`)
	waitStat(t, pool, "public.shadow_mnt_settle", "last_analyze")
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
}

func TestDetectRelationGoneIsUnscored(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	o := fastOptions()
	gone := mntShadow(t, s, pool, "vacuum", "public.shadow_mnt_gone", ptr(10),
		o.Horizon+time.Minute)
	young := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_gone", ptr(10), time.Hour)
	runOnce(t, settled(pool, o))
	got := assertScore(t, s, gone.ID, ScoreUnscored, SourceNone, false)
	if !strings.Contains(got.ScoreReason, "relation gone") {
		t.Fatalf("reason %q", got.ScoreReason)
	}
	assertNotApplied(t, s, young.ID)
}

func TestDetectWithoutBaselineIsNeverCounted(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_nobase", 100)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_nobase", nil, justNow)
	mustExec(t, pool, `ANALYZE public.shadow_mnt_nobase`)
	waitStat(t, pool, "public.shadow_mnt_nobase", "last_analyze")
	runOnce(t, settled(pool, fastOptions()))
	assertScore(t, s, d.ID, ScoreUnscored, SourceExternal, false)
}

// Authorship (earned-03 guard): pg_sage's own VACUUM since the decision,
// logged under another shape than the decision's SQL, is not external.
func TestDetectOwnMaintenanceIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_own", 500)
	s := NewStore(pool)
	o := fastOptions()
	d := mntShadow(t, s, pool, "vacuum", "public.shadow_mnt_own", ptr(500), justNow)
	mustExec(t, pool, `VACUUM public.shadow_mnt_own`) // pg_sage's executor
	waitStat(t, pool, "public.shadow_mnt_own", "last_vacuum")
	executedAction(t, pool, "vacuum", `VACUUM (VERBOSE) public.shadow_mnt_own`, "success",
		"", 0)
	runOnce(t, settled(pool, o))
	assertNotApplied(t, s, d.ID)
	age(t, pool, d.ID, o.Horizon+time.Minute)
	runOnce(t, settled(pool, o))
	assertScore(t, s, d.ID, ScoreUnscored, SourceNone, false)
}

// The same VACUUM run through pg_sage is applied evidence, never counted.
func TestDetectOwnSameShapeIsApplied(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_same", 500)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "vacuum", "public.shadow_mnt_same", ptr(500), justNow)
	mustExec(t, pool, `VACUUM public.shadow_mnt_same`)
	waitStat(t, pool, "public.shadow_mnt_same", "last_vacuum")
	act := executedAction(t, pool, "vacuum", `VACUUM public.shadow_mnt_same`, "success",
		"improved", 0)
	runOnce(t, settled(pool, fastOptions()))
	got := assertScore(t, s, d.ID, ScoreCorrect, SourceApplied, false)
	if got.RefActionLogID != act || got.AppliedDetectedAt != nil {
		t.Fatalf("ref %d (want %d), applied_detected_at %v", got.RefActionLogID, act,
			got.AppliedDetectedAt)
	}
}

// denyingCatalog answers the catalog reads of one object with 42501.
type denyingCatalog struct {
	catalogReader
	denied string
}

func (c denyingCatalog) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if len(args) > 0 && args[0] == c.denied {
		return deniedRow{}
	}
	return c.catalogReader.QueryRow(ctx, sql, args...)
}

type deniedRow struct{}

func (deniedRow) Scan(...any) error {
	return &pgconn.PgError{Code: "42501", Message: "permission denied for table pg_class"}
}

func TestDetectPermissionDeniedIsUnscoredAndThePassGoesOn(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_ok", 100)
	s := NewStore(pool)
	o := fastOptions()
	denied := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_denied", ptr(100),
		o.Horizon+time.Hour)
	ok := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_ok", ptr(100), justNow)
	mustExec(t, pool, `ANALYZE public.shadow_mnt_ok`)
	waitStat(t, pool, "public.shadow_mnt_ok", "last_analyze")
	sc := settled(pool, o)
	sc.catalog = denyingCatalog{catalogReader: pool, denied: "public.shadow_mnt_denied"}
	res := runOnce(t, sc)
	got := assertScore(t, s, denied.ID, ScoreUnscored, SourceNone, false)
	if !strings.Contains(got.ScoreReason, "permission denied") ||
		!strings.Contains(got.ScoreReason, "42501") {
		t.Fatalf("reason %q", got.ScoreReason)
	}
	assertScore(t, s, ok.ID, ScoreCorrect, SourceExternal, true)
	if res.Scored[SourceNone] != 1 || res.Scored[SourceExternal] != 1 {
		t.Fatalf("result %+v", res)
	}
}

// Boundary: a run at exactly recorded_at is not after the decision; one
// microsecond later is.
func TestDetectRunAtRecordedAtIsNotApplied(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_edge", 100)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "vacuum", "public.shadow_mnt_edge", ptr(100), time.Hour)
	mustExec(t, pool, `VACUUM public.shadow_mnt_edge`)
	ran := waitStat(t, pool, "public.shadow_mnt_edge", "last_vacuum")
	recordAt := func(at time.Time) {
		mustExec(t, pool, `UPDATE sage.shadow_decision
			SET recorded_at = $2, last_seen_at = $2 WHERE id = $1`, d.ID, at)
	}
	recordAt(ran)
	runOnce(t, settled(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
	recordAt(ran.Add(-time.Microsecond))
	runOnce(t, settled(pool, fastOptions()))
	if got := get(t, s, d.ID); got.AppliedDetectedAt == nil || got.Status != StatusScored {
		t.Fatalf("a run 1us after recording: %+v", got)
	}
}

// pg_stat_reset() clears every table's counters of its database, so the
// test resets a database of its own.
func TestDetectStatisticsResetIsUnscored(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "shadow_stat_reset"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mntTable(t, pool, "shadow_mnt_reset", 100)
	s := NewStore(pool)
	o := fastOptions()
	d := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_reset", ptr(100), justNow)
	mustExec(t, pool, `SELECT pg_stat_reset()`)
	mustExec(t, pool, `ANALYZE public.shadow_mnt_reset`)
	waitStat(t, pool, "public.shadow_mnt_reset", "last_analyze")
	runOnce(t, settled(pool, o))
	assertNotApplied(t, s, d.ID)
	age(t, pool, d.ID, o.Horizon+time.Minute)
	runOnce(t, settled(pool, o))
	got := assertScore(t, s, d.ID, ScoreUnscored, SourceNone, false)
	if !strings.Contains(got.ScoreReason, "statistics were reset") {
		t.Fatalf("reason %q", got.ScoreReason)
	}
}

// Racing scorers mark the run applied and score it once (the
// applied_detected_at IS NULL and status guards).
func TestDetectConcurrentPassesScoreOnce(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_race", 100)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "analyze", "public.shadow_mnt_race", ptr(100), justNow)
	mustExec(t, pool, `ANALYZE public.shadow_mnt_race`)
	waitStat(t, pool, "public.shadow_mnt_race", "last_analyze")
	before := scoreCount("orders", "analyze", ScoreCorrect, SourceExternal)
	var wg sync.WaitGroup
	scored := make([]int, 4)
	errs := make([]error, 4)
	for i := range scored {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := settled(pool, fastOptions()).RunOnce(context.Background())
			scored[i], errs[i] = res.Scored[SourceExternal], err
		}(i)
	}
	wg.Wait()
	total := 0
	for i, n := range scored {
		if errs[i] != nil {
			t.Fatalf("scorer %d: %v", i, errs[i])
		}
		total += n
	}
	if total != 1 || scoreCount("orders", "analyze", ScoreCorrect, SourceExternal)-before != 1 {
		t.Fatalf("scored %d times across racing scorers, want 1 (%v)", total, scored)
	}
	assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
}

// CHECK-03: the classes without a catalog detector end unscored (none).
func TestDetectUnscoredClassesEndWithNone(t *testing.T) {
	pool, _ := testPool(t)
	mntTable(t, pool, "shadow_mnt_none", 10)
	s := NewStore(pool)
	o := fastOptions()
	var ids []int64
	for _, c := range []struct{ class, family, sql string }{
		{"config_guc", "tuning", `ALTER SYSTEM SET work_mem = '32MB'`},
		{"query_hint", "tuning", `SELECT hint_plan.set_hint('x', 'SeqScan(t)')`},
		{"reindex", "hygiene", `REINDEX TABLE CONCURRENTLY public.shadow_mnt_none`},
	} {
		d := sample(c.class, c.sql)
		d.Family, d.Object = c.family, "public.shadow_mnt_none"
		stored := record(t, s, d)
		age(t, pool, stored.ID, o.Horizon+time.Minute)
		ids = append(ids, stored.ID)
	}
	mustExec(t, pool, `REINDEX TABLE public.shadow_mnt_none`)
	runOnce(t, settled(pool, o))
	for _, id := range ids {
		got := assertScore(t, s, id, ScoreUnscored, SourceNone, false)
		if got.AppliedDetectedAt != nil {
			t.Fatalf("decision %d marked applied: %+v", id, got)
		}
	}
}

// pg_sage quotes every name of its VACUUM (finding SQL), while the object
// is schema.name unquoted: a table whose name needs quotes is still found
// (on PG14 the unquoted object is a syntax error that stopped every pass).
func TestDetectVacuumOfAQuotedNameIsCountedExternal(t *testing.T) {
	pool, _ := testPool(t)
	mustExec(t, pool, `DROP TABLE IF EXISTS public."shadow mnt sp"`)
	mustExec(t, pool, `CREATE TABLE public."shadow mnt sp" (a int)
		WITH (autovacuum_enabled = false)`)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public."shadow mnt sp"`) })
	mustExec(t, pool, `INSERT INTO public."shadow mnt sp" SELECT generate_series(1, 300)`)
	mustExec(t, pool, `DELETE FROM public."shadow mnt sp" WHERE a <= 200`)
	waitCounter(t, pool, `public."shadow mnt sp"`, "n_tup_del", 200)
	s := NewStore(pool)
	d := mntShadow(t, s, pool, "vacuum", "public.shadow mnt sp", ptr(200), justNow)
	mustExec(t, pool, `UPDATE sage.shadow_decision SET sql = $2 WHERE id = $1`, d.ID,
		`VACUUM "public"."shadow mnt sp";`)
	mustExec(t, pool, `VACUUM public."shadow mnt sp"`)
	waitStat(t, pool, `public."shadow mnt sp"`, "last_vacuum")
	runOnce(t, settled(pool, fastOptions()))
	assertScore(t, s, d.ID, ScoreCorrect, SourceExternal, true)
}
