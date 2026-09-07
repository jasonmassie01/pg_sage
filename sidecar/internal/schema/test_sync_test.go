package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// destructiveTestLockKey is the advisory-lock key used to serialize any
// test that mutates sage.* schema objects across packages. Destructive
// tests in this package (schema/coverage_phase2) and any integration
// tests in sibling packages that operate on sage.* tables must acquire
// this lock for the full duration of their test case. Otherwise one
// process can DROP a table while another is using it — the flake that
// produced the pre-existing "relation sage.databases does not exist"
// failures before package-scoped fixture databases isolated destructive setup.
//
// The key is a stable string hashed with Postgres's hashtext() so the
// key space doesn't collide with Bootstrap's own lock (hashtext('pg_sage')).
const destructiveTestLockKey = "pg_sage_test_cross_pkg"

var destructiveTestLocks sync.Map

func acquireDestructiveTestLock(
	ctx context.Context, pool *pgxpool.Pool,
) (*pgxpool.Conn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))", destructiveTestLockKey)
	if err != nil {
		return nil, errors.Join(err, discardDestructiveTestConn(conn))
	}
	return conn, nil
}

func releaseDestructiveTestLock(conn *pgxpool.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(ctx,
		"SELECT pg_advisory_unlock(hashtext($1))",
		destructiveTestLockKey).Scan(&unlocked)
	if err == nil && unlocked {
		conn.Release()
		return nil
	}
	closeErr := discardDestructiveTestConn(conn)
	if err != nil {
		return errors.Join(err, closeErr)
	}
	return errors.Join(fmt.Errorf("cross-package lock not owned"), closeErr)
}

func discardDestructiveTestConn(conn *pgxpool.Conn) error {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return raw.Close(ctx)
}

// serializeAcrossPackages pins the advisory lock's owning connection until
// test cleanup. Repeated calls for one test are idempotent.
func serializeAcrossPackages(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	if _, ok := destructiveTestLocks.Load(t); ok {
		return
	}
	conn, err := acquireDestructiveTestLock(ctx, pool)
	if err != nil {
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	_, loaded := destructiveTestLocks.LoadOrStore(t, conn)
	if loaded {
		_ = releaseDestructiveTestLock(conn)
		return
	}
	t.Cleanup(func() {
		destructiveTestLocks.Delete(t)
		if err := releaseDestructiveTestLock(conn); err != nil {
			t.Errorf("release cross-package test lock: %v", err)
		}
	})
}
