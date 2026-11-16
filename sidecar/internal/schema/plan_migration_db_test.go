package schema

import (
	"context"
	"strings"
	"testing"
)

// plan-01a on a real server: bootstrap twice is clean, records ddlPlans in
// the ledger once and writes no plan; the second boot finds every plan
// table and index already there. No concurrent-writer test: Bootstrap
// holds its advisory lock (TestBootstrap_ParallelBootsLeaveTheFreshCatalog).

func TestPlansMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plans") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlans'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlans: %d (%v), want 1", ledger, err)
	}
	for _, table := range []string{"plan", "plan_step", "plan_step_edge", "plan_approval"} {
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.`+table).Scan(&rows); err != nil ||
			rows != 0 {
			t.Fatalf("sage.%s after bootstrap: %d rows (%v), want an empty table", table, rows, err)
		}
	}
	var indexes []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(indexname::text ORDER BY indexname)
		FROM pg_indexes WHERE schemaname = 'sage' AND tablename LIKE 'plan%'`).
		Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(indexes, ",")
	for _, want := range []string{"plan_approval_live", "plan_pack", "plan_step_edge_to",
		"plan_updated", "plan_step_plan_id_ordinal_key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("index %s missing; plan indexes %v", want, indexes)
		}
	}
}

// The live-approval key allows one live approval per plan and any number
// of voided ones.
func TestPlansMigration_OneLiveApprovalPerPlan(t *testing.T) {
	pool := freshSchemaPool(t, "plans_live")
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('g', 'agent', 'proposed', repeat('b', 64), 'agp_1')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	insert := func(voided bool) error {
		sql := `INSERT INTO sage.plan_approval (plan_id, plan_hash, approver_user_id)
			VALUES ($1, repeat('b', 64), 3)`
		if voided {
			sql = `INSERT INTO sage.plan_approval (plan_id, plan_hash, approver_user_id,
				voided_at, void_reason) VALUES ($1, repeat('b', 64), 3, now(), 'edited')`
		}
		_, err := pool.Exec(ctx, sql, id)
		return err
	}
	for i, voided := range []bool{true, true, false} {
		if err := insert(voided); err != nil {
			t.Fatalf("approval %d (voided %v): %v", i, voided, err)
		}
	}
	if err := insert(false); err == nil || !strings.Contains(err.Error(), "plan_approval_live") {
		t.Fatalf("a second live approval: %v, want the live key", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('', 'agent', 'proposed', repeat('b', 64), 'x')`); err == nil {
		t.Fatal("an empty goal was accepted")
	}
}
