package optimizer

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/workload"
)

// applicationQueries keeps the workload statements (internal/workload):
// pg_stat_statements tracks pg_sage (perf v1.8.3) and diagnostic tooling
// (EXPLAIN, VACUUM, CREATE INDEX, backups), and the optimizer must never
// recommend an index for, or capture plans of, either. The collector
// already leaves tagged statements out; this also covers older snapshots.
func applicationQueries(queries []collector.QueryStats) []collector.QueryStats {
	return workload.Queries(queries)
}
