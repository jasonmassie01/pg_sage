package analyzer

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// rules-05: slow_query and query_regression judge the interval mean (the
// time added between two snapshots over the calls added), not the
// cumulative pg_stat_statements mean, which hides a plan flip under a
// long history of fast calls.

// No concurrent access tests: the rules are pure functions over two
// snapshots, and the baseline reader is called once per cycle by the
// single analyzer goroutine.

var intervalEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// intervalStats is one statement's counters, with the cumulative mean the
// collector reports.
func intervalStats(id int64, calls int64, totalMs float64) collector.QueryStats {
	mean := 0.0
	if calls > 0 {
		mean = totalMs / float64(calls)
	}
	return collector.QueryStats{QueryID: id, Query: "SELECT * FROM orders WHERE id = $1",
		Calls: calls, TotalExecTime: totalMs, MeanExecTime: mean}
}

// intervalPair is two snapshots seconds apart in one statistics epoch.
func intervalPair(seconds float64, prev, cur []collector.QueryStats) (*collector.Snapshot,
	*collector.Snapshot) {
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	p := &collector.Snapshot{CollectedAt: at, StatsEpoch: intervalEpoch, Queries: prev}
	c := &collector.Snapshot{CollectedAt: at.Add(time.Duration(seconds * float64(time.Second))),
		StatsEpoch: intervalEpoch, Queries: cur}
	return c, p
}

func intervalConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Analyzer.SlowQueryThresholdMs = 1000
	cfg.Analyzer.RegressionThresholdPct = 50
	cfg.Analyzer.RegressionLookbackDays = 7
	return cfg
}

func detailFloat(t *testing.T, f Finding, key string) float64 {
	t.Helper()
	switch v := f.Detail[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	t.Fatalf("detail %q = %#v (%T), want a number", key, f.Detail[key], f.Detail[key])
	return 0
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// Happy path: 1M calls at 2 ms, then 10k calls at 2,000 ms. The
// cumulative mean (about 21.8 ms) is far below the 1,000 ms threshold;
// the interval mean is 2,000 ms.
func TestSlowQuery_JudgesTheIntervalMean(t *testing.T) {
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1_000_000, 2_000_000)},
		[]collector.QueryStats{intervalStats(1, 1_010_000, 2_000_000+10_000*2000)})
	got := ruleSlowQueries(cur, prev, intervalConfig(), nil)
	if len(got) != 1 || got[0].Category != "slow_query" ||
		got[0].ObjectIdentifier != "queryid:1" {
		t.Fatalf("findings = %+v, want one slow_query for queryid:1", got)
	}
	f := got[0]
	if !near(detailFloat(t, f, "interval_mean_ms"), 2000, 1e-6) ||
		detailFloat(t, f, "interval_calls") != 10_000 ||
		!near(detailFloat(t, f, "interval_seconds"), 60, 1e-9) {
		t.Fatalf("interval detail = %v", f.Detail)
	}
	if !near(detailFloat(t, f, "cumulative_mean_ms"), 22_000_000.0/1_010_000, 1e-6) {
		t.Fatalf("cumulative_mean_ms = %v", f.Detail["cumulative_mean_ms"])
	}
	if !strings.Contains(f.Title, "2000.0ms") {
		t.Fatalf("title must name the interval mean: %q", f.Title)
	}
}

// The cumulative mean alone no longer raises a finding: a query that was
// slow long ago and is fast in this interval is not slow now.
func TestSlowQuery_FastIntervalIsNotSlow(t *testing.T) {
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 100, 500_000)},
		[]collector.QueryStats{intervalStats(1, 200, 500_000+100*3)})
	if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 0 {
		t.Fatalf("findings = %+v, want none (interval mean 3 ms)", got)
	}
}

