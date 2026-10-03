package schema

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The decision ledger's indexes are created by Bootstrap (the ledger
// migration), never by a concurrent build running beside it: a CREATE
// INDEX CONCURRENTLY on sage.decision deadlocked a fleet reload's
// bootstrap, whose existing CREATE INDEX IF NOT EXISTS statements lock the
// table (found on PostgreSQL 18). The migration skips what exists, so a
// re-bootstrap takes no lock on sage.decision.

const coordinatorIndexDDL = `CREATE INDEX CONCURRENTLY IF NOT EXISTS
	idx_decision_schema_guard_targets ON sage.decision USING gin (target_objects)
	WHERE feature = 'schema_guard'`

func schemaGuardIndex(t *testing.T, pool *pgxpool.Pool) (oid uint32, valid bool, def string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT c.oid, i.indisvalid,
		pg_get_indexdef(c.oid) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE n.nspname = 'sage' AND c.relname = 'idx_decision_schema_guard_targets'`).
		Scan(&oid, &valid, &def)
	if err != nil {
		t.Fatalf("read schema guard index: %v", err)
	}
	return oid, valid, def
}

func dropSchemaGuardIndex(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"DROP INDEX IF EXISTS sage.idx_decision_schema_guard_targets"); err != nil {
		t.Fatalf("drop schema guard index: %v", err)
	}
}

func TestBootstrapCreatesThePartialGINIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dropSchemaGuardIndex(t, pool)
	bootstrapWithRetry(t, ctx, pool)
	oid, valid, def := schemaGuardIndex(t, pool)
	if !valid || !strings.Contains(def, "USING gin (target_objects)") ||
		!strings.Contains(def, "WHERE (feature = 'schema_guard'::text)") {
		t.Fatalf("index valid=%v def=%s, want a valid partial GIN index", valid, def)
	}
	bootstrapWithRetry(t, ctx, pool)
	if again, _, _ := schemaGuardIndex(t, pool); again != oid {
		t.Fatalf("re-bootstrap rebuilt the index (oid %d -> %d), want a no-op", oid, again)
	}
}

// The coordinator already created the index by hand on lifeos: the
// migration must leave it alone.
func TestBootstrapKeepsAnExistingIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dropSchemaGuardIndex(t, pool)
	if _, err := pool.Exec(ctx, coordinatorIndexDDL); err != nil {
		t.Fatalf("create index the coordinator's way: %v", err)
	}
	oid, _, _ := schemaGuardIndex(t, pool)
	bootstrapWithRetry(t, ctx, pool)
	if again, valid, _ := schemaGuardIndex(t, pool); again != oid || !valid {
		t.Fatalf("existing index replaced (oid %d -> %d, valid=%v)", oid, again, valid)
	}
}

// A failed concurrent build (by hand) leaves an INVALID index that IF NOT
// EXISTS would skip forever; the migration rebuilds it.
func TestBootstrapRebuildsAnInvalidIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	oid, _, _ := schemaGuardIndex(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = $1`, oid); err != nil {
		t.Skipf("cannot mark the index invalid (needs superuser): %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	again, valid, _ := schemaGuardIndex(t, pool)
	if !valid || again == oid {
		t.Fatalf("invalid index not rebuilt: oid %d -> %d valid=%v", oid, again, valid)
	}
}

// A complete ledger migration runs again without locking sage.decision,
// so it cannot wait on (or deadlock with) a writer or an index build.
func TestCompleteLedgerMigrationTakesNoLockOnDecision(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx,
		"LOCK TABLE sage.decision IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock sage.decision: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET lock_timeout = '1s'"); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET lock_timeout") }()
	start := time.Now()
	if _, err := conn.Exec(ctx, ddlDecisionLedger()); err != nil {
		t.Fatalf("re-running the complete ledger migration waited for a lock: %v", err)
	}
	// The lock is held throughout, so a lock wait could only end in the
	// 1 s lock_timeout error above; the duration only guards against a hang
	// and is generous for a loaded runner.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("re-run took %v, want no lock wait", elapsed)
	}
}
