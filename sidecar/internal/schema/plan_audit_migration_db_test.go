package schema

import (
	"context"
	"strings"
	"testing"
)

// plan-01b on a real server: bootstrap twice (and a third time) is clean,
// records ddlPlanEvents once, leaves both tables empty and chains them;
// the table checks refuse what the store also refuses.

func TestPlanEventsMigration_BootstrapTwiceIsCleanAndChained(t *testing.T) {
	pool := freshSchemaPool(t, "plan_events") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanEvents'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanEvents: %d (%v), want 1", ledger, err)
	}
	for _, table := range []string{"plan_event", "plan_model_record"} {
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.`+table).Scan(&rows); err != nil ||
			rows != 0 {
			t.Fatalf("sage.%s after bootstrap: %d rows (%v), want empty", table, rows, err)
		}
	}
	var chains []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(chain::text ORDER BY chain)
		FROM sage.audit_chain_meta WHERE chain LIKE 'plan%'`).Scan(&chains); err != nil {
		t.Fatal(err)
	}
	if strings.Join(chains, ",") != "plan_events,plan_model_records" {
		t.Fatalf("plan chains installed = %v", chains)
	}
}

func TestPlanEventsMigration_ChecksRefuseBadRows(t *testing.T) {
	pool := freshSchemaPool(t, "plan_events_checks")
	ctx := context.Background()
	sha := strings.Repeat("d", 64)
	bad := []string{
		`INSERT INTO sage.plan_event (plan_id, kind, plan_hash, actor)
			VALUES (1, 'exploded', '` + sha + `', 'x')`,
		`INSERT INTO sage.plan_event (plan_id, kind, plan_hash, actor)
			VALUES (1, 'created', 'short', 'x')`,
		`INSERT INTO sage.plan_event (plan_id, kind, plan_hash, actor, detail)
			VALUES (1, 'created', '` + sha + `', 'x', '[1]')`,
		`INSERT INTO sage.plan_model_record (plan_id, model, raw_output, raw_output_sha256)
			VALUES (1, 'm', repeat('x', 1048577), '` + sha + `')`,
		`INSERT INTO sage.plan_model_record (plan_id, model, raw_output, raw_output_sha256)
			VALUES (1, '', 'x', '` + sha + `')`,
	}
	for _, sql := range bad {
		if _, err := pool.Exec(ctx, sql); err == nil ||
			!strings.Contains(err.Error(), "23514") {
			t.Errorf("%s: %v, want a check violation", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.plan_model_record (plan_id, model,
		raw_output, raw_output_sha256) VALUES (1, 'm', repeat('x', 1048576), $1)`,
		sha); err != nil {
		t.Fatalf("exactly 1 MiB refused: %v", err)
	}
}
