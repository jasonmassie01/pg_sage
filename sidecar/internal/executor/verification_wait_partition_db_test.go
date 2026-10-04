package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Owner decisions on PR #122, on real Postgres:
//   - an index drop holds its table only until its first window concludes
//     (trust.rollback_window_minutes, 15 min by default); the release is
//     recorded as "drop's first window concluded" and its soft-drop
//     monitoring keeps watching the business cycle;
//   - a partitioned table, its partitions and their indexes are one object
//     for index, statistics and reloption changes (pg_inherits, through
//     pg_partition_root), in both directions.

func TestOneChange_DropHoldsItsTableOnlyForItsFirstWindow(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	lifeosTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	second := memoriesIndexFinding("idx_memories_status_type_quality_current",
		lifeos6411Where)
	drop := recordInFlight(t, pool, ctx, "DROP INDEX CONCURRENTLY public.idx_memories_gone",
		"CREATE INDEX CONCURRENTLY idx_memories_gone ON public.memories USING btree (status)",
		"monitoring", 15*time.Minute-30*time.Second)
	pendingOutcome(t, pool, ctx, drop, "index_drop")
	requireParkedOn(t, authorizeFinding(t, exec, ctx, second), drop)

	// Thirty seconds past the first window: released, the drop still watched.
	mustExec(t, pool, ctx, fmt.Sprintf(`UPDATE sage.action_log SET executed_at =
		now() - interval '15 minutes 30 seconds' WHERE id = %d`, drop))
	before := waitReleaseCount(exec, "drop_first_window")
	d := authorizeFinding(t, exec, ctx, second)
	if d.Verdict != policy.VerdictExecute ||
		!strings.Contains(d.Detail, "drop's first window concluded") {
		t.Fatalf("after the drop's first window = %+v, want execute with the release noted", d)
	}
	_, evidence := decisionEvidence(t, pool, ctx, d.DecisionID)
	released, _ := evidence["verification_wait_released"].([]any)
	entry, _ := firstEntry(released)
	if fmt.Sprint(evidenceActionIDs(evidence, "verification_wait_released")) !=
		fmt.Sprint([]int64{drop}) || entry["release_reason"] != "drop's first window concluded" {
		t.Fatalf("evidence %v, want the drop's first-window release recorded", evidence)
	}
	if got := waitReleaseCount(exec, "drop_first_window"); got != before+1 {
		t.Fatalf("drop_first_window releases %d, want %d", got, before+1)
	}
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT outcome FROM sage.action_log WHERE id = $1`,
		drop).Scan(&outcome); err != nil || outcome != "monitoring" {
		t.Fatalf("the drop's own monitoring = %q, %v; want it still watching", outcome, err)
	}
}

func firstEntry(items []any) (map[string]any, bool) {
	if len(items) == 0 {
		return nil, false
	}
	entry, ok := items[0].(map[string]any)
	return entry, ok
}

// partitionTables builds public.pevents, range-partitioned in two, with an
// index on one partition, plus an unrelated plain table.
func partitionTables(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	mustExec(t, pool, ctx, `CREATE TABLE public.pevents (id bigint, kind text)
		  PARTITION BY RANGE (id);
		CREATE TABLE public.pevents_p1 PARTITION OF public.pevents
		  FOR VALUES FROM (0) TO (1000);
		CREATE TABLE public.pevents_p2 PARTITION OF public.pevents
		  FOR VALUES FROM (1000) TO (2000);
		CREATE INDEX pevents_p1_kind ON public.pevents_p1 (kind);
		CREATE TABLE public.plain (id bigint, kind text);`)
}

func partitionIndexFinding(table, index string) analyzer.Finding {
	return analyzer.Finding{Category: "missing_index", ObjectType: "index",
		ObjectIdentifier: table + "|btree(kind)", Title: "index " + index,
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + index + " ON " + table + " (kind)",
		RollbackSQL:    "DROP INDEX CONCURRENTLY IF EXISTS public." + index,
		Detail:         verifiedDetail()}
}

func reloptionFinding(table string) analyzer.Finding {
	return analyzer.Finding{Category: "autovacuum_tuning", ObjectType: "table",
		ObjectIdentifier: table, Title: "tune " + table,
		RecommendedSQL: "ALTER TABLE " + table + " SET (autovacuum_vacuum_scale_factor = 0.02)",
		RollbackSQL:    "ALTER TABLE " + table + " RESET (autovacuum_vacuum_scale_factor)"}
}

func TestOneChange_ParentChangeHoldsItsPartitions(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	partitionTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	onP1 := partitionIndexFinding("public.pevents_p1", "pevents_p1_id")
	reloP2 := reloptionFinding("public.pevents_p2")
	requireExecutable(t, exec, ctx, onP1)
	requireExecutable(t, exec, ctx, reloP2)
	parent := partitionIndexFinding("public.pevents", "pevents_kind")
	first := recordInFlight(t, pool, ctx, parent.RecommendedSQL, parent.RollbackSQL,
		"monitoring", time.Minute)

	d := authorizeFinding(t, exec, ctx, onP1)
	requireParkedOn(t, d, first)
	if !strings.Contains(d.Detail, "public.pevents") {
		t.Fatalf("detail %q, want the partition tree named", d.Detail)
	}
	requireParkedOn(t, authorizeFinding(t, exec, ctx, reloP2), first)
	plain := partitionIndexFinding("public.plain", "plain_kind")
	if got := authorizeFinding(t, exec, ctx, plain); got.Verdict != policy.VerdictExecute {
		t.Fatalf("index on an unrelated table = %+v, want execute", got)
	}
	// VACUUM and ANALYZE are not partition-scoped: a partition's own
	// maintenance does not wait for a change to the parent.
	analyze := analyzer.Finding{Category: "stale_statistics", ObjectType: "table",
		ObjectIdentifier: "public.pevents_p2", Title: "analyze p2",
		RecommendedSQL: "ANALYZE public.pevents_p2"}
	if got := authorizeFinding(t, exec, ctx, analyze); got.Verdict != policy.VerdictExecute {
		t.Fatalf("ANALYZE of a partition = %+v, want execute", got)
	}
}

func TestOneChange_PartitionChangeHoldsItsParent(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	partitionTables(t, pool, ctx)
	exec := waitExecutor(t, pool)
	parent := partitionIndexFinding("public.pevents", "pevents_kind")
	requireExecutable(t, exec, ctx, parent)
	// An in-flight REINDEX names a partition's index: index -> partition ->
	// partitioned root.
	first := recordInFlight(t, pool, ctx, "REINDEX INDEX CONCURRENTLY public.pevents_p1_kind",
		"", "monitoring", time.Minute)
	requireParkedOn(t, authorizeFinding(t, exec, ctx, parent), first)
	stats := analyzer.Finding{Category: "extended_statistics", ObjectType: "table",
		ObjectIdentifier: "public.pevents", Title: "stats pevents",
		RecommendedSQL: "CREATE STATISTICS public.pevents_s (dependencies) ON id, kind " +
			"FROM public.pevents"}
	if got := authorizeFinding(t, exec, ctx, stats); got.Reason !=
		policy.ReasonAwaitingVerification {
		t.Fatalf("statistics on the parent = %+v, want parked", got)
	}
	concludeVerification(t, pool, ctx, first, "success", "neutral")
	if got := authorizeFinding(t, exec, ctx, parent); got.Verdict != policy.VerdictExecute {
		t.Fatalf("after the verdict = %+v, want execute", got)
	}
}

// The catalog side of the lookup: a partition, a partition's index and the
// parent resolve to one partition-tree root; a plain table is its own.
func TestResolveChangeTablesNamesThePartitionRoot(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	partitionTables(t, pool, ctx)
	got, err := policy.ResolveChangeTables(ctx, pool, []string{"public.pevents",
		"public.pevents_p2", "public.pevents_p1_kind", "plain", "public.missing", "x y"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]policy.ChangeTable{
		"public.pevents":         {Table: "public.pevents", Root: "public.pevents"},
		"public.pevents_p2":      {Table: "public.pevents_p2", Root: "public.pevents"},
		"public.pevents_p1_kind": {Table: "public.pevents_p1", Root: "public.pevents"},
		"plain":                  {Table: "public.plain"},
		"public.missing":         {Table: "public.missing"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ResolveChangeTables = %v, want %v", got, want)
	}
}
