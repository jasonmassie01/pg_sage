package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrency test beyond the session-scope tests: one connection per
// action under the DDL lease.

// plantLowerAttack reproduces CVE-2018-1058 against pg_sage's own DDL: a
// second, low-privilege role plants public.lower(varchar) IMMUTABLE, which
// beats pg_catalog.lower(text) for a varchar argument on the default path.
func plantLowerAttack(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	role := fmt.Sprintf("sage_sp_attacker_%d", os.Getpid())
	setup := []string{
		"DROP TABLE IF EXISTS public.sp_attack_t",
		"DROP FUNCTION IF EXISTS public.lower(varchar)",
		"DROP ROLE IF EXISTS " + role,
		"CREATE ROLE " + role + " NOLOGIN",
		// PG15+ revoked PUBLIC CREATE on public; grant what PG14 gives everyone.
		"GRANT USAGE, CREATE ON SCHEMA public TO " + role,
		"CREATE TABLE public.sp_attack_t (id int, email varchar(100))",
		"INSERT INTO public.sp_attack_t VALUES (1, 'Alice@Example.COM')",
	}
	for _, sql := range setup {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("attack setup %q: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, sql := range []string{
			"DROP TABLE IF EXISTS public.sp_attack_t",
			"DROP FUNCTION IF EXISTS public.lower(varchar)",
			"REVOKE ALL ON SCHEMA public FROM " + role,
			"DROP ROLE IF EXISTS " + role,
		} {
			if _, err := pool.Exec(cctx, sql); err != nil {
				t.Errorf("attack cleanup %q: %v", sql, err)
			}
		}
	})
	plantAsRole(t, ctx, pool, role)
	// Guard against a vacuous test: on the role's default path the planted
	// function really does win.
	var got string
	if err := pool.QueryRow(ctx,
		"SELECT lower(email) FROM public.sp_attack_t").Scan(&got); err != nil {
		t.Fatalf("probe planted function: %v", err)
	}
	if got != "planted" {
		t.Fatalf("default-path lower(varchar) = %q, want the planted function", got)
	}
}

// plantAsRole creates public.lower(varchar) IMMUTABLE as the attacker role.
func plantAsRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire attacker connection: %v", err)
	}
	defer conn.Release()
	for _, sql := range []string{
		"SET ROLE " + role,
		`CREATE FUNCTION public.lower(varchar) RETURNS text
		 LANGUAGE sql IMMUTABLE AS $$ SELECT 'planted'::text $$`,
		"RESET ROLE",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("attacker %q: %v", sql, err)
		}
	}
}

