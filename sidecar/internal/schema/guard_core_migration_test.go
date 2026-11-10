package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
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

// agentgov-04: sage.guard_principal_files belonged to a declarative
// principals package that had no caller and was removed. Bootstrap drops
// the table on upgraded installs and never creates it on fresh ones, and
// it leaves the principals and identity bindings alone.

// legacyPrincipalFilesDDL is the table as the removed E2 migration made it.
const legacyPrincipalFilesDDL = `CREATE TABLE sage.guard_principal_files (
    principal_id text PRIMARY KEY
                 REFERENCES sage.guard_principals(id) ON DELETE CASCADE,
    file_name    text NOT NULL CHECK (file_name ~ '^[a-z][a-z0-9-]{1,62}$'),
    applied_by   text NOT NULL,
    applied_at   timestamptz NOT NULL DEFAULT now()
)`

// No concurrent-bootstrap test: Bootstrap holds its advisory lock, so one
// drop runs per database at a time (bootstrap_lock_test.go covers the lock).

func TestGuardCoreMigration_DropsPrincipalFiles(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "schema_principal_files")
	const id = "agp_aaaaaaaaaaaaaaaaaaag"
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sage.guard_principals (id, name, profile, created_by)
			VALUES ($1, 'file-managed', 'legacy', 'test')`, []any{id}},
		{`INSERT INTO sage.guard_identity_bindings (issuer, subject, principal_id,
			created_by) VALUES ('https://idp.test', 'sub-1', $1, 'test')`, []any{id}},
		{legacyPrincipalFilesDDL, nil},
		{`INSERT INTO sage.guard_principal_files (principal_id, file_name, applied_by)
			VALUES ($1, 'agents-prod', 'test')`, []any{id}},
	} {
		if _, err := pool.Exec(ctx, seed.sql, seed.args...); err != nil {
			t.Fatalf("seed an install from before the removal: %v: %s", err, seed.sql)
		}
	}
	for run := range 2 { // the drop, then a re-run that must be a no-op
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d over a legacy principal files table: %v", run, err)
		}
		assertPrincipalFilesGone(t, pool)
		assertPrincipalKept(t, pool, id)
	}
}

// assertPrincipalFilesGone fails unless sage.guard_principal_files is gone.
func assertPrincipalFilesGone(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var gone bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regclass('sage.guard_principal_files') IS NULL`).Scan(&gone); err != nil {
		t.Fatalf("look up sage.guard_principal_files: %v", err)
	}
	if !gone {
		t.Fatal("sage.guard_principal_files still exists after bootstrap")
	}
}

// assertPrincipalKept fails unless the principal, its binding and the
// binding's foreign key to sage.guard_principals all survived.
func assertPrincipalKept(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var name, status, bound string
	var fks int
	if err := pool.QueryRow(context.Background(), `SELECT p.name, p.status, b.subject,
		(SELECT count(*) FROM pg_constraint
		  WHERE conrelid = 'sage.guard_identity_bindings'::regclass AND contype = 'f'
		    AND confrelid = 'sage.guard_principals'::regclass)
		FROM sage.guard_principals p
		JOIN sage.guard_identity_bindings b ON b.principal_id = p.id
		WHERE p.id = $1`, id).Scan(&name, &status, &bound, &fks); err != nil {
		t.Fatalf("principal %s or its identity binding is gone: %v", id, err)
	}
	if name != "file-managed" || status != "active" || bound != "sub-1" || fks != 1 {
		t.Fatalf("principal %s changed: name %q status %q binding %q fks %d",
			id, name, status, bound, fks)
	}
}

// A fresh database bootstraps without ever creating the table: an event
// trigger records every table the bootstrap creates.
func TestGuardCoreMigration_FreshNeverCreatesPrincipalFiles(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "schema_principal_files_fresh"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, createdTablesTriggerSQL); err != nil {
		t.Fatalf("install the created-tables event trigger (needs superuser): %v", err)
	}
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", run, err)
		}
	}
	var created, principalFiles int
	if err := pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE identity = 'sage.guard_principal_files')
		FROM public.created_tables`).Scan(&created, &principalFiles); err != nil {
		t.Fatalf("read created tables: %v", err)
	}
	if created == 0 {
		t.Fatal("the event trigger saw no table created; the test cannot judge")
	}
	if principalFiles != 0 {
		t.Fatalf("bootstrap created sage.guard_principal_files %d time(s)", principalFiles)
	}
	assertPrincipalFilesGone(t, pool)
}

// createdTablesTriggerSQL logs every CREATE TABLE into public.created_tables.
const createdTablesTriggerSQL = `
CREATE TABLE public.created_tables (identity text NOT NULL);
CREATE FUNCTION public.log_created_tables() RETURNS event_trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    INSERT INTO public.created_tables (identity)
    SELECT object_identity FROM pg_event_trigger_ddl_commands()
     WHERE command_tag = 'CREATE TABLE';
END $fn$;
CREATE EVENT TRIGGER log_created_tables ON ddl_command_end
    WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION public.log_created_tables();
`

// The migration text itself: no CREATE of the table, an idempotent drop
// guarded by a catalog check, and a bounded lock wait.
func TestDDLGuardIdentityBindings_DropsPrincipalFiles(t *testing.T) {
	ddl := strings.Join(strings.Fields(ddlGuardIdentityBindings), " ")
	if strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS sage.guard_principal_files") {
		t.Error("ddlGuardIdentityBindings still creates sage.guard_principal_files")
	}
	for _, want := range []string{
		"DROP TABLE IF EXISTS sage.guard_principal_files;",
		"to_regclass('sage.guard_principal_files') IS NOT NULL",
		"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.guard_identity_bindings",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("ddlGuardIdentityBindings lacks %q", want)
		}
	}
}
