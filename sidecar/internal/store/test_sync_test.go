package store

import (
	"context"
	"testing"

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

// serializeAcrossPackages acquires a session-scoped advisory lock so
// that destructive schema-package tests cannot DROP sage.* tables while
// a store test is reading them. Pool must be MaxConns=1 so the lock
// and the subsequent queries share a PG session.
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