// indexFunctionDeps lists the schema-qualified functions an index has
// pg_depend rows for.
func indexFunctionDeps(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string,
) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT n.nspname || '.' || p.proname || '(' ||
		       pg_catalog.pg_get_function_identity_arguments(p.oid) || ')'
		  FROM pg_catalog.pg_depend d
		  JOIN pg_catalog.pg_proc p ON p.oid = d.refobjid
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		 WHERE d.classid = 'pg_catalog.pg_class'::regclass
		   AND d.objid = $1::regclass
		   AND d.refclassid = 'pg_catalog.pg_proc'::regclass
		 ORDER BY 1`, index)
	if err != nil {
		t.Fatalf("read pg_depend for %s: %v", index, err)
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			t.Fatalf("scan dependency: %v", err)
		}
		deps = append(deps, dep)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dependencies: %v", err)
	}
	return deps
}

// Attack (fails without the pin): pg_sage's expression-index DDL binds
// pg_catalog.lower, never the planted public.lower, on both DDL paths.
func TestSearchPathPinBlocksPlantedFunction(t *testing.T) {
	pool, ctx := requireDB(t)
	cases := []struct {
		name, index, sql string
		exec             func(context.Context, *pgxpool.Pool, string, time.Duration,
			...DDLOption) error
	}{
		{"concurrently", "public.sp_attack_cic",
			"CREATE INDEX CONCURRENTLY sp_attack_cic ON public.sp_attack_t ((lower(email)))",
			ExecConcurrently},
		{"transaction", "public.sp_attack_tx",
			"CREATE INDEX sp_attack_tx ON public.sp_attack_t ((lower(email)))",
			ExecInTransaction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plantLowerAttack(t, ctx, pool)
			if err := tc.exec(ctx, pool, tc.sql, time.Minute,
				WithLockTimeout(5000)); err != nil {
				t.Fatalf("pg_sage DDL: %v", err)
			}
			// pg_depend never records pinned pg_catalog objects, so any
			// function dependency at all is one outside pg_catalog.
			if deps := indexFunctionDeps(t, ctx, pool, tc.index); len(deps) != 0 {
				t.Fatalf("index %s depends on functions %q, want none outside pg_catalog",
					tc.index, deps)
			}
			var bindsCatalogLower bool
			if err := pool.QueryRow(ctx, `SELECT pg_catalog.strpos(i.indexprs::text,
				':funcid ' || 'pg_catalog.lower(text)'::regprocedure::oid || ' ') > 0
				FROM pg_catalog.pg_index i WHERE i.indexrelid = $1::regclass`,
				tc.index).Scan(&bindsCatalogLower); err != nil {
				t.Fatalf("read index expression: %v", err)
			}
			if !bindsCatalogLower {
				t.Fatalf("index %s expression does not call pg_catalog.lower(text)", tc.index)
			}
			var def string
			if err := pool.QueryRow(ctx, "SELECT pg_catalog.pg_get_indexdef($1::regclass)",
				tc.index).Scan(&def); err != nil {
				t.Fatalf("pg_get_indexdef: %v", err)
			}
			// pg_catalog.lower(text) needs the varchar cast; the planted
			// lower(varchar) would deparse without it.
			if !strings.Contains(def, "lower((email)::text)") {
				t.Fatalf("index definition %q does not use pg_catalog.lower(text)", def)
			}
		})
	}
}

// isUnqualifiedIndexTarget reports the validation refusal of a CREATE
// INDEX whose table names no schema (security-01).
func isUnqualifiedIndexTarget(err error) bool {
	return errors.Is(err, ErrDisallowedSQL) &&
		strings.Contains(err.Error(), "INDEX target must be schema-qualified")
}

// Invalid input: an unqualified target no longer resolves through public;
// since security-01 it is refused at validation, before PostgreSQL sees
// it, and builds nothing. The same statement qualified still succeeds (the
// generators qualify targets).
func TestSearchPathPinRejectsUnqualifiedTarget(t *testing.T) {
	pool, ctx := requireDB(t)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.sp_unq_t;
		CREATE TABLE public.sp_unq_t (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.sp_unq_t")
	})
	err := ExecConcurrently(ctx, pool,
		"CREATE INDEX CONCURRENTLY sp_unq_cic ON sp_unq_t (id)", time.Minute)
	if !isUnqualifiedIndexTarget(err) {
		t.Fatalf("unqualified CONCURRENTLY error = %v, want refused at validation", err)
	}
	err = ExecInTransaction(ctx, pool, "CREATE INDEX sp_unq_tx ON sp_unq_t (id)", time.Minute)
	if !isUnqualifiedIndexTarget(err) {
		t.Fatalf("unqualified transactional error = %v, want refused at validation", err)
	}
	var built int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class
		WHERE relname IN ('sp_unq_cic', 'sp_unq_tx')`).Scan(&built); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if built != 0 {
		t.Fatalf("%d indexes built from unqualified DDL, want 0", built)
	}
	if err := ExecConcurrently(ctx, pool,
		"CREATE INDEX CONCURRENTLY sp_unq_ok ON public.sp_unq_t (id)", time.Minute); err != nil {
		t.Fatalf("qualified CONCURRENTLY: %v", err)
	}
}

// Invalid input through the action path: since security-01 the operator
// action on an unqualified target is refused at validation, so it never
// reaches PostgreSQL and records no action.
func TestSearchPathPinFailsUnqualifiedManualAction(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = SetEmergencyStop(ctx, pool, false, "test")
	const sql = "CREATE INDEX CONCURRENTLY sp_unq_manual ON sp_unq_manual_t (id)"
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.sp_unq_manual_t;
		CREATE TABLE public.sp_unq_manual_t (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql)
		 VALUES ('test_sp_unqualified', 'warning', 'index', 'public.sp_unq_manual_t',
		         'unqualified target', '{}', 'rec', $1)
		 RETURNING id`, sql).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = pool.Exec(cctx, "DELETE FROM sage.findings WHERE id = $1", findingID)
		_, _ = pool.Exec(cctx, "DROP TABLE IF EXISTS public.sp_unq_manual_t")
	})
	cfg := &config.Config{Safety: config.SafetyConfig{
		DDLTimeoutSeconds: 30, LockTimeoutMs: 5000}}
	cfg.Trust.Level = "advisory"
	e := &Executor{pool: pool, cfg: cfg, recentActions: make(map[string]time.Time),
		logFn: func(string, string, ...any) {}, execMode: "auto"}
	withTestStandingGate(e)

	_, err := e.ExecuteManual(ctx, findingID, sql, "", nil)
	if !isUnqualifiedIndexTarget(err) {
		t.Fatalf("ExecuteManual error = %v, want refused at validation", err)
	}
	var actions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE finding_id = $1`, findingID).Scan(&actions); err != nil {
		t.Fatalf("read action_log: %v", err)
	}
	if actions != 0 {
		t.Fatalf("%d actions recorded for SQL refused at validation, want 0", actions)
	}
}
