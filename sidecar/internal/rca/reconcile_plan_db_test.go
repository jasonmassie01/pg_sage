package rca

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The merge of open duplicates must read each open row a bounded number of
// times, whatever the planner believes about sage.incidents. With
// statistics from before a burst of legacy rows (an estimate of one open
// row), the planner re-ran the grouping subquery once per open row: the
// v2.4.0 release CI spent 52 s merging 5,000 duplicates of one identity,
// 25 million index reads.
//
// Only the plan bound is pinned here. Correct merges, invalid input,
// empty backlogs and concurrent runs are covered in reconcile_db_test.go.

// planOpenDuplicates is the open legacy duplicates of one identity, over
// twice as much resolved history.
const planOpenDuplicates = 2000

// mergeReadsPerOpenRow bounds the merge's reads of sage.incidents per open
// row. It needs 4: the grouping scan, the window scan, and each row's
// primary-key lookup and update.
const mergeReadsPerOpenRow = 6

func TestReconcile_MergeReadsEachOpenRowBoundedTimes(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	seedPlanBacklog(t, ctx, db)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if n, err := backfillIdentities(ctx, conn, db); err != nil || n != planOpenDuplicates {
		t.Fatalf("backfill = %d (%v), want %d", n, err, planOpenDuplicates)
	}
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
	reads, updated := 0.0, 0.0
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation != "incidents" {
			return
		}
		reads += n.ActualRows * max(n.ActualLoops, 1)
		if n.NodeType == "ModifyTable" {
			updated += n.ActualRows
		}
	})
	// The bound is only meaningful when the explained merge did the work:
	// the survivor kept and every other duplicate resolved.
	if updated != planOpenDuplicates {
		t.Fatalf("merge updated %g rows, want %d:\n%s", updated, planOpenDuplicates, plan)
	}
	if limit := float64(mergeReadsPerOpenRow * planOpenDuplicates); reads > limit {
		t.Fatalf("merge read sage.incidents %g times for %d open rows, want <= %g:\n%s",
			reads, planOpenDuplicates, limit, plan)
	}
}

// seedPlanBacklog analyzes sage.incidents while db has no rows, then adds
// planOpenDuplicates open rows of one identity without identity keys, and
// twice as many resolved ones.
func seedPlanBacklog(t *testing.T, ctx context.Context, db string) {
	t.Helper()
	pool, _ := lifecycleDB(t)
	mark := "fixture:" + db
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE rollback_sql = $1", mark)
	})
	if _, err := pool.Exec(ctx, "ANALYZE sage.incidents"); err != nil {
		t.Fatalf("analyze before the seed: %v", err)
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
}