// Boundary: the call floor. 4 calls in the interval are not evidence, 5
// are (the default analyzer.slow_query_min_calls).
func TestSlowQuery_MinCallsBoundary(t *testing.T) {
	for _, tc := range []struct {
		calls int64
		want  int
	}{{4, 0}, {5, 1}} {
		cur, prev := intervalPair(600,
			[]collector.QueryStats{intervalStats(1, 1000, 1000)},
			[]collector.QueryStats{intervalStats(1, 1000+tc.calls,
				1000+float64(tc.calls)*1500)})
		if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != tc.want {
			t.Fatalf("%d interval calls: %d findings, want %d", tc.calls, len(got), tc.want)
		}
	}
	cfg := intervalConfig()
	cfg.Analyzer.SlowQueryMinCalls = 2
	cur, prev := intervalPair(600,
		[]collector.QueryStats{intervalStats(1, 10, 10)},
		[]collector.QueryStats{intervalStats(1, 12, 10+2*1500)})
	if got := ruleSlowQueries(cur, prev, cfg, nil); len(got) != 1 {
		t.Fatalf("configured floor 2, 2 calls: %d findings, want 1", len(got))
	}
}

// Boundary: critical needs ratio > 5 and at least 1% of wall time. 5
// calls at 6 s in 3,000 s are 1.00%; at 5.94 s they are 0.99%.
func TestSlowQuery_CriticalNeedsWallTimeShare(t *testing.T) {
	for _, tc := range []struct {
		totalMs float64
		want    string
	}{{30_000, "critical"}, {29_700, "warning"}} {
		cur, prev := intervalPair(3000,
			[]collector.QueryStats{intervalStats(1, 100, 100)},
			[]collector.QueryStats{intervalStats(1, 105, 100+tc.totalMs)})
		got := ruleSlowQueries(cur, prev, intervalConfig(), nil)
		if len(got) != 1 || got[0].Severity != tc.want {
			t.Fatalf("interval total %v ms: findings %+v, want one %s", tc.totalMs, got,
				tc.want)
		}
	}
	// Ratio exactly 5 stays a warning whatever the share.
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 100, 100)},
		[]collector.QueryStats{intervalStats(1, 110, 100+10*5000)})
	if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 1 ||
		got[0].Severity != "warning" {
		t.Fatalf("ratio 5: %+v, want one warning", got)
	}
}

// Noise: one ad hoc 6 s call is not a critical slow query (it is below the
// call floor, so not a finding at all).
func TestSlowQuery_SingleAdHocCallIsNotCritical(t *testing.T) {
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 0, 0)},
		[]collector.QueryStats{intervalStats(1, 1, 6000)})
	for _, f := range ruleSlowQueries(cur, prev, intervalConfig(), nil) {
		if f.Severity == "critical" {
			t.Fatalf("one 6 s call raised a critical finding: %+v", f)
		}
	}
	if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 0 {
		t.Fatalf("findings = %+v, want none below the call floor", got)
	}
}

// Zero threshold still falls back to the default (G2-B23), now on the
// interval mean.
func TestSlowQuery_ZeroThresholdUsesDefault(t *testing.T) {
	cfg := intervalConfig()
	cfg.Analyzer.SlowQueryThresholdMs = 0
	cfg.Analyzer.SlowQueryMinCalls = 0
	cur, prev := intervalPair(600,
		[]collector.QueryStats{intervalStats(1, 10, 10), intervalStats(2, 10, 10)},
		[]collector.QueryStats{intervalStats(1, 20, 10+10*5), intervalStats(2, 20,
			10+10*3000)})
	got := ruleSlowQueries(cur, prev, cfg, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "queryid:2" ||
		got[0].Severity != "warning" || strings.Contains(got[0].Title, "Inf") {
		t.Fatalf("findings = %+v, want only queryid:2 as a warning", got)
	}
}

// Invalid input: a counter that fell (eviction and re-entry) or a query
// absent from the previous snapshot is skipped; no negative mean results.
func TestIntervalMean_SkipsDecreasesAndNewQueries(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur collector.QueryStats
	}{
		{"calls fell", intervalStats(1, 1000, 9e6), intervalStats(1, 10, 9e6+50_000)},
		{"total fell", intervalStats(1, 1000, 9e6), intervalStats(1, 1010, 1e6)},
		{"no new calls", intervalStats(1, 1000, 9e6), intervalStats(1, 1000, 9e6)},
	}
	for _, tc := range cases {
		if iv, ok := intervalMean(tc.cur, tc.prev); ok {
			t.Fatalf("%s: interval %+v accepted, want skipped", tc.name, iv)
		}
		cur, prev := intervalPair(60, []collector.QueryStats{tc.prev},
			[]collector.QueryStats{tc.cur})
		if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 0 {
			t.Fatalf("%s: findings %+v, want none", tc.name, got)
		}
		if got := regressionCandidates(cur, prev, intervalConfig()); len(got) != 0 {
			t.Fatalf("%s: candidates %+v, want none", tc.name, got)
		}
	}
	iv, ok := intervalMean(intervalStats(1, 110, 1500), intervalStats(1, 100, 1000))
	if !ok || iv.Calls != 10 || iv.TotalMs != 500 || iv.MeanMs != 50 {
		t.Fatalf("valid interval = %+v, %v; want 10 calls, 500 ms, 50 ms mean", iv, ok)
	}
	cur, prev := intervalPair(60, nil, []collector.QueryStats{intervalStats(9, 100, 9e6)})
	if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 0 {
		t.Fatalf("query without a previous sample: %+v, want skipped", got)
	}
}

