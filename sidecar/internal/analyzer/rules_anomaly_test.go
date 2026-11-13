package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-02: workload_anomaly scores each cycle's workload against the
// same hour of the week in earlier weeks (robust z over the bucket's
// weeks). A weekly peak is usual; the same value at an off-peak hour is
// an anomaly after two consecutive cycles.

type fakeBucket struct {
	db     string
	key    baseline.Key
	bucket int
}

// fakeBaselines is an in-memory BaselineStore: History returns the
// seeded weeks of the bucket of at; Accumulate records the samples.
type fakeBaselines struct {
	mu          sync.Mutex
	hist        map[fakeBucket][]float64
	accumulated [][]baseline.Sample
	historyErr  error
	reads       int
}

func newFakeBaselines() *fakeBaselines {
	return &fakeBaselines{hist: map[fakeBucket][]float64{}}
}

func (f *fakeBaselines) seed(db string, k baseline.Key, bucket int, weeks ...float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hist[fakeBucket{db, k, bucket}] = weeks
}

func (f *fakeBaselines) History(_ context.Context, db string, w baseline.Window,
	at time.Time, keys []baseline.Key) (map[baseline.Key][]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	out := map[baseline.Key][]float64{}
	for _, k := range keys {
		if h, ok := f.hist[fakeBucket{db, k, w.Bucket(at)}]; ok {
			out[k] = h
		}
	}
	return out, nil
}

func (f *fakeBaselines) Accumulate(_ context.Context, _ string, _ baseline.Window,
	_ time.Time, samples []baseline.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accumulated = append(f.accumulated, samples)
	return nil
}

func (f *fakeBaselines) lastSamples() []baseline.Sample {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.accumulated) == 0 {
		return nil
	}
	return f.accumulated[len(f.accumulated)-1]
}

// qspec is one statement's interval: calls at meanMs each.
type qspec struct {
	id     int64
	meanMs float64
	calls  int64
}

var anomalyEpoch = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// snapPair is a previous and current snapshot 60 s apart ending at at.
func snapPair(at time.Time, qs ...qspec) (cur, prev *collector.Snapshot) {
	prev = &collector.Snapshot{CollectedAt: at.Add(-time.Minute), StatsEpoch: anomalyEpoch,
		System: collector.SystemStats{ActiveBackends: 3, TotalBackends: 10}}
	cur = &collector.Snapshot{CollectedAt: at, StatsEpoch: anomalyEpoch,
		System: collector.SystemStats{ActiveBackends: 3, TotalBackends: 10}}
	for _, q := range qs {
		text := "SELECT * FROM orders WHERE id = $1"
		prev.Queries = append(prev.Queries, collector.QueryStats{QueryID: q.id, Query: text,
			Calls: 10_000, TotalExecTime: 10_000, MeanExecTime: 1})
		total := 10_000 + float64(q.calls)*q.meanMs
		cur.Queries = append(cur.Queries, collector.QueryStats{QueryID: q.id, Query: text,
			Calls: 10_000 + q.calls, TotalExecTime: total,
			MeanExecTime: total / float64(10_000+q.calls)})
	}
	return cur, prev
}

func anomalyAnalyzer(t *testing.T, store BaselineStore) *Analyzer {
	t.Helper()
	cfg := config.DefaultConfig()
	a := New(nil, cfg, nil, nil, nil, nil, nil, noopLog)
	a.WithDatabaseName("orders")
	a.WithBaselineStore(store)
	return a
}

// runAnomalyCycle runs workload_anomaly for one cycle on a fresh cycleEval.
func runAnomalyCycle(a *Analyzer, cur, prev *collector.Snapshot) []Finding {
	a.eval = newCycleEval()
	return a.runWorkloadAnomaly(context.Background(), cur, prev)
}

func qkey(id int64) baseline.Key {
	return baseline.Key{Metric: MetricQueryMean, Key: queryIdent(id)}
}

// The weekly-peak fixture (CHECK-02): 4 weeks of a 10x peak on Monday at
// 09:00. Two peak cycles on Monday give nothing; the same value on
// Tuesday at 09:00 gives one warning, on the second cycle. A heavy
// statement keeps the report's share of DB time under 10%.
const reportQ, bulkQ = int64(101), int64(102)

func weeklyPeakStore() *fakeBaselines {
	f := newFakeBaselines()
	for _, bucket := range []int{9, 33} { // Monday and Tuesday 09:00 UTC
		f.seed("orders", qkey(bulkQ), bucket, 1000, 1000, 1000, 1000)
	}
	f.seed("orders", qkey(reportQ), 9, 50, 52, 48, 50) // the Monday peak
	f.seed("orders", qkey(reportQ), 33, 5, 5, 6, 4)    // Tuesday is quiet
	return f
}

