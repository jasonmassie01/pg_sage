package optimizer

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// applicationQueries drops pg_sage's own statements: pg_stat_statements
// tracks pg_sage (perf v1.8.3), and the optimizer must never recommend an
// index for, or capture plans of, its own SQL. The collector already
// leaves tagged statements out; this also covers older snapshots.
func applicationQueries(queries []collector.QueryStats) []collector.QueryStats {
	out := make([]collector.QueryStats, 0, len(queries))
	for _, q := range queries {
		if !selfmonitor.IsQueryText(q.Query) {
			out = append(out, q)
		}
	}
	return out
}