// Invalid input: the window is not measurable across a statistics epoch
// change, an unknown epoch, a reset, or time that did not advance.
func TestQueryInterval_WindowNeedsOneKnownEpoch(t *testing.T) {
	mk := func() (*collector.Snapshot, *collector.Snapshot) {
		return intervalPair(60, []collector.QueryStats{intervalStats(1, 10, 10)},
			[]collector.QueryStats{intervalStats(1, 20, 10+10*5000)})
	}
	cur, prev := mk()
	if !hasQueryInterval(cur, prev) {
		t.Fatal("fixture window must be measurable")
	}
	mutations := map[string]func(c, p *collector.Snapshot){
		"epoch changed": func(c, _ *collector.Snapshot) {
			c.StatsEpoch = c.StatsEpoch.Add(time.Hour)
		},
		"epoch unknown": func(c, p *collector.Snapshot) {
			c.StatsEpoch, p.StatsEpoch = time.Time{}, time.Time{}
		},
		"stats reset":      func(c, _ *collector.Snapshot) { c.StatsReset = true },
		"time not forward": func(c, p *collector.Snapshot) { c.CollectedAt = p.CollectedAt },
		"queries unavailable": func(c, _ *collector.Snapshot) {
			c.Unavailable = map[string]string{"queries": "permission denied"}
		},
	}
	for name, mutate := range mutations {
		cur, prev := mk()
		mutate(cur, prev)
		if hasQueryInterval(cur, prev) {
			t.Fatalf("%s: window accepted", name)
		}
		if got := ruleSlowQueries(cur, prev, intervalConfig(), nil); len(got) != 0 {
			t.Fatalf("%s: findings %+v, want none", name, got)
		}
	}
	if hasQueryInterval(cur, nil) || hasQueryInterval(nil, prev) {
		t.Fatal("a missing snapshot must not be a window")
	}
}

