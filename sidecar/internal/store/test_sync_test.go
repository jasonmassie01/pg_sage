package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// destructiveTestLockKey must match internal/schema's constant of the
// same name. It's duplicated rather than imported because the schema
// helper lives in a _test.go file (not exported across packages).
//
// Untagged file (no //go:build integration) because it's used by both
// integration_helpers_test.go (tagged) and coverage_boost_test.go
// (untagged). Go's build tool scopes _test.go files to the test binary,
// so this doesn't leak into non-test builds.
const destructiveTestLockKey = "pg_sage_test_cross_pkg"

var storeDestructiveTestLocks sync.Map

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
	if _, ok := storeDestructiveTestLocks.Load(t); ok {
		return
	}
	conn, err := acquireDestructiveTestLock(ctx, pool)
	if err != nil {
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	_, loaded := storeDestructiveTestLocks.LoadOrStore(t, conn)
	if loaded {
		_ = releaseDestructiveTestLock(conn)
		return
	}
	t.Cleanup(func() {
		storeDestructiveTestLocks.Delete(t)
		if err := releaseDestructiveTestLock(conn); err != nil {
			t.Errorf("release cross-package test lock: %v", err)
		}
	})
}
