package advisor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood round 2: the LLM advisors read workload only. EXPLAIN ANALYZE
// and maintenance statements are the heaviest entries on a quiet database
// (lifeos: a 2.35 s EXPLAIN of a pg_sage probe) and spill in
// maintenance_work_mem, not work_mem.
func adviceQueries() []collector.QueryStats {
	return []collector.QueryStats{
		{QueryID: 1, Query: "EXPLAIN (ANALYZE) SELECT * FROM orders", Calls: 500,
			MeanExecTime: 2352, TotalExecTime: 9e9, TempBlksWritten: 900},
		{QueryID: 2, Query: "CREATE INDEX ix ON orders (a)", Calls: 500,
			MeanExecTime: 9000, TotalExecTime: 8e9, TempBlksWritten: 70000},
		{QueryID: 3, Query: "VACUUM orders", Calls: 500, MeanExecTime: 900,
			TotalExecTime: 7e9, TempBlksWritten: 10},
		{QueryID: 4, Query: "SELECT * FROM orders WHERE a = $1", Calls: 500,
			MeanExecTime: 80, TotalExecTime: 4e4, TempBlksWritten: 40},
		{QueryID: 5, Query: "SELECT * FROM items WHERE b = $1", Calls: 50,
			MeanExecTime: 1, TotalExecTime: 50},
	}
}

func TestRewriteCandidatesAreWorkloadOnly(t *testing.T) {
	got := selectRewriteCandidates(&collector.Snapshot{Queries: adviceQueries()})
	if len(got) == 0 {
		t.Fatal("no candidate: the application query must still be considered")
	}
	for _, c := range got {
		if c.query.QueryID != 4 {
			t.Errorf("rewrite candidate %d (%q): diagnostic statements are not workload",
				c.query.QueryID, c.query.Query)
		}
	}
}

func TestRewriteCandidatesEmptySnapshot(t *testing.T) {
	if got := selectRewriteCandidates(&collector.Snapshot{}); len(got) != 0 {
		t.Fatalf("empty snapshot gave %d candidates", len(got))
	}
}

func TestSpillSummaryCountsWorkloadOnly(t *testing.T) {
	n, total, top := spillSummary(adviceQueries())
	if n != 1 || total != 40 {
		t.Fatalf("spilling queries = %d, temp blocks = %d; want 1 and 40 (workload only)",
			n, total)
	}
	if len(top) != 1 || top[0].query != "SELECT * FROM orders WHERE a = $1" {
		t.Fatalf("top spills = %+v, want only the application query", top)
	}
	if n, total, top := spillSummary(nil); n != 0 || total != 0 || len(top) != 0 {
		t.Fatalf("nil queries: %d, %d, %v", n, total, top)
	}
}
