package rca

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The merge of open duplicates must process each open row a bounded number
// of times, whatever the planner believes about sage.incidents. The v2.4.0
// release CI spent 52 s merging 5,000 duplicates of one identity. With
// statistics from before the backlog (one open row expected) the planner
// re-ran the grouping subquery once per open row, 25 million index reads;
// with fresher statistics it re-scanned the per-row CTE once per row
// instead, millions of CTE tuples. Both are quadratic in the backlog.
//
// Only the plan bound is pinned here. Correct merges, invalid input,
// empty backlogs and concurrent runs are covered in reconcile_db_test.go.

// planOpenDuplicates is the open legacy duplicates of one identity, over
// twice as much resolved history.
const planOpenDuplicates = 2000

// mergeTuplesPerOpenRow bounds the tuples every scan node of the merge
// (tables, indexes and CTEs) returns per open row. A linear merge needs
// under 10, counting the dead index entries the backfill leaves; a
// quadratic one needs about planOpenDuplicates.
const mergeTuplesPerOpenRow = 25

func TestReconcile_MergeIsLinearInTheOpenBacklog(t *testing.T) {
	for _, tc := range []struct {
		name          string
		analyzeBefore bool // statistics older than the backlog
	}{
		{"statistics before the backlog", true},
		{"statistics of the backlog", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := lifecycleDB(t)
			_, db := lifecycleEngine(t, pool)
			seedPlanBacklog(t, ctx, db, tc.analyzeBefore)
			plan := explainMerge(t, ctx, db)
			tuples, updated := 0.0, 0.0
			plan.Walk(func(n testdb.PlanNode) {
				if n.NodeType == "ModifyTable" && n.Relation == "incidents" {
					updated += n.ActualRows
				}
				if isScanNode(n.NodeType) {
					tuples += n.ActualRows * max(n.ActualLoops, 1)
				}
			})
			// The bound only means something when the explained merge did the
			// work: the survivor kept and every other duplicate resolved.
			if updated != planOpenDuplicates {
				t.Fatalf("merge updated %g rows, want %d:\n%s", updated,
					planOpenDuplicates, plan)
			}
			limit := float64(mergeTuplesPerOpenRow * planOpenDuplicates)
			if tuples > limit {
				t.Fatalf("merge scanned %g tuples for %d open rows, want <= %g:\n%s",
					tuples, planOpenDuplicates, limit, plan)
			}
		})
	}
}

func isScanNode(nodeType string) bool {
	switch nodeType {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Index Scan",
		"Bitmap Heap Scan", "CTE Scan", "Subquery Scan", "Function Scan",
		"WorkTable Scan", "Tid Scan", "Tid Range Scan", "Values Scan":
		return true
	}
	return false
}

// seedPlanBacklog adds planOpenDuplicates open rows of one identity
// without identity keys, and twice as many resolved ones, then backfills
// their identities. analyzeBefore takes the table's statistics while db
// has no rows; otherwise they are taken after the backfill.
func seedPlanBacklog(t *testing.T, ctx context.Context, db string, analyzeBefore bool) {
	t.Helper()
	pool, _ := lifecycleDB(t)
	mark := "fixture:" + db
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE rollback_sql = $1", mark)
	})
	analyze := func(when string) {
		if _, err := pool.Exec(ctx, "ANALYZE sage.incidents"); err != nil {
			t.Fatalf("analyze %s: %v", when, err)
		}
	}
	if analyzeBefore {
		analyze("before the seed")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.incidents (detected_at, severity,
		root_cause, source, signal_ids, affected_objects, database_name, rollback_sql,
		resolved_at, resolved_by)
		SELECT now() - make_interval(mins => i), 'warning', 'x', 'deterministic',
		       '{vacuum_blocked}', ARRAY['public.big'], $1, $2,
		       CASE WHEN i > $3 THEN now() END, CASE WHEN i > $3 THEN 'pg_sage' END
		FROM generate_series(1, $3 * 3) i`, db, mark, planOpenDuplicates); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if n, err := backfillIdentities(ctx, conn, db); err != nil || n != planOpenDuplicates {
		t.Fatalf("backfill = %d (%v), want %d", n, err, planOpenDuplicates)
	}
	if !analyzeBefore {
		analyze("after the backfill")
	}
}

// explainMerge runs the merge statement under EXPLAIN ANALYZE in a
// transaction it rolls back.
func explainMerge(t *testing.T, ctx context.Context, db string) testdb.PlanNode {
	t.Helper()
	pool, _ := lifecycleDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	plan, err := testdb.Explain(ctx, tx, "ANALYZE", mergeDuplicatesSQL, db,
		reconcileGroupsBatch, ResolvedByMerged)
	if err != nil {
		t.Fatalf("explain merge: %v", err)
	}
	return plan
}
