package slo

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// measured.md M12 and static.md F10 (v1.8.3): every SLO window was
// re-aggregated from raw samples every minute (a 30-day window reads
// ~43,000 samples per series per evaluation), and the generic plan of the
// aggregate read every SLO's samples in the window (`$3 = '' OR series =
// $3` defeated the primary key). Each sample now carries its series'
// running reset-compensated counters, so a window is a few primary-key
// probes per series; the raw aggregation stays as the reference and the
// fallback for samples stored before the running counters existed.

// referenceAggregateSQL is the raw aggregation shipped before v1.8.3.
const referenceAggregateSQL = `
WITH s AS (
    SELECT series, observed_at, bad, eligible,
           observed_at >= $4 AS inside,
           row_number() OVER (PARTITION BY series, observed_at >= $4
                              ORDER BY observed_at DESC) AS rn
    FROM sage.sre_sli_samples
    WHERE deployment_id = $1 AND slo_name = $2 AND ($3 = '' OR series = $3)
      AND observed_at > $4::timestamptz - make_interval(secs => $6)
      AND observed_at <= $5
), kept AS (
    SELECT series, observed_at, bad, eligible,
           lag(bad) OVER w AS pbad, lag(eligible) OVER w AS pel
    FROM s WHERE inside OR rn = 1
    WINDOW w AS (PARTITION BY series ORDER BY observed_at)
)
SELECT series, count(*)::int, min(observed_at), max(observed_at),
       COALESCE(sum(CASE WHEN pbad IS NULL THEN 0 WHEN bad >= pbad THEN bad - pbad
                         ELSE bad END), 0),
       COALESCE(sum(CASE WHEN pel IS NULL THEN 0 WHEN eligible >= pel THEN eligible - pel
                         ELSE eligible END), 0),
       count(*) FILTER (WHERE pbad IS NOT NULL AND (bad < pbad OR eligible < pel))::int
FROM kept GROUP BY series ORDER BY series`

func referenceAggregate(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	dep sre.UUID, slo, series string, from, to time.Time,
	lookback time.Duration) []SeriesAgg {
	t.Helper()
	rows, err := pool.Query(ctx, referenceAggregateSQL, string(dep), slo, series, from, to,
		lookback.Seconds())
	if err != nil {
		t.Fatalf("reference aggregate: %v", err)
	}
	defer rows.Close()
	var out []SeriesAgg
	for rows.Next() {
		var a SeriesAgg
		if err := rows.Scan(&a.Series, &a.Samples, &a.First, &a.Last, &a.Bad, &a.Eligible,
			&a.Resets); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameAggs(got, want []SeriesAgg) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Series != w.Series || g.Samples != w.Samples || !g.First.Equal(w.First) ||
			!g.Last.Equal(w.Last) || g.Resets != w.Resets ||
			math.Abs(g.Bad-w.Bad) > 1e-6*math.Max(1, w.Bad) ||
			math.Abs(g.Eligible-w.Eligible) > 1e-6*math.Max(1, w.Eligible) {
			return false
		}
	}
	return true
}

// seedCounterSeries records n samples per series a minute apart ending
// at end, with counter resets, a gap longer than the lookback, and a
// shuffled (late, out-of-order) insertion order.
func seedCounterSeries(t *testing.T, ctx context.Context, st *Store, dep sre.UUID,
	slo string, end time.Time, n int, series ...string) {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	var all []PushSample
	for _, name := range series {
		bad, el := 0.0, 0.0
		for i := 0; i < n; i++ {
			if i >= n/2 && i < n/2+30 {
				continue // a 30-minute gap
			}
			if i%97 == 0 && i > 0 {
				bad, el = 0, 0 // a counter reset
			}
			el += float64(100 + rng.Intn(50))
			bad += float64(rng.Intn(5))
			all = append(all, PushSample{Series: name, Bad: bad, Eligible: el,
				ObservedAt: end.Add(-time.Duration(n-i) * time.Minute)})
		}
	}
	// Mostly in order, a few late: swap 5% of neighbours.
	for i := 1; i < len(all); i++ {
		if rng.Intn(20) == 0 {
			all[i], all[i-1] = all[i-1], all[i]
		}
	}
	for _, s := range all {
		if _, err := st.RecordSample(ctx, dep, slo, s, nil); err != nil {
			t.Fatalf("record %+v: %v", s, err)
		}
	}
}

func TestAggregate_RunningCountersMatchTheRawReference(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("cumulative")
	end := time.Now().UTC().Truncate(time.Second)
	seedCounterSeries(t, ctx, st, scope.DeploymentID, name, end, 420, "pod-a", "pod-b")
	windows := []time.Duration{5 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour,
		6 * time.Hour, 12 * time.Hour}
	lookbacks := []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
	offsets := []time.Duration{0, 7 * time.Minute, 3*time.Hour + 30*time.Second}
	for _, w := range windows {
		for _, lb := range lookbacks {
			for _, off := range offsets {
				to := end.Add(-off)
				for _, series := range []string{"", "pod-b"} {
					got, err := st.Aggregate(ctx, scope.DeploymentID, name, series,
						to.Add(-w), to, lb)
					if err != nil {
						t.Fatalf("aggregate: %v", err)
					}
					want := referenceAggregate(t, ctx, st.pool, scope.DeploymentID, name,
						series, to.Add(-w), to, lb)
					if !sameAggs(got, want) {
						t.Fatalf("window %s lookback %s offset %s series %q:\n got %+v\nwant %+v",
							w, lb, off, series, got, want)
					}
				}
			}
		}
	}
}

