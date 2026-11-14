package collector

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// exclusivePool gives t a fresh database on the exclusive test server,
// held for t alone (testdb.Exclusive), and a pool on it: for tests that
// force a CHECKPOINT or read cluster-wide counters (ci-13).
func exclusivePool(t *testing.T, reason string) *pgxpool.Pool {
	t.Helper()
	return connectPool(t, testdb.Exclusive(t, reason))
}

// connectPool opens a pool on dsn for the rest of the test.
func connectPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping the test database: %v", err)
	}
	return pool
}
