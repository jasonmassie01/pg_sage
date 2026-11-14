package executor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// exclusiveDB is requireDB on a fresh database of the exclusive test
// server, held for t alone (testdb.Exclusive): for tests that force a
// CHECKPOINT or count the checkpointer's cluster-wide work (ci-13).
func exclusiveDB(t *testing.T, reason string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.Exclusive(t, reason)
	ctx := context.Background()
	setup, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	pool, err := pgxpool.New(setup, dsn)
	if err != nil {
		t.Fatalf("connect to the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(setup, pool); err != nil {
		t.Fatalf("bootstrap the exclusive test database: %v", err)
	}
	if err := schema.MigrateConfigSchema(setup, pool); err != nil {
		t.Fatalf("config migration on the exclusive test database: %v", err)
	}
	return pool, ctx
}
