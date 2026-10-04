package optimizer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood round 2: diagnostic and maintenance statements are not
// workload. EXPLAIN ANALYZE of a query names its tables, and a CREATE INDEX
// names its table too; neither may become evidence for an index.
func TestGroupQueriesByTableSkipsDiagnosticStatements(t *testing.T) {
	snap := &collector.Snapshot{Queries: []collector.QueryStats{
		{QueryID: 1, Query: "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM public.orders " +
			"WHERE status = $1", Calls: 5000, MeanExecTime: 900, TotalExecTime: 4.5e6},
		{QueryID: 2, Query: "VACUUM (ANALYZE) public.orders", Calls: 50, MeanExecTime: 900},
		{QueryID: 3, Query: "CREATE INDEX CONCURRENTLY ix ON public.orders (status)",
			Calls: 1, MeanExecTime: 9000},
		{QueryID: 4, Query: "COPY public.orders (id, status) TO stdout", Calls: 3},
		{QueryID: 5, Query: "SELECT * FROM public.orders WHERE customer_id = $1",
			Calls: 10, MeanExecTime: 50},
	}}
	for table, qs := range groupQueriesByTable(snap) {
		for _, q := range qs {
			if q.QueryID != 5 {
				t.Errorf("table %s: diagnostic statement %d kept: %q", table, q.QueryID,
					q.Text)
			}
		}
	}
	if got := groupQueriesByTable(snap)["public.orders"]; len(got) != 1 {
		t.Fatalf("public.orders queries = %+v, want only the application query", got)
	}
	if got := applicationQueries(snap.Queries); len(got) != 1 || got[0].QueryID != 5 {
		t.Fatalf("applicationQueries = %+v, want only query 5", got)
	}
}