// Samples stored before the running counters existed (NULL counters) are
// still aggregated exactly, through the raw path.
func TestAggregate_LegacySamplesUseTheRawPath(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("legacy")
	end := time.Now().UTC().Truncate(time.Second)
	if _, err := st.pool.Exec(ctx, `INSERT INTO sage.sre_sli_samples (deployment_id,
		slo_name, series, observed_at, bad, eligible)
		SELECT $1, $2, 'pod-a', $3::timestamptz - g * interval '1 minute', 100 - g,
		       10000 - 50 * g
		FROM generate_series(1, 90) g`, string(scope.DeploymentID), name, end); err != nil {
		t.Fatalf("legacy rows: %v", err)
	}
	if _, err := st.RecordSample(ctx, scope.DeploymentID, name, PushSample{Series: "pod-a",
		Bad: 120, Eligible: 11000, ObservedAt: end}, nil); err != nil {
		t.Fatal(err)
	}
	for _, w := range []time.Duration{10 * time.Minute, time.Hour} {
		got, err := st.Aggregate(ctx, scope.DeploymentID, name, "", end.Add(-w), end,
			5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		want := referenceAggregate(t, ctx, st.pool, scope.DeploymentID, name, "",
			end.Add(-w), end, 5*time.Minute)
		if !sameAggs(got, want) || len(want) != 1 {
			t.Fatalf("window %s: got %+v, want %+v", w, got, want)
		}
	}
}

// A window's cost does not grow with its length: over a week of
// one-minute samples the aggregate reads a few rows per series, and its
// generic plan probes the primary key by SLO name.
func TestAggregate_ReadsAFewRowsPerSeries(t *testing.T) {
	pool, ctx := livePool(t)
	cfg := pool.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	st, err := NewStore(traced)
	if err != nil {
		t.Fatal(err)
	}
	scope := bindScope(t, ctx, pool)
	name := uniqueName("week")
	end := time.Now().UTC().Truncate(time.Second)
	seedCounterSeries(t, ctx, st, scope.DeploymentID, name, end, 7*24*60/6, "pod-a")
	if _, err := pool.Exec(ctx, "ANALYZE sage.sre_sli_samples"); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	if _, err := st.Aggregate(ctx, scope.DeploymentID, name, "", end.Add(-7*24*time.Hour),
		end, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	stmts := rec.Matching("sage.sre_sli_samples")
	if len(stmts) == 0 {
		t.Fatal("no aggregate statement recorded")
	}
	for _, q := range stmts {
		plan, err := testdb.Explain(ctx, pool, "ANALYZE", q.SQL, q.Args...)
		if err != nil {
			t.Fatal(err)
		}
		var read float64
		plan.Walk(func(n testdb.PlanNode) {
			if n.Relation == "sre_sli_samples" {
				read += n.ActualRows * n.ActualLoops
			}
		})
		if read > 10 {
			t.Fatalf("a 7-day window read %v samples, want a few per series:\n%s", read, plan)
		}
		checkGenericPlan(t, ctx, pool, q)
	}
}

func checkGenericPlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	q testdb.RecordedQuery) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET plan_cache_mode") }()
	plan, err := testdb.Explain(ctx, conn, "", q.SQL, q.Args...)
	if err != nil {
		t.Fatal(err)
	}
	var conds []string
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation == "sre_sli_samples" {
			conds = append(conds, n.NodeType+" "+n.Index+" "+n.IndexCond)
		}
	})
	sort.Strings(conds)
	for _, c := range conds {
		if !strings.Contains(c, "slo_name") {
			t.Fatalf("generic plan reads samples without the SLO name in its index "+
				"condition: %v\n%s", conds, plan)
		}
	}
}

// Concurrent writers of one series (two pushers interleaving their
// samples) leave running counters that still aggregate exactly.
func TestRecordSample_ConcurrentWritersKeepCountersExact(t *testing.T) {
	st, scope, ctx := liveStore(t)
	name := uniqueName("concurrent")
	end := time.Now().UTC().Truncate(time.Second)
	const n = 120
	errs := make(chan error, 2)
	for w := 0; w < 2; w++ {
		go func(w int) {
			for i := w; i < n; i += 2 {
				s := PushSample{Series: "pod-a", Bad: float64(i / 3),
					Eligible: float64(100 * i), ObservedAt: end.Add(-time.Duration(n-i) *
						time.Minute)}
				if _, err := st.RecordSample(ctx, scope.DeploymentID, name, s, nil); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(w)
	}
	for w := 0; w < 2; w++ {
		if err := <-errs; err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	for _, w := range []time.Duration{10 * time.Minute, time.Hour, 3 * time.Hour} {
		got, err := st.Aggregate(ctx, scope.DeploymentID, name, "", end.Add(-w), end,
			5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		want := referenceAggregate(t, ctx, st.pool, scope.DeploymentID, name, "",
			end.Add(-w), end, 5*time.Minute)
		if !sameAggs(got, want) {
			t.Fatalf("window %s: got %+v, want %+v", w, got, want)
		}
	}
}
