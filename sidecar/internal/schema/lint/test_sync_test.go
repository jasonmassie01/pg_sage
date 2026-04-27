package lint

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// destructiveTestLockKey matches the key used in internal/schema and
// internal/store so every package that mutates sage.findings holds the
// same cross-package advisory lock. Without this, concurrent package
// test binaries (go test spawns one per package in parallel) can have
// one test's upsert race another test's resolveCleared, masking rows
// mid-test. Seen as a flake in TestIntegration_Runner_SagePersistence
// where Phase 2's re-upsert SELECT found the row but a concurrent
// scan flipped its status before the assertion re-read it.
const destructiveTestLockKey = "pg_sage_test_cross_pkg"

// serializeAcrossPackages acquires a session-scoped advisory lock on
// a dedicated pool connection (so MaxConns=2 is enough) and releases
// it via t.Cleanup. Other processes attempting the same lock block
// until release, serializing cross-package writers to sage.findings.
func serializeAcrossPackages(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn for cross-pkg lock: %v", err)
	}
	_, err = conn.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))",
		destructiveTestLockKey)
	if err != nil {
		conn.Release()
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(),
			"SELECT pg_advisory_unlock(hashtext($1))",
			destructiveTestLockKey)
		conn.Release()
	})
}
