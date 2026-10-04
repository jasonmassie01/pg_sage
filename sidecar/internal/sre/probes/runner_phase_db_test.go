package probes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Probe deadlines measure execution, not queueing (dogfood lifeos,
// 2026-10-04: xid_runway, 0.1 ms on the server, failed with
// deadline_exceeded while pg_sage's 3-connection pool was busy). Each
// phase has its own budget: the limiter queue, the pool acquire, and the
// statement (server execution, including lock waits). A failure names
// the phase that spent its budget, and a server-side timeout is retried
// once within the caller's deadline.

// smallPool is a pool of n connections to the package's fixture database.
func smallPool(t *testing.T, n int32) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = n, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// holdConn takes one pool connection and releases it after d.
func holdConn(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	d time.Duration) <-chan struct{} {
	t.Helper()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold a connection: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(d)
		c.Release()
	}()
	return released
}

const cheapSQL = "SELECT 1::int8 AS one LIMIT $1"

// A probe waiting for a busy pool longer than its statement budget (plus
// the 1 s client margin) still runs: the pool wait is not execution.
func TestRunner_PoolWaitDoesNotConsumeStatementBudget(t *testing.T) {
	pool, ctx := smallPool(t, 1)
	r := testRunner(t, pool, testSpec("cheap", cheapSQL))
	released := holdConn(t, ctx, pool, 1800*time.Millisecond)
	res := r.Run(ctx, "cheap", Args{})
	<-released
	if res.Status != StatusOK || res.Rows[0]["one"] != int64(1) {
		t.Fatalf("probe behind a busy pool = %+v, want ok (pool wait is not execution)", res)
	}
	if res.Timing.Acquire < 1500*time.Millisecond {
		t.Fatalf("acquire timing = %s, want the ~1.8 s the pool was held", res.Timing.Acquire)
	}
	if res.Timing.Execution <= 0 || res.Timing.Execution > time.Second {
		t.Fatalf("execution timing = %s, want the statement alone", res.Timing.Execution)
	}
	if res.Timing.Attempts != 1 || res.Phase != "" {
		t.Fatalf("attempts %d phase %q, want 1 and none", res.Timing.Attempts, res.Phase)
	}
	if res.ElapsedMS >= 1000 {
		t.Fatalf("elapsed_ms = %d: it must report the execution, not the pool wait",
			res.ElapsedMS)
	}
}

// A pool that stays busy past the acquire budget fails the probe in the
// pool_acquire phase, without a retry (the wait already had its budget).
func TestRunner_PoolAcquireTimeoutNamesPhase(t *testing.T) {
	pool, ctx := smallPool(t, 1)
	r := testRunner(t, pool, testSpec("cheap", cheapSQL))
	r.acquireWait = 300 * time.Millisecond
	released := holdConn(t, ctx, pool, 1500*time.Millisecond)
	start := time.Now()
	res := r.Run(ctx, "cheap", Args{})
	el := time.Since(start)
	<-released
	if res.Status != StatusError || res.Reason != "deadline_exceeded" ||
		res.Phase != PhasePoolAcquire {
		t.Fatalf("result = %+v, want error/deadline_exceeded in pool_acquire", res)
	}
	if res.Timing.Attempts != 1 || res.Timing.Execution != 0 {
		t.Fatalf("timing = %+v, want one attempt that never executed", res.Timing)
	}
	if res.Timing.Acquire < 250*time.Millisecond || el > 1200*time.Millisecond {
		t.Fatalf("acquire %s, elapsed %s: want the 300 ms acquire budget", res.Timing.Acquire,
			el)
	}
	_, err := rowsFor(res, "cheap")
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.Phase != PhasePoolAcquire {
		t.Fatalf("decode error = %v, want it to name pool_acquire", err)
	}
}

// Queue time (the sidecar-wide limiter) is recorded apart from execution.
func TestRunner_QueueWaitRecordedSeparately(t *testing.T) {
	pool, ctx := livePool(t)
	global := NewLimiter(1)
	r := NewRunner(pool, mustTestRegistry(t, testSpec("cheap", cheapSQL)), global)
	if err := global.acquire(ctx); err != nil {
		t.Fatalf("hold the slot: %v", err)
	}
	go func() { time.Sleep(1600 * time.Millisecond); global.release() }()
	res := r.Run(ctx, "cheap", Args{})
	if res.Status != StatusOK {
		t.Fatalf("queued probe = %+v, want ok", res)
	}
	if res.Timing.Queue < 1400*time.Millisecond || res.Timing.Execution > time.Second {
		t.Fatalf("timing = %+v, want ~1.6 s queued and a short execution", res.Timing)
	}
}