// Nil/empty: without a previous snapshot neither category is evaluated,
// so their open findings are not resolved this cycle.
func TestQueryRules_NoPreviousSnapshotNotEvaluated(t *testing.T) {
	a := New(nil, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur, _ := intervalPair(60, nil, []collector.QueryStats{intervalStats(1, 20, 9e6)})
	if got := a.runSnapshotRules(cur, nil, false); hasCategory(got, "slow_query") {
		t.Fatalf("slow_query emitted without a previous snapshot: %+v", got)
	}
	if got := a.runQueryRegression(context.Background(), cur, nil); len(got) != 0 {
		t.Fatalf("regression findings without a previous snapshot: %+v", got)
	}
	res := a.eval.resolvable(nil)
	if res["slow_query"] || res["query_regression"] {
		t.Fatalf("resolvable = %v; slow_query and query_regression must stay open", res)
	}
	if !a.eval.hasFailed("slow_query") || !a.eval.hasFailed("query_regression") {
		t.Fatal("both categories must be marked not evaluated")
	}
}

// hourly is n hourly baseline buckets of calls calls at meanMs each.
func hourly(n int, calls int64, meanMs float64) []querystore.Interval {
	out := make([]querystore.Interval, n)
	for i := range out {
		out[i] = querystore.Interval{Calls: calls, TotalMs: float64(calls) * meanMs}
	}
	return out
}

// Happy path (the plan's fixture): 1M calls at 2 ms, then 10k calls at
// 20 ms. The cumulative mean moves only to ~2.18 ms (+9%), which the old
// rule compared against a 50% threshold and missed; the interval mean
// (20 ms) against the median hourly baseline (2 ms) is a regression.
func TestQueryRegression_IntervalMeanAgainstMedianBaseline(t *testing.T) {
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1_000_000, 2_000_000)},
		[]collector.QueryStats{intervalStats(1, 1_010_000, 2_000_000+10_000*20)})
	cands := regressionCandidates(cur, prev, intervalConfig())
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v, want queryid 1", cands)
	}
	base, ok := baselineFromIntervals(hourly(168, 600, 2), 5)
	if !ok || base.MeanMs != 2 || base.Buckets != 168 {
		t.Fatalf("baseline = %+v, %v; want 2 ms over 168 buckets", base, ok)
	}
	got := ruleQueryRegression(cands, map[int64]regressionBaseline{1: base}, intervalConfig())
	if len(got) != 1 || got[0].Category != "query_regression" {
		t.Fatalf("findings = %+v, want one query_regression", got)
	}
	f := got[0]
	if !near(detailFloat(t, f, "interval_mean_ms"), 20, 1e-6) ||
		!near(detailFloat(t, f, "baseline_ms"), 2, 1e-9) ||
		f.Detail["baseline_method"] != "median_hourly_interval" ||
		detailFloat(t, f, "baseline_buckets") != 168 ||
		!near(detailFloat(t, f, "cumulative_mean_ms"), 2_200_000.0/1_010_000, 1e-9) {
		t.Fatalf("detail = %v", f.Detail)
	}
	if f.Severity != "critical" || !near(detailFloat(t, f, "pct_increase"), 900, 1e-6) {
		t.Fatalf("severity %s, pct %v; want critical at +900%%", f.Severity,
			f.Detail["pct_increase"])
	}
}

// Boundary: an interval mean at most baseline x (1 + pct/100) is not a
// regression; just above it is a warning.
func TestQueryRegression_ThresholdBoundary(t *testing.T) {
	cfg := intervalConfig()
	mk := func(mean float64) []Finding {
		cur, prev := intervalPair(60,
			[]collector.QueryStats{intervalStats(1, 1000, 1000)},
			[]collector.QueryStats{intervalStats(1, 1100, 1000+100*mean)})
		cands := regressionCandidates(cur, prev, cfg)
		return ruleQueryRegression(cands, map[int64]regressionBaseline{
			1: {MeanMs: 2, Buckets: 6}}, cfg)
	}
	if got := mk(3); len(got) != 0 {
		t.Fatalf("interval 3 ms vs baseline 2 ms at 50%%: %+v, want none", got)
	}
	if got := mk(3.2); len(got) != 1 || got[0].Severity != "warning" {
		t.Fatalf("interval 3.2 ms: %+v, want one warning", got)
	}
}

// Candidates: interval mean above cumulative x (1 + pct/100), with the
// call floor, at most 50, the largest interval total time first.
func TestRegressionCandidates_ThresholdFloorAndCap(t *testing.T) {
	var prevQ, curQ []collector.QueryStats
	for i := int64(1); i <= 60; i++ {
		prevQ = append(prevQ, intervalStats(i, 10_000, 10_000))
		curQ = append(curQ, intervalStats(i, 10_000+100, 10_000+100*(10+float64(i))))
	}
	prevQ = append(prevQ, intervalStats(100, 10_000, 10_000), intervalStats(101, 10_000, 10_000))
	curQ = append(curQ, intervalStats(100, 10_100, 10_000+100*1.4),
		intervalStats(101, 10_004, 10_000+4*500))
	cur, prev := intervalPair(60, prevQ, curQ)
	got := regressionCandidates(cur, prev, intervalConfig())
	if len(got) != maxRegressionCandidates || maxRegressionCandidates != 50 {
		t.Fatalf("candidates = %d, want the cap of 50", len(got))
	}
	for i, c := range got {
		if want := int64(60 - i); c.q.QueryID != want {
			t.Fatalf("candidate %d = query %d, want %d (largest interval total first)", i,
				c.q.QueryID, want)
		}
	}
}