var (
	monday0900  = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	tuesday0900 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
)

func peakCycle(a *Analyzer, at time.Time) []Finding {
	cur, prev := snapPair(at, qspec{reportQ, 50, 500}, qspec{bulkQ, 1000, 500})
	return runAnomalyCycle(a, cur, prev)
}

func TestWorkloadAnomaly_WeeklyPeakFixture(t *testing.T) {
	a := anomalyAnalyzer(t, weeklyPeakStore())
	for i := 0; i < 2; i++ {
		if got := peakCycle(a, monday0900.Add(time.Duration(i)*time.Minute)); len(got) != 0 {
			t.Fatalf("Monday peak cycle %d = %+v, want no finding", i, got)
		}
	}
	if got := peakCycle(a, tuesday0900); len(got) != 0 {
		t.Fatalf("first Tuesday cycle = %+v, want none (one cycle is not enough)", got)
	}
	got := peakCycle(a, tuesday0900.Add(time.Minute))
	if len(got) != 1 {
		t.Fatalf("second Tuesday cycle = %+v, want one finding", got)
	}
	f := got[0]
	if f.Category != CategoryWorkloadAnomaly || f.Severity != "warning" ||
		f.ObjectType != "workload" || f.ObjectIdentifier != "query_mean_ms:queryid:101" {
		t.Fatalf("finding = %s/%s %s %q, want a warning on query_mean_ms:queryid:101",
			f.Category, f.Severity, f.ObjectType, f.ObjectIdentifier)
	}
	d := f.Detail
	if d["metric"] != MetricQueryMean || d["key"] != "queryid:101" ||
		d["method"] != baseline.Method || d["weeks"] != 4 || d["baseline_median"] != 5.0 ||
		d["observed"] != 50.0 || d["how"] != "interval_mean" || d["queryid"] != reportQ {
		t.Fatalf("detail = %v, want metric, key, method, 4 weeks, median 5, observed 50", d)
	}
	if z, _ := d["z"].(float64); z < 6 || math.IsInf(z, 0) {
		t.Fatalf("z = %v, want a finite z above 6 (warning only by DB-time share)", d["z"])
	}
	if share, _ := d["db_time_share"].(float64); share >= 0.10 || share <= 0 {
		t.Fatalf("db_time_share = %v, want under 10%%", d["db_time_share"])
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("detail is not JSON: %v", err)
	}
}

// Boundary: z 3.99 is nothing, 4.0 a warning, 6 a critical only when the
// statement holds at least 10% of the interval's DB time; other metrics
// are critical at z 6.
func TestAnomalyLevel_ZBoundaries(t *testing.T) {
	cfg := config.DefaultBaseline()
	score := func(z float64) baseline.Score {
		return baseline.Score{Observed: 100, Median: 10, Z: z, Weeks: 4}
	}
	cases := []struct {
		name  string
		z     float64
		share float64
		query bool
		want  string
	}{
		{"3.99", 3.99, 1, true, ""},
		{"4.0", 4.0, 1, true, "warning"},
		{"6 with 10% share", 6, 0.10, true, "critical"},
		{"6 with 9.9% share", 6, 0.099, true, "warning"},
		{"5.99 with full share", 5.99, 1, true, "warning"},
		{"6 on backends", 6, 0, false, "critical"},
	}
	for _, c := range cases {
		if got := anomalyLevel(score(c.z), 5, c.share, c.query, cfg); got != c.want {
			t.Errorf("%s: level %q, want %q", c.name, got, c.want)
		}
	}
	learning := baseline.Score{Observed: 100, Median: 10, Z: 50, Weeks: 2}
	if got := anomalyLevel(learning, 5, 1, true, cfg); got != "" {
		t.Fatalf("learning bucket level %q, want none", got)
	}
	small := baseline.Score{Observed: 14, Median: 10, Z: 50, Weeks: 4}
	if got := anomalyLevel(small, 5, 1, true, cfg); got != "" {
		t.Fatalf("delta under min_delta_ms level %q, want none", got)
	}
}

// Boundary: a bucket with 2 weeks of data is learning and never fires,
// whatever the value.
func TestWorkloadAnomaly_TwoWeeksIsLearning(t *testing.T) {
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5)
	a := anomalyAnalyzer(t, f)
	for i := 0; i < 3; i++ {
		cur, prev := snapPair(tuesday0900.Add(time.Duration(i)*time.Minute),
			qspec{reportQ, 500, 500})
		if got := runAnomalyCycle(a, cur, prev); len(got) != 0 {
			t.Fatalf("learning cycle %d = %+v, want none", i, got)
		}
	}
	if len(a.seasonal) != 0 {
		t.Fatalf("seasonal = %v, want no active query bucket", a.seasonal)
	}
}

