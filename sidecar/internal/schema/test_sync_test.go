package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// destructiveTestLockKey is the advisory-lock key used to serialize any
// test that mutates sage.* schema objects across packages. Destructive
// tests in this package (schema/coverage_phase2) and any integration
// tests in sibling packages that operate on sage.* tables must acquire
// this lock for the full duration of their test case. Otherwise one
// process can DROP a table while another is using it — the flake that
// produced the pre-existing "relation sage.databases does not exist"
// failures when `go test -tags=integration ./...` ran without -p 1.
//
// The key is a stable string hashed with Postgres's hashtext() so the
// key space doesn't collide with Bootstrap's own lock (hashtext('pg_sage')).
const destructiveTestLockKey = "pg_sage_test_cross_pkg"

// serializeAcrossPackages acquires a session-scoped advisory lock and
// registers a t.Cleanup that releases it. The lock is BLOCKING, so
// another test in a different OS process that also calls this helper
// will wait rather than racing. Pool must have MaxConns=1 so every
// acquire/release lands on the same PG session.
func serializeAcrossPackages(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))", destructiveTestLockKey)
	if err != nil {
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			"SELECT pg_advisory_unlock(hashtext($1))",
			destructiveTestLockKey)
	})
}
