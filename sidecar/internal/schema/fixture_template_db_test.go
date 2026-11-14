package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// ci-15 CHECK-28: the schema package tests bootstrap and migration
// behaviour, so its databases start empty from template0, never from the
// bootstrapped fixture template other packages opt into.
func TestFixtureDatabasesStartFromTemplate0(t *testing.T) {
	_ = testdb.SkipUnlessLive(t)
	if got := testdb.FixtureTemplate(); got != "template0" {
		t.Fatalf("the schema package fixture came from %q, want template0", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "tpl0check"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if n := ledgerRowCount(t, ctx, pool); n != 0 {
		t.Fatalf("a new database already has %d sage.schema_migrations rows, want 0", n)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n := ledgerRowCount(t, ctx, pool); n == 0 {
		t.Fatal("bootstrap recorded no sage.schema_migrations rows")
	}
}

// ledgerRowCount is the row count of sage.schema_migrations, 0 without it.
func ledgerRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('sage.schema_migrations') IS NOT NULL").Scan(&present); err != nil {
		t.Fatalf("look for sage.schema_migrations: %v", err)
	}
	if !present {
		return 0
	}
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count sage.schema_migrations: %v", err)
	}
	return n
}
