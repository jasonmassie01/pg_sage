package decommission

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/decommission"))
}

// freshDB is an empty database of its own, so tests that need the legacy
// tables absent or present never see each other's rows.
func freshDB(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// legacyDB is a fresh database carrying the v2.3.1 AgentDB tables.
func legacyDB(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := freshDB(t, label)
	applyLegacySchema(t, ctx, pool)
	return pool, ctx
}

func applyLegacySchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ddl, err := os.ReadFile("testdata/legacy_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}
}

// bootstrapped adds pg_sage's own schema (and so the acknowledgement table)
// to a database.
func bootstrapped(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
