package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one action owns its pooled session; the
// pool's own concurrency is selfmonitor's (20 concurrent acquires).

// observationPool is one session configured exactly as pg_sage's pools.
func observationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func sessionPath(t *testing.T, pool *pgxpool.Pool) (string, int) {
	t.Helper()
	var path string
	var pid int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('search_path'), pg_backend_pid()").Scan(&path, &pid); err != nil {
		t.Fatal(err)
	}
	return path, pid
}

// State transition: a top-level action session (CONCURRENTLY, VACUUM)
// runs on the executor's pin and returns its connection to the pool still
// on the observation pin. A RESET would restore the role's path instead.
func TestTopLevelSessionReturnsTheObservationPin(t *testing.T) {
	for _, runErr := range []error{nil, errors.New("ddl failed")} {
		pool := observationPool(t)
		_, before := sessionPath(t, pool)
		err := withTopLevelSession(context.Background(), pool, "SELECT 1", time.Minute,
			ddlOpts{lockTimeoutMs: 1000}, func(s pooledExecutor) error {
				if _, err := s.Exec(context.Background(), "SELECT 1"); err != nil {
					return err
				}
				return runErr
			})
		if !errors.Is(err, runErr) {
			t.Fatalf("withTopLevelSession = %v, want %v", err, runErr)
		}
		after, pid := sessionPath(t, pool)
		if after != testdb.PinnedPath {
			t.Fatalf("run error %v: search_path after the action = %q, want %q",
				runErr, after, testdb.PinnedPath)
		}
		if pid != before {
			t.Fatalf("run error %v: session %d replaced by %d; want the clean session "+
				"returned to the pool", runErr, before, pid)
		}
	}
}
