package analyzer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-02: the workload_anomaly samples, concurrency, and the
// hour-of-week baseline query_regression uses. Fixtures are in
// rules_anomaly_test.go.

// Backends, WAL rate and lock-graph wait classes are metrics too.
func TestWorkloadSamples_GaugesWALAndWaitClasses(t *testing.T) {
	cur, prev := snapPair(tuesday0900)
	wal := int64(60 << 20)
	cur.System.Interval = &collector.SystemInterval{Seconds: 60, WALBytes: &wal,
		WALSeconds: 60}
	lock, lw := "Lock", "LWLock"
	cur.Locks = []collector.LockInfo{{PID: 1, WaitEventType: &lock},
		{PID: 1, WaitEventType: &lock}, {PID: 2, WaitEventType: &lock},
		{PID: 3, WaitEventType: &lw}, {PID: 4}}
	got := map[string]float64{}
	for _, s := range workloadSamples(cur, prev, 5, 500).samples {
		got[s.Key.Metric+"/"+s.Key.Key] = s.Value
	}
	want := map[string]float64{"wal_bytes_per_s/cluster": 1 << 20,
		"wait_sessions/Lock": 2, "wait_sessions/LWLock": 1, "wait_sessions/IO": 0,
		"active_backends/database": 3, "total_backends/database": 10}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	cur.Unavailable = map[string]string{"locks": "denied", "system": "denied"}
	for _, s := range workloadSamples(cur, prev, 5, 500).samples {
		if s.Key.Metric == MetricWaitSessions || s.Key.Metric == MetricActiveBackends {
			t.Fatalf("unread category sampled: %+v", s)
		}
	}
}

// Boundary: the statement keys are capped at max_query_keys, the largest
// interval time first; 0 samples no statement.
func TestWorkloadSamples_CapsQueryKeys(t *testing.T) {
	cur, prev := snapPair(tuesday0900, qspec{1, 1, 100}, qspec{2, 10, 100},
		qspec{3, 5, 100})
	ids := func(maxKeys int) []string {
		var out []string
		for _, s := range workloadSamples(cur, prev, 5, maxKeys).samples {
			if s.Key.Metric == MetricQueryMean {
				out = append(out, s.Key.Key)
			}
		}
		return out
	}
	if got := ids(2); len(got) != 2 || got[0] != "queryid:2" || got[1] != "queryid:3" {
		t.Fatalf("capped keys = %v, want queryid:2 and queryid:3", got)
	}
	if got := ids(0); len(got) != 0 {
		t.Fatalf("max_query_keys 0 = %v, want none", got)
	}
}

// Concurrent access: accumulation into the store from other goroutines
// while the analyzer scores (CLAUDE.md asks for -race, unavailable here
// without cgo; the store's own concurrency runs on PostgreSQL in
// internal/baseline).
func TestWorkloadAnomaly_ScoresWhileOthersAccumulate(t *testing.T) {
	f := weeklyPeakStore()
	a := anomalyAnalyzer(t, f)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = f.Accumulate(context.Background(), "orders", baseline.Window{},
					tuesday0900, []baseline.Sample{{Key: qkey(9), Value: 1}})
			}
		}()
	}
	var found int
	for i := 0; i < 20; i++ {
		found += len(peakCycle(a, tuesday0900.Add(time.Duration(i)*time.Minute)))
	}
	wg.Wait()
	if found != 19 || len(f.accumulated) != 200+20 {
		t.Fatalf("found %d, accumulated %d, want 19 findings and 220 writes", found,
			len(f.accumulated))
	}
}

// query_regression judges against the bucket median when the bucket is
// active (baseline_method hour_of_week) and keeps the flat baseline
// otherwise: the Monday peak is not a regression.
func TestQueryRegression_HourOfWeekBucketWhenActive(t *testing.T) {
	cfg := config.DefaultConfig()
	cand := regressionCandidate{q: collector.QueryStats{QueryID: reportQ, MeanExecTime: 10},
		interval: queryInterval{Calls: 500, TotalMs: 25_000, MeanMs: 50}}
	flat := map[int64]regressionBaseline{reportQ: {MeanMs: 5, Buckets: 100}}
	peak := map[int64]baseline.Score{reportQ: {Median: 50, Weeks: 4}}
	got := ruleQueryRegression([]regressionCandidate{cand}, seasonalBaselines(flat, peak), cfg)
	if len(got) != 0 {
		t.Fatalf("Monday peak judged on its bucket = %+v, want no regression", got)
	}
	quiet := map[int64]baseline.Score{reportQ: {Median: 6, Weeks: 4}}
	got = ruleQueryRegression([]regressionCandidate{cand}, seasonalBaselines(flat, quiet), cfg)
	if len(got) != 1 || got[0].Detail["baseline_method"] != regressionMethodHourOfWeek ||
		got[0].Detail["baseline_ms"] != 6.0 || got[0].Detail["baseline_buckets"] != 4 {
		t.Fatalf("quiet bucket = %+v, want a regression against the 6 ms bucket", got)
	}
	learning := map[int64]baseline.Score{reportQ: {Median: 50, Weeks: 2}}
	got = ruleQueryRegression([]regressionCandidate{cand},
		seasonalBaselines(flat, learning), cfg)
	if len(got) != 1 || got[0].Detail["baseline_method"] != regressionMethodFlat ||
		got[0].Detail["baseline_ms"] != 5.0 {
		t.Fatalf("learning bucket = %+v, want the flat baseline", got)
	}
	if got := seasonalBaselines(nil, quiet); got[reportQ].MeanMs != 6 {
		t.Fatalf("seasonal without a flat baseline = %v, want the bucket", got)
	}
}

// runWorkloadAnomaly records the active statement buckets for
// query_regression.
func TestWorkloadAnomaly_RecordsActiveQueryBuckets(t *testing.T) {
	a := anomalyAnalyzer(t, weeklyPeakStore())
	peakCycle(a, monday0900)
	s, ok := a.seasonal[reportQ]
	if !ok || s.Median != 50 || s.Weeks != 4 {
		t.Fatalf("seasonal[%d] = %+v (%v), want the Monday median 50 over 4 weeks",
			reportQ, s, ok)
	}
}