// Nil/empty: no statements and no history give no finding, the category
// is evaluated and the gauges are still accumulated.
func TestWorkloadAnomaly_NoSamplesNoHistory(t *testing.T) {
	f := newFakeBaselines()
	a := anomalyAnalyzer(t, f)
	cur, prev := snapPair(tuesday0900)
	if got := runAnomalyCycle(a, cur, prev); len(got) != 0 {
		t.Fatalf("empty cycle = %+v, want none", got)
	}
	if a.eval.hasFailed(CategoryWorkloadAnomaly) ||
		!a.eval.resolvable(nil)[CategoryWorkloadAnomaly] {
		t.Fatal("empty cycle did not evaluate workload_anomaly")
	}
	got := map[string]float64{}
	for _, s := range f.lastSamples() {
		got[s.Key.Metric+"/"+s.Key.Key] = s.Value
	}
	if got["active_backends/database"] != 3 || got["total_backends/database"] != 10 {
		t.Fatalf("accumulated = %v, want the backend gauges", got)
	}
	for k := range got {
		if len(k) > 13 && k[:13] == MetricQueryMean {
			t.Fatalf("accumulated a query sample %s without statements", k)
		}
	}
}

// MAD = 0 on a constant series: the floor applies and z is finite.
func TestWorkloadAnomaly_ConstantSeriesIsFinite(t *testing.T) {
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5, 5, 5)
	a := anomalyAnalyzer(t, f)
	var got []Finding
	for i := 0; i < 2; i++ {
		cur, prev := snapPair(tuesday0900.Add(time.Duration(i)*time.Minute),
			qspec{reportQ, 50, 500})
		got = runAnomalyCycle(a, cur, prev)
	}
	if len(got) != 1 || got[0].Detail["mad"] != 0.0 {
		t.Fatalf("findings = %+v, want one with MAD 0", got)
	}
	z := got[0].Detail["z"].(float64)
	if math.IsInf(z, 0) || math.IsNaN(z) || got[0].Severity != "critical" {
		t.Fatalf("z = %v severity %s, want finite and critical (sole statement)", z,
			got[0].Severity)
	}
}

// Invalid input: a pg_stat_statements reset (calls decrease) and a moved
// statistics epoch add no statement to the bucket.
func TestWorkloadAnomaly_StatsResetIsExcludedFromTheBucket(t *testing.T) {
	f := newFakeBaselines()
	a := anomalyAnalyzer(t, f)
	cur, prev := snapPair(tuesday0900, qspec{reportQ, 50, 500})
	cur.Queries[0].Calls = 3 // reset: fewer calls than before
	cur.Queries[0].TotalExecTime = 150
	runAnomalyCycle(a, cur, prev)
	for _, s := range f.lastSamples() {
		if s.Key.Metric == MetricQueryMean {
			t.Fatalf("reset statement accumulated: %+v", s)
		}
	}
	cur, prev = snapPair(tuesday0900, qspec{reportQ, 50, 500})
	cur.StatsEpoch = anomalyEpoch.Add(time.Hour)
	runAnomalyCycle(a, cur, prev)
	for _, s := range f.lastSamples() {
		if s.Key.Metric == MetricQueryMean {
			t.Fatalf("statement of a moved epoch accumulated: %+v", s)
		}
	}
}

// Error propagation: a failed baseline read marks workload_anomaly failed
// (evalFail), so its open findings stay open, and accumulates nothing.
func TestWorkloadAnomaly_FailedReadKeepsOpenFindings(t *testing.T) {
	f := weeklyPeakStore()
	a := anomalyAnalyzer(t, f)
	peakCycle(a, tuesday0900)
	if got := peakCycle(a, tuesday0900.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("setup: %d findings, want 1", len(got))
	}
	before := len(f.accumulated)
	f.historyErr = errors.New("connection refused")
	got := peakCycle(a, tuesday0900.Add(2*time.Minute))
	if len(got) != 0 || !a.eval.hasFailed(CategoryWorkloadAnomaly) {
		t.Fatalf("failed read = %+v failed=%v, want none and evalFail", got,
			a.eval.hasFailed(CategoryWorkloadAnomaly))
	}
	if a.eval.resolvable(nil)[CategoryWorkloadAnomaly] {
		t.Fatal("a failed read made workload_anomaly resolvable")
	}
	if len(f.accumulated) != before {
		t.Fatal("a failed read still accumulated samples")
	}
}