// Startup burst: many cheap probes from several runners at once on a
// 2-connection pool that other loops keep busy. None may fail.
func TestRunner_StartupBurstOnBusyPool(t *testing.T) {
	pool, ctx := smallPool(t, 2)
	global := NewLimiter(MaxSidecarConcurrency)
	reg := mustTestRegistry(t, testSpec("cheap", cheapSQL))
	runners := make([]*Runner, 6)
	for i := range runners {
		runners[i] = NewRunner(pool, reg, global)
	}
	stop := make(chan struct{})
	var hogs sync.WaitGroup
	for i := 0; i < 2; i++ { // two "collector" loops holding connections
		hogs.Add(1)
		go func() {
			defer hogs.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := pool.Acquire(ctx)
				if err != nil {
					return
				}
				time.Sleep(700 * time.Millisecond)
				c.Release()
			}
		}()
	}
	results := runConcurrently(ctx, 24, func(i int) Result {
		return runners[i%len(runners)].Run(ctx, "cheap", Args{})
	})
	close(stop)
	hogs.Wait()
	for i, res := range results {
		if res.Status != StatusOK {
			t.Errorf("probe %d = %s/%s in %q (timing %+v)", i, res.Status, res.Reason,
				res.Phase, res.Timing)
		}
	}
}

