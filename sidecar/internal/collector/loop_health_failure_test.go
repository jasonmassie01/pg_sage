package collector

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-06 review: the two cycle failures past the breaker (the
// collection itself, then the snapshot persist) are failed runs too.

// No concurrent access tests here: a collector runs one cycle at a time,
// and the tracker's locking is tested in selfbudget.

// Error propagation: a collection that cannot reach the database is a
// failed run, not a skipped one, and records no success.
func TestTrackedCycle_CollectFailureIsALoopError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	rec := &logRecorder{}
	c := New(pool, testConfig(), 170000, rec.log)
	c.loadDB = &fakeLoadDB{} // low load: the breaker lets the cycle run
	c.loops = selfbudget.NewLoops()
	err = c.cycle(ctx, &fakeTicker{})
	if err == nil || errors.Is(err, ErrCycleSkipped) ||
		!strings.HasPrefix(err.Error(), "collect: ") {
		t.Fatalf("cycle on an unreachable database = %v, want a collect error", err)
	}
	c.trackedCycle(ctx, &fakeTicker{})
	s := c.loops.Snapshot()["collector"]
	if s.Runs != 1 || s.Errors != 1 || !s.LastSuccess.IsZero() {
		t.Fatalf("collector loop = %+v, want 1 failed run and no last success", s)
	}
}

// Integration and error propagation: a snapshot collected but not stored
// (here: a database with no sage schema) is a failed run.
func TestTrackedCycle_PersistFailureIsALoopError(t *testing.T) {
	pool := freshDatabasePool(t)
	ctx := context.Background()
	rec := &logRecorder{}
	c := New(pool, testConfig(), serverVersion(t, pool), rec.log)
	c.loops = selfbudget.NewLoops()
	err := c.cycle(ctx, &fakeTicker{})
	if err == nil || !strings.HasPrefix(err.Error(), "persist snapshot: ") {
		t.Fatalf("cycle without sage.snapshots = %v, want a persist error", err)
	}
	if c.LatestSnapshot() == nil {
		t.Fatal("the collected snapshot was not kept in memory")
	}
	c.trackedCycle(ctx, &fakeTicker{})
	s := c.loops.Snapshot()["collector"]
	if s.Runs != 1 || s.Errors != 1 || !s.LastSuccess.IsZero() {
		t.Fatalf("collector loop = %+v, want 1 failed run and no last success", s)
	}
}

// freshDatabasePool creates an empty database (no sage schema) on the
// live server and returns a pool to it; the database is dropped after.
func freshDatabasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := testPool(t)
	ctx := context.Background()
	name := fmt.Sprintf("obs06_persist_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+
			" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}
