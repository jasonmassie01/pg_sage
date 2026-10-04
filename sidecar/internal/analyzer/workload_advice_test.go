package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// Dogfood round 2 (lifeos 2026-10-04): the briefing told the operator to
// "investigate" an untagged EXPLAIN ANALYZE of an old pg_sage probe.
// Diagnostic and maintenance statements are not workload: no query
// advice rule may name them, while rules about capacity still count them.

const diagExplain = "EXPLAIN (ANALYZE, BUFFERS, SUMMARY ON) WITH s AS (SELECT 1) " +
	"SELECT * FROM s"

func adviceSnapshot(at time.Time, scale int64) *collector.Snapshot {
	mk := func(id int64, query string) collector.QueryStats {
		return collector.QueryStats{QueryID: id, Query: query, Calls: 20000 * scale,
			TotalExecTime: 5e6 * float64(scale), MeanExecTime: 2352,
			MeanPlanTime: 9000}
	}
	return &collector.Snapshot{CollectedAt: at, Queries: []collector.QueryStats{
		mk(1, "SELECT * FROM orders WHERE id = $1"),
		mk(-3534472208683223210, diagExplain),
		mk(3, "VACUUM public.orders"),
		mk(4, "COPY public.orders (id) TO stdout"),
		mk(5, "CREATE INDEX IF NOT EXISTS ix ON orders (a)"),
		mk(6, "SELECT pg_stat_statements_reset()"),
	}}
}

func adviceConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Analyzer.SlowQueryThresholdMs = 1000
	cfg.Analyzer.RegressionThresholdPct = 50
	return cfg
}

func assertOnlyWorkloadQuery(t *testing.T, rule string, findings []Finding) {
	t.Helper()
	if len(findings) == 0 {
		t.Fatalf("%s: no finding at all; the application query must still be flagged",
			rule)
	}
	for _, f := range findings {
		if f.ObjectIdentifier != "queryid:1" {
			t.Errorf("%s flagged %s (%v): diagnostic statements are not workload",
				rule, f.ObjectIdentifier, f.Detail["query"])
		}
	}
}

func TestSlowQueriesIgnoresDiagnosticStatements(t *testing.T) {
	cur := adviceSnapshot(time.Now(), 1)
	assertOnlyWorkloadQuery(t, "slow_queries", ruleSlowQueries(cur, nil, adviceConfig(), nil))
}

func TestHighPlanTimeIgnoresDiagnosticStatements(t *testing.T) {
	cur := adviceSnapshot(time.Now(), 1)
	for i := range cur.Queries {
		cur.Queries[i].MeanExecTime = 10
	}
	assertOnlyWorkloadQuery(t, "high_plan_time",
		ruleHighPlanTime(cur, nil, adviceConfig(), nil))
}

func TestQueryRegressionIgnoresDiagnosticStatements(t *testing.T) {
	now := time.Now()
	prev, cur := adviceSnapshot(now.Add(-time.Minute), 1), adviceSnapshot(now, 2)
	hist := map[int64]float64{}
	for _, q := range cur.Queries {
		hist[q.QueryID] = 10
	}
	assertOnlyWorkloadQuery(t, "query_regression",
		ruleQueryRegression(cur, prev, hist, adviceConfig()))
}

func TestTotalTimeHeavyIgnoresDiagnosticStatements(t *testing.T) {
	now := time.Now()
	prev, cur := adviceSnapshot(now.Add(-time.Minute), 1), adviceSnapshot(now, 2)
	assertOnlyWorkloadQuery(t, "high_total_time",
		ruleTotalTimeHeavy(cur, prev, adviceConfig(), nil))
}

func TestHighFreqFirstCycleIgnoresDiagnosticStatements(t *testing.T) {
	cur := adviceSnapshot(time.Now(), 1)
	// The diagnostic statements are the heaviest: they must not crowd the
	// application query out of the top three either.
	for i := range cur.Queries {
		if cur.Queries[i].QueryID != 1 {
			cur.Queries[i].TotalExecTime = 9e9
		}
	}
	assertOnlyWorkloadQuery(t, "high_total_time(first cycle)",
		ruleHighFreqFirstCycle(cur, nil, adviceConfig(), nil))
}

// Capacity is about pg_stat_statements entries, diagnostic or not.
func TestStatStatementsCapacityStillCountsDiagnosticStatements(t *testing.T) {
	cur := adviceSnapshot(time.Now(), 1)
	cur.System.StatStatementsMax = 6
	got := ruleStatStatementsCapacity(cur, nil, adviceConfig(), nil)
	if len(got) != 1 || got[0].Detail["tracked_queries"] != 6 {
		t.Fatalf("capacity findings = %+v, want 6 of 6 tracked", got)
	}
}

func TestDropNonWorkloadFindings(t *testing.T) {
	in := []Finding{
		{Category: "plan_regression", ObjectIdentifier: "queryid:7",
			Detail: map[string]any{"query": diagExplain}},
		{Category: "slow_query", ObjectIdentifier: "queryid:1",
			Detail: map[string]any{"query": "SELECT 1 FROM orders"}},
		{Category: "sort_without_index", ObjectIdentifier: "queryid:8",
			Detail: map[string]any{"query_text": "VACUUM orders"}},
		{Category: "table_bloat", ObjectIdentifier: "public.orders",
			Detail: map[string]any{"n_dead_tup": 5}},
		{Category: "slow_query", ObjectIdentifier: "queryid:9"},
	}
	got := dropNonWorkloadFindings(in)
	if len(got) != 3 {
		t.Fatalf("kept %d findings, want 3 (app query, table, no detail): %+v", len(got), got)
	}
	for _, f := range got {
		if f.ObjectIdentifier == "queryid:7" || f.ObjectIdentifier == "queryid:8" {
			t.Fatalf("kept a finding about a diagnostic statement: %+v", f)
		}
	}
	if dropNonWorkloadFindings(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