// lockedTable creates a table and holds an ACCESS EXCLUSIVE lock on it in
// a transaction of its own; the returned func releases it (idempotent).
func lockedTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) func() {
	t.Helper()
	if _, err := pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s; CREATE TABLE %s (id int)",
		name, name)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+name)
	})
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := holder.Exec(ctx, "LOCK TABLE "+name+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", name, err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = holder.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// A lock wait that ends between attempts is retried once and observed.
func TestRunner_LockWaitRetriedOnce(t *testing.T) {
	pool, ctx := livePool(t)
	release := lockedTable(t, ctx, pool, "sre_probe_retry_lock")
	r := testRunner(t, pool, testSpec("locked",
		"SELECT count(*) AS n FROM public.sre_probe_retry_lock LIMIT $1",
		func(s *Spec) { s.LockTimeout = 50 * time.Millisecond }))
	var seen []Result
	r.afterAttempt = func(attempt int, res Result) {
		seen = append(seen, res)
		if attempt == 1 {
			release()
		}
	}
	res := r.Run(ctx, "locked", Args{})
	if res.Status != StatusOK || res.Rows[0]["n"] != int64(0) {
		t.Fatalf("result = %+v, want ok after the retry", res)
	}
	if res.Timing.Attempts != 2 || len(seen) != 2 {
		t.Fatalf("attempts = %d (hook saw %d), want 2", res.Timing.Attempts, len(seen))
	}
	if seen[0].Reason != "lock_timeout" || seen[0].Phase != PhaseLockWait {
		t.Fatalf("first attempt = %s in %q, want lock_timeout in lock_wait", seen[0].Reason,
			seen[0].Phase)
	}
}

// A lock that outlasts the retry budget fails in the lock_wait phase after
// exactly two attempts.
func TestRunner_PersistentLockWaitNamesPhase(t *testing.T) {
	pool, ctx := livePool(t)
	lockedTable(t, ctx, pool, "sre_probe_held_lock")
	r := testRunner(t, pool, testSpec("locked",
		"SELECT count(*) AS n FROM public.sre_probe_held_lock LIMIT $1",
		func(s *Spec) { s.LockTimeout = 50 * time.Millisecond }))
	var attempts atomic.Int32
	r.afterAttempt = func(int, Result) { attempts.Add(1) }
	start := time.Now()
	res := r.Run(ctx, "locked", Args{})
	el := time.Since(start)
	if res.Status != StatusError || res.Reason != "lock_timeout" || res.Phase != PhaseLockWait {
		t.Fatalf("result = %+v, want error/lock_timeout in lock_wait", res)
	}
	if res.Timing.Attempts != probeAttempts || attempts.Load() != probeAttempts {
		t.Fatalf("attempts = %d (hook %d), want %d", res.Timing.Attempts, attempts.Load(),
			probeAttempts)
	}
	if el > 5*time.Second {
		t.Fatalf("a bounded retry took %s", el)
	}
}

// A lock held on pg_stat_statements (what a dealloc or a DBA's LOCK does
// to the statistics view) is a lock wait, not a deadline.
func TestRunner_LockOnStatStatementsIsLockWait(t *testing.T) {
	pool, ctx := livePool(t)
	var installed bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('pg_stat_statements') IS NOT NULL`).
		Scan(&installed); err != nil {
		t.Fatalf("check pg_stat_statements: %v", err)
	}
	if !installed {
		// The fixture installs it wherever the server has it (CI: always).
		t.Skip("pg_stat_statements is not installed on this server")
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		"LOCK TABLE pg_stat_statements IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock pg_stat_statements: %v", err)
	}
	r := testRunner(t, pool, testSpec("pgss",
		"SELECT count(*) AS n FROM public.pg_stat_statements LIMIT $1",
		func(s *Spec) { s.LockTimeout = 50 * time.Millisecond }))
	res := r.Run(ctx, "pgss", Args{})
	if res.Status != StatusError || res.Reason != "lock_timeout" || res.Phase != PhaseLockWait {
		t.Fatalf("result = %+v, want error/lock_timeout in lock_wait", res)
	}
}

// A statement timeout is retried once, then reported in server_execution.
func TestRunner_StatementTimeoutRetriedOnceThenReported(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("slow", "SELECT pg_sleep(30) AS s LIMIT $1",
		func(s *Spec) { s.StatementTimeout = 150 * time.Millisecond }))
	start := time.Now()
	res := r.Run(ctx, "slow", Args{})
	el := time.Since(start)
	if res.Status != StatusError || res.Reason != "statement_timeout" ||
		res.Phase != PhaseExecution {
		t.Fatalf("result = %+v, want error/statement_timeout in server_execution", res)
	}
	if res.Timing.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", res.Timing.Attempts)
	}
	if el < 300*time.Millisecond || el > 5*time.Second {
		t.Fatalf("elapsed %s: want two bounded 150 ms attempts", el)
	}
}

// No retry starts when the caller's deadline leaves no room for a whole
// attempt.
func TestRunner_NoRetryPastCallerDeadline(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("slow", "SELECT pg_sleep(30) AS s LIMIT $1",
		func(s *Spec) { s.StatementTimeout = 150 * time.Millisecond }))
	short, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	res := r.Run(short, "slow", Args{})
	if res.Status != StatusError || res.Reason != "statement_timeout" {
		t.Fatalf("result = %+v, want error/statement_timeout", res)
	}
	if res.Timing.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no room for a retry)", res.Timing.Attempts)
	}
}

// A non-timeout failure is never retried.
func TestRunner_PermanentFailureNotRetried(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("broken",
		"SELECT * FROM public.sre_probe_no_such_table LIMIT $1"))
	var attempts atomic.Int32
	r.afterAttempt = func(int, Result) { attempts.Add(1) }
	res := r.Run(ctx, "broken", Args{})
	if res.Status != StatusUnsupported || res.Reason != "undefined_table" ||
		attempts.Load() != 1 || res.Timing.Attempts != 1 {
		t.Fatalf("result = %+v after %d attempts, want unsupported once", res,
			attempts.Load())
	}
	if res.Phase != PhaseExecution {
		t.Fatalf("phase = %q, want server_execution", res.Phase)
	}
}

// The server version is read on the probe's own connection, inside the
// execution budget: a busy pool at startup does not fail the first probe.
func TestRunner_FirstProbeBehindBusyPoolReadsVersion(t *testing.T) {
	pool, ctx := smallPool(t, 1)
	r := testRunner(t, pool, testSpec("cheap", cheapSQL))
	released := holdConn(t, ctx, pool, 900*time.Millisecond)
	res := r.Run(ctx, "cheap", Args{})
	<-released
	if res.Status != StatusOK {
		t.Fatalf("first probe behind a busy pool = %+v, want ok", res)
	}
	if r.version < 140000 {
		t.Fatalf("server version = %d, want it read and cached", r.version)
	}
}
