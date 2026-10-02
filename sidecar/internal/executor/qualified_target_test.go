package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 0 #4: destructive executor statements must name their schema. The
// text layer enforces it too, so builds without the parse-tree layer agree.
func TestValidateExecutorSQLRejectsUnqualifiedDestructiveTargets(t *testing.T) {
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY idx_orders_old",
		"DROP INDEX CONCURRENTLY IF EXISTS idx_orders_old",
		`DROP INDEX CONCURRENTLY "idx_orders_old"`,
		"DROP INDEX pg_class_oid_index",
		"ALTER TABLE orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		"ALTER TABLE IF EXISTS orders RESET (autovacuum_vacuum_scale_factor)",
	} {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
			continue
		}
		if !strings.Contains(err.Error(), "schema-qualified") {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want schema-qualified reason", sql, err)
		}
	}
}

func TestValidateExecutorSQLAcceptsQualifiedDestructiveTargets(t *testing.T) {
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY public.idx_orders_old",
		`DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_old"`,
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		// Non-destructive maintenance stays allowed unqualified.
		"VACUUM orders",
		"ANALYZE orders",
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestValidateExecutorSQLRejectsProtectedQualifiedTargets(t *testing.T) {
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY sage.idx_findings_category",
		"DROP INDEX CONCURRENTLY pg_catalog.pg_class_oid_index",
		"DROP INDEX CONCURRENTLY pg_toast.pg_toast_2619_index",
		"ALTER TABLE sage.findings SET (fillfactor = 50)",
	} {
		if err := ValidateExecutorSQL(sql); !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
}

// searchPathPool opens a pool whose sessions resolve unqualified names in
// sage first, the way a role named "sage" does through "$user".
func searchPathPool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = "sage, public"
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open search_path pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The attack is real: with sage first on the search_path an unqualified
// index name resolves into sage. pg_sage must refuse it before execution
// and leave the index in place; a qualified non-protected drop still runs.
func TestExecConcurrentlyRefusesUnqualifiedDropThroughSearchPath(t *testing.T) {
	base, ctx := requireDB(t)
	pool := searchPathPool(t, base)
	_, err := base.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
		CREATE TABLE IF NOT EXISTS sage.q4_probe (id int);
		CREATE INDEX IF NOT EXISTS q4_probe_idx ON sage.q4_probe (id);
		CREATE TABLE IF NOT EXISTS public.q4_target (id int);
		CREATE INDEX IF NOT EXISTS q4_target_idx ON public.q4_target (id)`)
	if err != nil {
		t.Fatalf("create probes: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(),
			"DROP TABLE IF EXISTS sage.q4_probe; DROP TABLE IF EXISTS public.q4_target")
	})
	var resolved string
	if err := pool.QueryRow(ctx, "SELECT to_regclass('q4_probe_idx')::text").
		Scan(&resolved); err != nil || resolved != "q4_probe_idx" {
		t.Fatalf("unqualified name did not resolve via search_path: %q %v", resolved, err)
	}
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY q4_probe_idx",
		"DROP INDEX CONCURRENTLY pg_class_oid_index",
	} {
		err := ExecConcurrently(ctx, pool, sql, 10*time.Second)
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ExecConcurrently(%q) = %v, want ErrDisallowedSQL", sql, err)
		}
	}
	assertRegclass(t, base, "sage.q4_probe_idx", true)
	assertRegclass(t, base, "pg_catalog.pg_class_oid_index", true)
	err = ExecConcurrently(ctx, pool, "DROP INDEX CONCURRENTLY public.q4_target_idx",
		10*time.Second)
	if err != nil {
		t.Fatalf("qualified drop: %v", err)
	}
	assertRegclass(t, base, "public.q4_target_idx", false)
}

func assertRegclass(t *testing.T, pool *pgxpool.Pool, name string, exists bool) {
	t.Helper()
	var found bool
	err := pool.QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", name).Scan(&found)
	if err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	if found != exists {
		t.Errorf("%s exists = %v, want %v", name, found, exists)
	}
}
