package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// executor-05 review: the build watch must work on the sidecar's default
// pool of config.DefaultPGMaxConnections (2). The gate and the build hold
// both connections, so a poll that needs a third waited until the build
// timed out on the gate: every CREATE INDEX CONCURRENTLY failed with a
// lock timeout and left an unbound INVALID index.
func TestBuildWatchOnTheDefaultPoolSize(t *testing.T) {
	_, ctx := requireDB(t)
	poolCfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	poolCfg.MaxConns = int32(config.DefaultPGMaxConnections)
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open the small pool: %v", err)
	}
	t.Cleanup(pool.Close)
	table := fmt.Sprintf("public.rmnt_pool_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (a int NOT NULL, b int NOT NULL)"+
		"; INSERT INTO "+table+" SELECT g, g % 7 FROM generate_series(1, 200) g"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }

	start := time.Now()
	ok := "CREATE INDEX CONCURRENTLY rmnt_pool_ok ON " + table + " (a)"
	if err := e.execConcurrentDDL(ctx, ok, nil, WithLockTimeout(3000)); err != nil {
		t.Fatalf("a build on a %d-connection pool failed: %v", poolCfg.MaxConns, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the build took %s: the gate held it until its hold expired", elapsed)
	}
	state := map[string]any{}
	bad := "CREATE INDEX CONCURRENTLY rmnt_pool_bad ON " + table + " ((b / 0))"
	err = e.execConcurrentDDL(ctx, bad, state, WithLockTimeout(3000))
	if err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("error = %v, want division by zero", err)
	}
	if c := cleanupOf(t, state); c.Outcome != remnantDropped || len(c.Indexes) != 1 {
		t.Fatalf("cleanup = %+v, want the fast failure's own index dropped", c)
	}
	if n := invalidCount(t, ctx, pool, table); n != 0 {
		t.Fatalf("%d INVALID indexes remain on %s", n, table)
	}
}
