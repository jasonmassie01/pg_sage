package optimizer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// pg_sage is tracked by pg_stat_statements now (perf v1.8.3). Even if one
// of its statements reaches a snapshot (an older snapshot, or a text
// pg_stat_statements kept from an untagged first call), the optimizer
// must never build an index recommendation for it.
func TestGroupQueriesByTableSkipsPgSageStatements(t *testing.T) {
	snap := &collector.Snapshot{Queries: []collector.QueryStats{
		{QueryID: 1, Query: "/* pg_sage */ SELECT * FROM public.orders WHERE status = $1",
			Calls: 5000, MeanExecTime: 900, TotalExecTime: 4.5e6},
		{QueryID: 2, Query: "/* pg_sage sre:lock_graph v1 */ SELECT * FROM public.orders o",
			Calls: 5000, MeanExecTime: 900},
		{QueryID: 3, Query: "SELECT * FROM sage.findings WHERE status = $1", Calls: 100},
		{QueryID: 4, Query: "SELECT * FROM public.orders WHERE customer_id = $1",
			Calls: 10, MeanExecTime: 50},
	}}
	groups := groupQueriesByTable(snap)
	orders := groups["public.orders"]
	if len(orders) != 1 || orders[0].QueryID != 4 {
		t.Fatalf("public.orders queries = %+v, want only the application query 4", orders)
	}
	for table, qs := range groups {
		for _, q := range qs {
			if q.QueryID != 4 {
				t.Errorf("table %s: pg_sage query %d kept: %q", table, q.QueryID, q.Text)
			}
		}
	}
}

func TestGroupQueriesByTableEmptySnapshot(t *testing.T) {
	if got := groupQueriesByTable(&collector.Snapshot{}); len(got) != 0 {
		t.Fatalf("empty snapshot grouped %v", got)
	}
}
