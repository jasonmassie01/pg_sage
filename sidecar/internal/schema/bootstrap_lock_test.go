package schema

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func requireMultiConnDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("SAGE_TEST_DATABASE_URL")
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	poolCfg.MinConns = 0
	poolCfg.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	return pool
}

func advisoryLocksForPID(
	t *testing.T, pool *pgxpool.Pool, backendPID uint32,
) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var count int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_locks
		WHERE pid = $1
		  AND locktype = 'advisory'
		  AND granted`, backendPID).Scan(&count)
	if err != nil {
		t.Fatalf("count advisory locks for backend %d: %v", backendPID, err)
	}
	return count
}

func waitForBootstrapLockWaiters(
	t *testing.T, pool *pgxpool.Pool, want int,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		var count int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
			  AND query LIKE '%pg_advisory_lock%'`).Scan(&count)
		cancel()
		if err == nil && count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe %d concurrent bootstrap lock waiters", want)
}

func TestAdvisoryLock_PinsOwningConnectionUntilRelease(t *testing.T) {
	pool := requireMultiConnDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lock, err := acquireAdvisoryLock(ctx, pool, time.Second)
	if err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	backendPID := lock.conn.Conn().PgConn().PID()
	if got := advisoryLocksForPID(t, pool, backendPID); got != 1 {
		t.Fatalf("owning backend has %d advisory locks, want 1", got)
	}

	if err := lock.Release(ctx); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}
	if got := advisoryLocksForPID(t, pool, backendPID); got != 0 {
		t.Fatalf("owning backend has %d advisory locks after release, want 0", got)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("idempotent second release: %v", err)
	}
}

func TestDestructiveTestLock_PinsOwningConnectionUntilRelease(t *testing.T) {
	pool := requireMultiConnDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := acquireDestructiveTestLock(ctx, pool)
	if err != nil {
		t.Fatalf("acquire destructive test lock: %v", err)
	}
	backendPID := conn.Conn().PgConn().PID()
	if got := advisoryLocksForPID(t, pool, backendPID); got != 1 {
		t.Fatalf("owning backend has %d advisory locks, want 1", got)
	}

	if err := releaseDestructiveTestLock(conn); err != nil {
		t.Fatalf("release destructive test lock: %v", err)
	}
	if got := advisoryLocksForPID(t, pool, backendPID); got != 0 {
		t.Fatalf("owning backend has %d advisory locks after release, want 0", got)
	}
}

func TestAdvisoryLock_TimesOutWhileAnotherSessionOwnsLock(t *testing.T) {
	pool := requireMultiConnDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	owner, err := acquireAdvisoryLock(ctx, pool, time.Second)
	if err != nil {
		t.Fatalf("acquire owner lock: %v", err)
	}
	t.Cleanup(func() { _ = owner.Release(context.Background()) })

	started := time.Now()
	_, err = acquireAdvisoryLock(context.Background(), pool, 150*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("contending lock acquisition unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock timeout error = %v, want context deadline exceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("lock timeout took %s, want less than 1s", elapsed)
	}
	if got := advisoryLocksForPID(
		t, pool, owner.conn.Conn().PgConn().PID(),
	); got != 1 {
		t.Fatalf("owner backend has %d locks after contender timeout, want 1", got)
	}

	if err := owner.Release(ctx); err != nil {
		t.Fatalf("release owner lock: %v", err)
	}
}

func TestWithAdvisoryLock_ReleasesAfterCallbackError(t *testing.T) {
	pool := requireMultiConnDB(t)
	wantErr := errors.New("bootstrap failed")
	var backendPID uint32

	err := withAdvisoryLock(
		context.Background(), pool, time.Second,
		func(conn *pgxpool.Conn) error {
			backendPID = conn.Conn().PgConn().PID()
			return wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("withAdvisoryLock error = %v, want %v", err, wantErr)
	}
	if got := advisoryLocksForPID(t, pool, backendPID); got != 0 {
		t.Fatalf("callback backend has %d locks after error, want 0", got)
	}
}

func TestBootstrap_ConcurrentCallsSerializeTerminateAndRelease(t *testing.T) {
	pool := requireMultiConnDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	owner, err := acquireAdvisoryLock(ctx, pool, time.Second)
	if err != nil {
		t.Fatalf("acquire blocking lock: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			ready.Done()
			<-start
			errs <- Bootstrap(ctx, pool)
		}()
	}
	ready.Wait()
	close(start)

	// Both bootstraps must be waiting while a different session owns
	// the lock. Releasing that exact owning connection lets them run
	// sequentially and return; neither may retain the session lock.
	waitForBootstrapLockWaiters(t, pool, 2)
	if err := owner.Release(ctx); err != nil {
		t.Fatalf("release blocking lock: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent Bootstrap %d: %v", i+1, err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent Bootstrap calls did not terminate")
		}
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
	defer checkCancel()
	probe, err := acquireAdvisoryLock(checkCtx, pool, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("bootstrap advisory lock leaked after concurrent calls: %v", err)
	}
	if err := probe.Release(checkCtx); err != nil {
		t.Fatalf("release probe lock: %v", err)
	}
}

func TestBootstrap_ContextDeadlineBoundsLockWait(t *testing.T) {
	pool := requireMultiConnDB(t)
	ownerCtx, ownerCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ownerCancel()
	owner, err := acquireAdvisoryLock(ownerCtx, pool, time.Second)
	if err != nil {
		t.Fatalf("acquire owner lock: %v", err)
	}
	t.Cleanup(func() { _ = owner.Release(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = Bootstrap(ctx, pool)
	if err == nil {
		t.Fatal("Bootstrap unexpectedly acquired a contended lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Bootstrap exceeded caller deadline by too much: %s", elapsed)
	}
	if got := advisoryLocksForPID(
		t, pool, owner.conn.Conn().PgConn().PID(),
	); got != 1 {
		t.Fatalf("owner backend has %d locks, want 1", got)
	}
}

func TestAdvisoryLock_ReleaseReportsClosedConnection(t *testing.T) {
	pool := requireMultiConnDB(t)
	lock, err := acquireAdvisoryLock(
		context.Background(), pool, time.Second,
	)
	if err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	backendPID := lock.conn.Conn().PgConn().PID()
	if err := lock.conn.Conn().PgConn().Close(context.Background()); err != nil {
		t.Fatalf("close owning connection: %v", err)
	}
	if err := lock.Release(context.Background()); err == nil {
		t.Fatal("release on closed owning connection unexpectedly succeeded")
	}
	if got := advisoryLocksForPID(t, pool, backendPID); got != 0 {
		t.Fatalf("closed backend has %d advisory locks, want 0", got)
	}
}