// Nil/zero inputs: without a store or a previous snapshot the category
// is not evaluated; disabled, it is evaluated (open findings resolve)
// and nothing is read.
func TestWorkloadAnomaly_NoStoreNoPreviousDisabled(t *testing.T) {
	a := anomalyAnalyzer(t, nil)
	cur, prev := snapPair(tuesday0900, qspec{reportQ, 50, 500})
	if got := runAnomalyCycle(a, cur, prev); got != nil ||
		!a.eval.hasFailed(CategoryWorkloadAnomaly) {
		t.Fatal("no store: want no findings and a failed category")
	}
	f := weeklyPeakStore()
	a = anomalyAnalyzer(t, f)
	if got := runAnomalyCycle(a, cur, nil); got != nil ||
		!a.eval.hasFailed(CategoryWorkloadAnomaly) {
		t.Fatal("no previous snapshot: want no findings and a failed category")
	}
	a.cfg.Baseline.Enabled = false
	if got := runAnomalyCycle(a, cur, prev); got != nil ||
		a.eval.hasFailed(CategoryWorkloadAnomaly) || f.reads != 0 {
		t.Fatalf("disabled: findings %v failed %v reads %d, want an evaluated no-op",
			got, a.eval.hasFailed(CategoryWorkloadAnomaly), f.reads)
	}
}

// State transitions: learning -> active -> open -> resolved after 2
// normal cycles. The first normal cycle keeps the finding open; the
// episode start is the first anomalous cycle's time.
func TestWorkloadAnomaly_LearningActiveOpenResolved(t *testing.T) {
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5)
	a := anomalyAnalyzer(t, f)
	ident := "query_mean_ms:queryid:101"
	at := tuesday0900
	cycle := func(mean float64) []Finding {
		cur, prev := snapPair(at, qspec{reportQ, mean, 500})
		at = at.Add(time.Minute)
		return runAnomalyCycle(a, cur, prev)
	}
	if got := cycle(50); len(got) != 0 {
		t.Fatalf("learning: %+v", got)
	}
	f.seed("orders", qkey(reportQ), 33, 5, 5, 5) // a third week: active
	episode := at
	if got := cycle(50); len(got) != 0 {
		t.Fatalf("first anomalous cycle: %+v", got)
	}
	open := cycle(50)
	if len(open) != 1 || open[0].Detail["episode_start"] != episode.Format(time.RFC3339) {
		t.Fatalf("open = %+v, want one finding with episode_start %s", open, episode)
	}
	if got := cycle(50); len(got) != 1 ||
		got[0].Detail["episode_start"] != episode.Format(time.RFC3339) {
		t.Fatalf("still anomalous = %+v, want the same episode", got)
	}
	if got := cycle(5); len(got) != 0 || !a.eval.kept(CategoryWorkloadAnomaly)[ident] {
		t.Fatalf("first normal cycle = %+v kept=%v, want none and the finding kept open",
			got, a.eval.kept(CategoryWorkloadAnomaly))
	}
	if got := cycle(5); len(got) != 0 || a.eval.kept(CategoryWorkloadAnomaly)[ident] {
		t.Fatal("second normal cycle still keeps the finding open")
	}
	if !a.eval.resolvable(nil)[CategoryWorkloadAnomaly] {
		t.Fatal("second normal cycle: workload_anomaly is not resolvable")
	}
	// A new anomaly is a new episode.
	cycle(50)
	next := at
	if got := cycle(50); len(got) != 1 ||
		got[0].Detail["episode_start"] != next.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("new episode = %+v, want episode_start %s", got, next.Add(-time.Minute))
	}
}

// A statement idle this cycle (no measured interval) has no verdict: its
// open finding is kept, not resolved.
func TestWorkloadAnomaly_IdleStatementKeepsItsFinding(t *testing.T) {
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5, 5)
	a := anomalyAnalyzer(t, f)
	for i := 0; i < 2; i++ {
		cur, prev := snapPair(tuesday0900.Add(time.Duration(i)*time.Minute),
			qspec{reportQ, 50, 500})
		runAnomalyCycle(a, cur, prev)
	}
	cur, prev := snapPair(tuesday0900.Add(2*time.Minute), qspec{reportQ, 50, 0})
	if got := runAnomalyCycle(a, cur, prev); len(got) != 0 ||
		!a.eval.kept(CategoryWorkloadAnomaly)["query_mean_ms:queryid:101"] {
		t.Fatal("idle statement: want no finding and the open one kept")
	}
}
