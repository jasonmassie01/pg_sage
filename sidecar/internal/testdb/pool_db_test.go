package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-16 on the test server: inside Run the package shares one working
// pool per DSN, at most 8 connections however many tests use it, and Run
// closes every pool before it drops the fixture database.

func currentDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		"SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("query through the shared pool: %v", err)
	}
	return name
}

// Happy path and error propagation: Pool is the same working pool on the
// fixture database, PoolFor gives another DSN that outlives the tests (the
// server's own database) its own, and after Run both are closed and the
// registry refuses new ones.
func TestPool_SharedWithinRunAndClosedAfterIt(t *testing.T) {
	base := SkipUnlessLive(t)
	baseDB := currentDatabase(t, statsPool(t, base))
	var fixture, other *pgxpool.Pool
	var fixtureDB, otherDB string
	code := runFixtureWith(func() int {
		fixture = Pool(t)
		if again := Pool(t); again != fixture {
			t.Error("Pool returned two pools within one run")
		}
		if byDSN := PoolFor(t, os.Getenv(EnvName)); byDSN != fixture {
			t.Error("PoolFor(the fixture DSN) is not Pool")
		}
		fixtureDB = currentDatabase(t, fixture)
		other = PoolFor(t, base)
		otherDB = currentDatabase(t, other)
		return 0
	}, "internal/testdb_pool", "", nil)
	if code != 0 || t.Failed() {
		t.Fatalf("run exited %d", code)
	}
	if !strings.HasPrefix(fixtureDB, "pgsage_internal_testdb_pool_") ||
		otherDB != baseDB || other == fixture {
		t.Fatalf("pools reach %q and %q; want the fixture and the server's %q",
			fixtureDB, otherDB, baseDB)
	}
	for _, p := range []*pgxpool.Pool{fixture, other} {
		if _, err := p.Acquire(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "closed pool") {
			t.Fatalf("a pool used after Run: %v; want pgxpool's closed pool error", err)
		}
	}
	if _, err := sharedPools.get(os.Getenv(EnvName)); !errors.Is(err, errPoolsClosed) {
		t.Fatalf("registry after Run: %v; want the closed error", err)
	}
}

// Concurrent access and boundary: 50 parallel subtests share the pool, and
// it never holds more than 8 connections. (-race needs cgo: the Linux
// runner covers it.)
func TestPool_FiftyParallelSubtestsShareEightConnections(t *testing.T) {
	_ = SkipUnlessLive(t)
	var pool *pgxpool.Pool
	var mu sync.Mutex
	var peak int32
	code := runFixtureWith(func() int {
		pool = Pool(t)
		t.Run("group", func(t *testing.T) {
			for i := 0; i < 50; i++ {
				t.Run(fmt.Sprintf("s%02d", i), func(t *testing.T) {
					t.Parallel()
					p := Pool(t)
					var one int
					if err := p.QueryRow(context.Background(),
						"SELECT 1 FROM pg_sleep(0.02)").Scan(&one); err != nil || one != 1 {
						t.Errorf("query: %d, %v", one, err)
					}
					n := p.Stat().TotalConns()
					mu.Lock()
					peak = max(peak, n)
					mu.Unlock()
				})
			}
		})
		return 0
	}, "internal/testdb_pool_par", "", nil)
	if code != 0 || t.Failed() {
		t.Fatalf("run exited %d", code)
	}
	if got := pool.Config().MaxConns; got != 8 {
		t.Fatalf("MaxConns %d, want 8", got)
	}
	if peak < 1 || peak > 8 {
		t.Fatalf("peak %d connections, want 1..8", peak)
	}
}
