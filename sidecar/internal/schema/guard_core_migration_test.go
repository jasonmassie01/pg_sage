package schema

import (
	"testing"
)

// Agent governance G1 core (AGENTDB-SPEC §7): principals, cluster roles,
// taint, the PUBLIC baseline, provenance columns and the proposed_via v2
// constraint. Every CHECK rejects what the spec forbids, and a re-run is a
// no-op.

func TestGuardCoreMigration_TablesAndChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent
	for _, tbl := range []string{"guard_principals", "guard_cluster_roles", "guard_taint",
		"guard_public_baseline"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			tbl).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s: %v %v", tbl, ok, err)
		}
	}
	insert := `INSERT INTO sage.guard_principals (id, name, profile, created_by)
		VALUES ($1, $2, 'legacy', 'test')`
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_principals WHERE created_by = 'test'`)
	})
	if _, err := pool.Exec(ctx, insert, "agp_aaaaaaaaaaaaaaaaaaab", "mig-ok"); err != nil {
		t.Fatalf("valid principal: %v", err)
	}
	for name, args := range map[string][]any{
		"short id":     {"agp_aaaa", "mig-a"},
		"upper id":     {"agp_AAAAAAAAAAAAAAAAAAAA", "mig-b"},
		"digit 1":      {"agp_aaaaaaaaaaaaaaaaaaa1", "mig-c"},
		"bad name":     {"agp_aaaaaaaaaaaaaaaaaaac", "Mig"},
		"one char":     {"agp_aaaaaaaaaaaaaaaaaaad", "m"},
		"dup name":     {"agp_aaaaaaaaaaaaaaaaaaae", "mig-ok"},
		"leading dash": {"agp_aaaaaaaaaaaaaaaaaaaf", "-mig"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, sql := range map[string]string{
		"env": `UPDATE sage.guard_principals SET env_ceiling = 'qa'
			WHERE id = 'agp_aaaaaaaaaaaaaaaaaaab'`,
		"status": `UPDATE sage.guard_principals SET status = 'killed'
			WHERE id = 'agp_aaaaaaaaaaaaaaaaaaab'`,
		"login role": `INSERT INTO sage.guard_cluster_roles (principal_id, cluster_key,
			login_role, broker_role, broker_secret_ct, key_id)
			VALUES ('agp_aaaaaaaaaaaaaaaaaaab', 'c', 'postgres', 'sage_agentb_abcdefghij',
			'\x00', 'k')`,
		"role status": `INSERT INTO sage.guard_cluster_roles (principal_id, cluster_key,
			login_role, broker_role, broker_secret_ct, key_id, status)
			VALUES ('agp_aaaaaaaaaaaaaaaaaaab', 'c', 'sage_agent_abcdefghij',
			'sage_agentb_abcdefghij', '\x00', 'k', 'frozen')`,
		"baseline kind": `INSERT INTO sage.guard_public_baseline (object_kind, object_oid,
			object_name, privilege) VALUES ('type', 1, 'x', 'USAGE')`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s violation was accepted", name)
		}
	}
}

func TestGuardCoreMigration_ProvenanceAndProposedVia(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for table, cols := range map[string][]string{
		"action_log": {"principal_id", "on_behalf_of", "task_id", "approval_id",
			"envelope_id", "artifact_hash", "policy_version"},
		"action_queue": {"principal_id", "artifact_hash"},
		"decision":     {"principal_id", "task_id", "artifact_hash"},
		"mcp_tokens":   {"principal_id"},
	} {
		for _, col := range cols {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'sage' AND table_name = $1 AND column_name = $2`,
				table, col).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s.%s: %d %v", table, col, n, err)
			}
		}
	}
	// A re-run of the Ask migration must not bring the old constraint back.
	bootstrapWithRetry(t, ctx, pool)
	var oldCheck, v2 int
	var validated bool
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE conname = 'action_queue_proposed_via_check'),
		count(*) FILTER (WHERE conname = 'action_queue_proposed_via_v2'),
		bool_and(convalidated) FILTER (WHERE conname = 'action_queue_proposed_via_v2')
		FROM pg_constraint WHERE conrelid = 'sage.action_queue'::regclass`).
		Scan(&oldCheck, &v2, &validated); err != nil {
		t.Fatal(err)
	}
	if oldCheck != 0 || v2 != 1 || !validated {
		t.Fatalf("old %d v2 %d validated %v", oldCheck, v2, validated)
	}
	insert := `INSERT INTO sage.action_queue (proposed_sql, action_risk, proposed_via,
		proposed_by) VALUES ('SELECT 1', 'safe', $1, 'mig-test')`
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE proposed_by = 'mig-test'`)
	})
	for _, via := range []string{"agent", "ask_sage"} {
		if _, err := pool.Exec(ctx, insert, via); err != nil {
			t.Fatalf("proposed_via %s: %v", via, err)
		}
	}
	if _, err := pool.Exec(ctx, insert, "chat"); err == nil {
		t.Fatal("proposed_via chat was accepted")
	}
}
