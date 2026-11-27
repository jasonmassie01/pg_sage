package shadow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender (2026-10-04, once sage.action_outcome was seeded):
// the scorer's read of recent actions LEFT JOINed the outcome table and,
// for a window of a few days (1,500 actions), hashed all 20,000 outcomes:
// a seq scan per scorer pass that grows with every verified action. Each
// action's verdict is one primary-key read, whatever the window.
func TestScorerFactReadsNeverScanTheOutcomeTable(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "scorer_facts_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:scorer_facts_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, _ := testdb.XactScansOf(ctx, tx, "sage.action_outcome")
	for _, days := range []int{1, 6, 30} {
		since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		for _, q := range []string{actionFactsSQL, queueFactsSQL} {
			rows, err := tx.Query(ctx, q, since, factBatch)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
		}
	}
	after, _ := testdb.XactScansOf(ctx, tx, "sage.action_outcome")
	if d := after.Minus(before); d.Seq != 0 {
		t.Fatalf("scorer fact reads scanned sage.action_outcome %d times (%d rows)", d.Seq,
			d.SeqRead)
	}
}

// earned-03: the read of pg_sage's own index changes since a decision
// walks the partial index of index DDL actions, and the pending read
// fetches its picked decisions by primary key, in their generic plans
// (EXPLAIN (GENERIC_PLAN) needs PG16): neither reads its table whole.
func TestScorerReadsWalkTheirIndexes(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "scorer_own_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:scorer_own_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sql, index, table string }{
		{"own index actions", ownIndexActionsSQL, "idx_action_log_index_ddl", "action_log"},
		{"pending", pendingSQL, "shadow_decision_pkey", "shadow_decision"},
	} {
		plan := genericPlan(ctx, t, pool, tc.sql)
		walked := plan.Has(func(n testdb.PlanNode) bool { return n.Index == tc.index })
		joined := plan.Has(func(n testdb.PlanNode) bool { return n.NodeType == "Hash Join" })
		if !walked || joined || plan.SeqScans(tc.table) != 0 {
			t.Errorf("%s: plan does not walk %s (or joins by hash):\n%s", tc.name, tc.index,
				plan)
		}
	}
}

// genericPlan is the generic plan of sql, explained over the simple
// protocol (pgx would bind its parameters).
func genericPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	sql string) testdb.PlanNode {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	results, err := conn.Conn().PgConn().Exec(ctx,
		"EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+sql).ReadAll()
	if err != nil || len(results) != 1 || len(results[0].Rows) != 1 {
		t.Fatalf("explain: %v (%d results)", err, len(results))
	}
	var out []struct {
		Plan testdb.PlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(results[0].Rows[0][0], &out); err != nil || len(out) != 1 {
		t.Fatalf("decode plan (%d plans): %v", len(out), err)
	}
	return out[0].Plan
}
