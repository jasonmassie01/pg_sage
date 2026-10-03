package rca

import (
	"context"
	"strings"
	"testing"
)

// Perf gate (sage.incidents, 3 unattributed seq scans of 20,000 rows per
// startup): the prune deleted WHERE id IN (subquery), which the planner may
// run as a hash join over a full scan of incidents. Collected into an array
// first, the batch is deleted through the primary key.
func TestPruneResolvedIncidents_PlanDoesNotScanIncidents(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.incidents (severity, root_cause, source,
		detected_at, last_detected_at, resolved_at, database_name)
		SELECT 'info', 'plan test', 'deterministic', now() - interval '400 days',
		       now() - interval '400 days', now() - g * interval '1 minute', $1
		FROM generate_series(1, 20000) g`, db); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE sage.incidents"); err != nil {
		t.Fatal(err)
	}
	var plan string
	sql := strings.NewReplacer("$1", "86400", "$2", "500").Replace(pruneIncidentsSQL)
	if err := pool.QueryRow(context.Background(), "EXPLAIN (FORMAT JSON) "+sql).
		Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan, `"Seq Scan"`) {
		t.Fatalf("prune plan scans sage.incidents:\n%s", plan)
	}
}
