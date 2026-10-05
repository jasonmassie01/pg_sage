package tuning

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/tuning"))
}

var (
	bootOnce sync.Once
	bootErr  error
	schemaN  atomic.Int64
)

// dbPool is the package's fixture database with the sage schema
// bootstrapped once. Integration tests skip only when no test server is
// configured (testdb's disabled DSN).
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(testdb.EnvName)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("no test PostgreSQL (%s): %v", testdb.EnvName, err)
	}
	t.Cleanup(pool.Close)
	bootOnce.Do(func() { bootErr = schema.Bootstrap(context.Background(), pool) })
	if bootErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootErr)
	}
	return pool
}

// freshSchema is a schema no other test in the package uses.
func freshSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("tune_%d_%d", time.Now().UnixNano()%1_000_000_000, schemaN.Add(1))
	if _, err := pool.Exec(context.Background(), "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+name+" CASCADE")
	})
	return name
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func serverVersion(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return v
}
