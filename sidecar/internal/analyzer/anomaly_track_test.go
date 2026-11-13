package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-02 review: the hysteresis is in memory, so it must not resolve
// an open finding it has no evidence against.

// After a restart the tracker is empty while sage.findings still holds
// the open anomaly. Its first anomalous cycle opens nothing yet (one
// cycle is not enough), but it must keep the open finding: an anomalous
// cycle is no evidence the anomaly ended.
func TestWorkloadAnomaly_FirstAnomalousCycleKeepsAnOpenFinding(t *testing.T) {
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5, 5)
	a := anomalyAnalyzer(t, f) // a restarted sidecar: no tracker state
	ident := "query_mean_ms:queryid:101"
	cur, prev := snapPair(tuesday0900, qspec{reportQ, 50, 500})
	if got := runAnomalyCycle(a, cur, prev); len(got) != 0 {
		t.Fatalf("first anomalous cycle = %+v, want no finding yet", got)
	}
	if !a.eval.kept(CategoryWorkloadAnomaly)[ident] {
		t.Fatalf("first anomalous cycle kept %v, want %s kept open",
			a.eval.kept(CategoryWorkloadAnomaly), ident)
	}
	cur, prev = snapPair(tuesday0900.Add(time.Minute), qspec{reportQ, 50, 500})
	if got := runAnomalyCycle(a, cur, prev); len(got) != 1 ||
		got[0].ObjectIdentifier != ident {
		t.Fatalf("second anomalous cycle = %+v, want the anomaly", got)
	}
	// A normal cycle with no open episode keeps nothing.
	b := anomalyAnalyzer(t, f)
	cur, prev = snapPair(tuesday0900, qspec{reportQ, 5, 500})
	if got := runAnomalyCycle(b, cur, prev); len(got) != 0 ||
		b.eval.kept(CategoryWorkloadAnomaly)[ident] {
		t.Fatalf("normal cycle = %+v kept %v, want none", got,
			b.eval.kept(CategoryWorkloadAnomaly))
	}
}

// openReportAnomaly runs two anomalous cycles of the report statement,
// next to a steady statement, so its finding is open.
func openReportAnomaly(t *testing.T) *Analyzer {
	t.Helper()
	f := newFakeBaselines()
	f.seed("orders", qkey(reportQ), 33, 5, 5, 5)
	a := anomalyAnalyzer(t, f)
	for i := 0; i < 2; i++ {
		cur, prev := snapPair(tuesday0900.Add(time.Duration(i)*time.Minute),
			qspec{reportQ, 50, 500}, qspec{bulkQ, 1000, 500})
		if got := runAnomalyCycle(a, cur, prev); i == 1 && len(got) != 1 {
			t.Fatalf("setup: cycle %d = %+v, want the open anomaly", i, got)
		}
	}
	return a
}

// A statement gone from pg_stat_statements (evicted, or no longer
// collected) can never be judged again: its open finding resolves rather
// than staying open until the next restart, and the tracker forgets it.
func TestWorkloadAnomaly_StatementGoneFromTheSnapshotResolves(t *testing.T) {
	a := openReportAnomaly(t)
	ident := "query_mean_ms:queryid:101"
	cur, prev := snapPair(tuesday0900.Add(2*time.Minute), qspec{bulkQ, 1000, 500})
	if got := runAnomalyCycle(a, cur, prev); len(got) != 0 {
		t.Fatalf("gone statement = %+v, want no finding", got)
	}
	if a.eval.kept(CategoryWorkloadAnomaly)[ident] {
		t.Fatal("a statement gone from the snapshot keeps its finding open forever")
	}
	if !a.eval.resolvable(nil)[CategoryWorkloadAnomaly] {
		t.Fatal("workload_anomaly is not resolvable")
	}
	if _, tracked := a.anomalies[ident]; tracked {
		t.Fatal("the tracker still holds the gone statement")
	}
}

// Without a statement interval this cycle (the queries category was not
// read, or a stats reset) absence is no evidence: the finding is kept.
func TestWorkloadAnomaly_UnreadStatementsKeepTheFinding(t *testing.T) {
	for name, mutate := range map[string]func(cur *collector.Snapshot){
		"queries unavailable": func(cur *collector.Snapshot) {
			cur.Unavailable = map[string]string{"queries": "permission denied"}
		},
		"stats reset": func(cur *collector.Snapshot) { cur.StatsReset = true },
	} {
		t.Run(name, func(t *testing.T) {
			a := openReportAnomaly(t)
			cur, prev := snapPair(tuesday0900.Add(2 * time.Minute))
			mutate(cur)
			if got := runAnomalyCycle(a, cur, prev); len(got) != 0 ||
				!a.eval.kept(CategoryWorkloadAnomaly)["query_mean_ms:queryid:101"] {
				t.Fatalf("%s: got %+v, want the open finding kept", name, got)
			}
		})
	}
}

// Boundary: a statement needs the rules-05 call floor (Δcalls >= k) in
// the interval before its mean is a sample; one call under it adds
// nothing to the bucket, so a rarely run statement cannot alarm.
func TestWorkloadSamples_StatementCallFloor(t *testing.T) {
	const k = 5
	cur, prev := snapPair(tuesday0900, qspec{1, 40, k - 1}, qspec{2, 40, k})
	var got []string
	for _, s := range workloadSamples(cur, prev, k, 500).samples {
		if s.Key.Metric == MetricQueryMean {
			got = append(got, s.Key.Key)
		}
	}
	if len(got) != 1 || got[0] != "queryid:2" {
		t.Fatalf("sampled statements = %v, want only queryid:2 (at the floor of %d calls)",
			got, k)
	}
}
