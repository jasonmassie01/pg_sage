package autonomy

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// sageTaggedPool opens a pool configured like pg_sage's own.
func sageTaggedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pg_sage pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pg_sage is now tracked by pg_stat_statements (perf v1.8.3). Its own
// statements must not become "related queries" of a schema-guard target,
// or pg_sage would cite (and protect) tables only it reads.
func TestLoadStatementIndexExcludesPgSageStatements(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable: %v", err)
	}
	execAll(t, pool,
		"CREATE TABLE IF NOT EXISTS public.guard_self_probe (id bigint)",
		"CREATE TABLE IF NOT EXISTS public.guard_app_probe (id bigint)",
		"SELECT count(*) FROM public.guard_app_probe")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TABLE IF EXISTS public.guard_self_probe, public.guard_app_probe")
	})
	var n int64
	if err := sageTaggedPool(t).QueryRow(ctx,
		"SELECT count(*) FROM public.guard_self_probe").Scan(&n); err != nil {
		t.Fatalf("pg_sage statement: %v", err)
	}
	index, err := loadStatementIndex(ctx, pool)
	if err != nil {
		t.Fatalf("loadStatementIndex: %v", err)
	}
	if !index.known {
		t.Skip("pg_stat_statements is not preloaded on this server")
	}
	if ids := index.queryIDs("public.guard_self_probe"); len(ids) != 0 {
		t.Fatalf("pg_sage's own statement indexed as a related query: %v", ids)
	}
	if ids := index.queryIDs("public.guard_app_probe"); len(ids) == 0 {
		t.Fatal("the application statement is missing from the index")
	}
}

// A lock or a running statement of pg_sage's own session never makes a
// clone family look in use.
func TestLoadSessionsIgnoresPgSageSessions(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	execAll(t, pool, "DROP SCHEMA IF EXISTS guard_self_sess CASCADE",
		"CREATE SCHEMA guard_self_sess", "CREATE TABLE guard_self_sess.t (id int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS guard_self_sess CASCADE")
	})
	tx, err := sageTaggedPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE guard_self_sess.t IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	locked, statements, err := loadSessions(ctx, pool)
	if err != nil {
		t.Fatalf("loadSessions: %v", err)
	}
	if locked["guard_self_sess"] {
		t.Fatal("pg_sage's own lock counted as session activity")
	}
	if statements.mentionsSchema("guard_self_sess") {
		t.Fatal("pg_sage's own statement counted as session activity")
	}
}
