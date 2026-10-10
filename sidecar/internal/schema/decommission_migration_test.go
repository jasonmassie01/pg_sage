package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The AgentDB decommission (AGENTDB-SPEC §12, G0-07): bootstrap creates the
// acknowledgement table, and deletes persisted agentdb.* overrides with one
// sage.config_audit row each. Nothing else in sage.config is touched, and a
// re-run neither deletes nor audits again.

func TestDecommissionMigration_AckTable(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, column := range []string{"resource_id", "exported", "acknowledged_by",
		"source", "acknowledged_at"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'agentdb_decommission'
			  AND column_name = $1`, column).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s: %d, %v", column, n, err)
		}
	}
	insert := `INSERT INTO sage.agentdb_decommission (resource_id, exported,
		acknowledged_by, source) VALUES ($1, $2, 'admin', $3)`
	if _, err := pool.Exec(ctx, insert, "mig-test:ok", true, "api"); err != nil {
		t.Fatalf("insert a valid acknowledgement: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agentdb_decommission
			WHERE resource_id LIKE 'mig-test:%'`)
	})
	for name, args := range map[string][]any{
		"duplicate":    {"mig-test:ok", true, "api"},
		"bad source":   {"mig-test:src", true, "chat"},
		"not exported": {"mig-test:exp", false, "yaml"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s acknowledgement was accepted", name)
		}
	}
}

func TestDecommissionMigration_DeletesPersistedOverridesWithAudit(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config WHERE key LIKE 'agentdb.%'
			OR key = 'mig_test.keep'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit WHERE key LIKE 'agentdb.%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	for key, value := range map[string]string{
		"agentdb.live_provisioning_enabled":  "true",
		"agentdb.reconcile_interval_seconds": "45",
		"mig_test.keep":                      "1",
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by)
			VALUES ($1, $2, 'test')`, key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	bootstrapWithRetry(t, ctx, pool)
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE key LIKE 'agentdb.%'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("agentdb.* overrides left = %d, %v", left, err)
	}
	var keep string
	if err := pool.QueryRow(ctx, `SELECT value FROM sage.config
		WHERE key = 'mig_test.keep'`).Scan(&keep); err != nil || keep != "1" {
		t.Fatalf("an unrelated override was touched: %q, %v", keep, err)
	}
	audits := auditRows(t, ctx, pool)
	if len(audits) != 2 {
		t.Fatalf("audit rows = %v, want one per deleted override", audits)
	}
	for _, a := range audits {
		if !strings.Contains(a, "§12") || !strings.HasPrefix(a, "agentdb.") {
			t.Fatalf("audit row %q must name the key and §12", a)
		}
	}
	if !strings.Contains(strings.Join(audits, "\n"),
		"agentdb.reconcile_interval_seconds|45|") {
		t.Fatalf("the audit row must keep the old value: %v", audits)
	}
	bootstrapWithRetry(t, ctx, pool)
	if again := auditRows(t, ctx, pool); len(again) != 2 {
		t.Fatalf("a re-run audited again: %v", again)
	}
}

func auditRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key || '|' || COALESCE(old_value, '') || '|' ||
		new_value || '|' || COALESCE(changed_by_actor, '')
		FROM sage.config_audit WHERE key LIKE 'agentdb.%' ORDER BY key`)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