// Nil/empty: fewer than 6 usable hourly buckets are no baseline, so no
// finding; buckets below the call floor or across an epoch break do not
// count; the median ignores a few regressed hours.
func TestRegressionBaseline_BucketsAndMedian(t *testing.T) {
	if b, ok := baselineFromIntervals(hourly(5, 600, 2), 5); ok {
		t.Fatalf("5 buckets gave baseline %+v, want none", b)
	}
	if b, ok := baselineFromIntervals(nil, 5); ok {
		t.Fatalf("no buckets gave baseline %+v", b)
	}
	ivs := append(hourly(5, 600, 2), querystore.Interval{Calls: 4, TotalMs: 8},
		querystore.Interval{Calls: 600, TotalMs: 1200, EpochBreak: true},
		querystore.Interval{Calls: 600, TotalMs: -5})
	if b, ok := baselineFromIntervals(ivs, 5); ok {
		t.Fatalf("5 usable of 8 buckets gave baseline %+v, want none", b)
	}
	ivs = append(hourly(6, 600, 2), hourly(2, 600, 20)...)
	b, ok := baselineFromIntervals(ivs, 5)
	if !ok || b.MeanMs != 2 || b.Buckets != 8 {
		t.Fatalf("baseline = %+v, %v; want the median 2 ms of 8 buckets", b, ok)
	}
	even := append(hourly(3, 100, 2), hourly(3, 100, 4)...)
	if b, ok := baselineFromIntervals(even, 5); !ok || b.MeanMs != 3 {
		t.Fatalf("even median = %+v, %v; want 3 ms", b, ok)
	}
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1000, 1000)},
		[]collector.QueryStats{intervalStats(1, 1100, 1000+100*50)})
	cands := regressionCandidates(cur, prev, intervalConfig())
	if got := ruleQueryRegression(cands, map[int64]regressionBaseline{},
		intervalConfig()); len(got) != 0 {
		t.Fatalf("no baseline: %+v, want no finding", got)
	}
}

// Error propagation: a failed baseline read leaves query_regression out of
// the evaluated set (its open findings are not resolved) and logs what
// failed.
func TestRunQueryRegression_ReadErrorFailsCategory(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var logged []string
	logFn := func(level, format string, args ...any) {
		logged = append(logged, level+" "+format)
	}
	a := New(pool, intervalConfig(), nil, nil, nil, nil, nil, logFn)
	a.eval = newCycleEval()
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1000, 1000)},
		[]collector.QueryStats{intervalStats(1, 1100, 1000+100*50)})
	if got := a.runQueryRegression(context.Background(), cur, prev); len(got) != 0 {
		t.Fatalf("findings on a failed read: %+v", got)
	}
	a.eval.evaluated("query_regression")
	if a.eval.resolvable(nil)["query_regression"] {
		t.Fatal("query_regression resolvable after a failed baseline read")
	}
	if len(logged) == 0 || !strings.HasPrefix(logged[0], "ERROR") ||
		!strings.Contains(logged[0], "baseline") {
		t.Fatalf("logs = %q, want an ERROR naming the baseline read", logged)
	}
}

// No candidate, no read: a cycle where nothing regressed against its
// cumulative mean does not touch the history store and stays evaluated.
func TestRunQueryRegression_NoCandidateNoRead(t *testing.T) {
	a := New(nil, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur, prev := intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1000, 2000)},
		[]collector.QueryStats{intervalStats(1, 1100, 2000+100*2)})
	if got := a.runQueryRegression(context.Background(), cur, prev); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
	if a.eval.hasFailed("query_regression") {
		t.Fatal("no candidate must not read (a nil pool read would fail)")
	}
}

// Defaults: with no configuration the floor is 5, and 0 means the default.
func TestSlowQueryMinCalls_Default(t *testing.T) {
	if got := config.DefaultConfig().Analyzer.SlowQueryMinCalls; got != 5 {
		t.Fatalf("default slow_query_min_calls = %d, want 5", got)
	}
	cfg := intervalConfig()
	cfg.Analyzer.SlowQueryMinCalls = 0
	if got := slowQueryMinCalls(cfg); got != 5 {
		t.Fatalf("0 floor = %d, want the default 5", got)
	}
	cfg.Analyzer.SlowQueryMinCalls = 12
	if got := slowQueryMinCalls(cfg); got != 12 {
		t.Fatalf("configured floor = %d, want 12", got)
	}
}
