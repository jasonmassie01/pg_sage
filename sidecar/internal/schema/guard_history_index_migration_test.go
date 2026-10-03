package schema

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema guard's history read (dogfood lifeos, v1.8.3: 12.7 s a scan
// over 252,974 legacy rows) looks up the newest row of each identity
// through idx_decision_schema_guard_key and counts dry runs and external
// reversions through idx_decision_schema_guard_counted. The old GIN index
// over every schema guard row (idx_decision_schema_guard_targets) served
// only that read and is retired.

const retiredGuardIndexDDL = `CREATE INDEX CONCURRENTLY IF NOT EXISTS
	idx_decision_schema_guard_targets ON sage.decision USING gin (target_objects)
	WHERE feature = 'schema_guard'`

func guardIndexOID(t *testing.T, pool *pgxpool.Pool, name string) uint32 {
	t.Helper()
	var oid *uint32
	if err := pool.QueryRow(context.Background(),
		"SELECT to_regclass('sage.' || $1)::oid", name).Scan(&oid); err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	if oid == nil {
		return 0
	}
	return *oid
}

func TestGuardHistoryIndexMigration_CreatesTheIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	key := indexDefinition(t, pool, "idx_decision_schema_guard_key")
	if !strings.Contains(key, "USING btree (((evidence ->> 'invariant_key'::text)), id DESC)") ||
		!strings.Contains(key, "WHERE (feature = 'schema_guard'::text)") {
		t.Fatalf("idx_decision_schema_guard_key = %s, want a partial btree on "+
			"(invariant_key, id DESC) of schema guard rows", key)
	}
	counted := indexDefinition(t, pool, "idx_decision_schema_guard_counted")
	if !strings.Contains(counted, "USING gin (target_objects)") ||
		!strings.Contains(counted, "'dry_run'::text") {
		t.Fatalf("idx_decision_schema_guard_counted = %s, want a partial GIN index", counted)
	}
	if oid := guardIndexOID(t, pool, "idx_decision_schema_guard_targets"); oid != 0 {
		t.Fatalf("retired idx_decision_schema_guard_targets still exists (oid %d)", oid)
	}
}

// A database carrying the retired index (built by hand on lifeos) loses it
// at the next bootstrap; the new indexes are kept (same oids).
func TestGuardHistoryIndexMigration_RetiresTheOldGINIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, retiredGuardIndexDDL); err != nil {
		t.Fatalf("create the retired index by hand: %v", err)
	}
	key := guardIndexOID(t, pool, "idx_decision_schema_guard_key")
	counted := guardIndexOID(t, pool, "idx_decision_schema_guard_counted")
	bootstrapWithRetry(t, ctx, pool)
	if oid := guardIndexOID(t, pool, "idx_decision_schema_guard_targets"); oid != 0 {
		t.Fatalf("retired index survived the bootstrap (oid %d)", oid)
	}
	if key == 0 || counted == 0 ||
		guardIndexOID(t, pool, "idx_decision_schema_guard_key") != key ||
		guardIndexOID(t, pool, "idx_decision_schema_guard_counted") != counted {
		t.Fatalf("new indexes missing or rebuilt (key %d, counted %d)", key, counted)
	}
}

// guardHistoryMigration is the registered migration that builds the
// schema guard history indexes.
func guardHistoryMigration(t *testing.T) string {
	t.Helper()
	for _, statement := range migrationStatements() {
		if strings.Contains(statement, "idx_decision_schema_guard_key") {
			return statement
		}
	}
	t.Fatal("no registered migration builds idx_decision_schema_guard_key")
	return ""
}

// A complete migration runs again without locking sage.decision: no wait
// on (or deadlock with) a writer or an index build.
func TestGuardHistoryIndexMigration_RerunTakesNoLockOnDecision(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	ddl := guardHistoryMigration(t)
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
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("re-running the complete migration waited for a lock: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("re-run took %v, want no lock wait", elapsed)
	}
}

// An INVALID key index (a failed build by hand) is rebuilt.
func TestGuardHistoryIndexMigration_RebuildsAnInvalidKeyIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	oid := guardIndexOID(t, pool, "idx_decision_schema_guard_key")
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = $1`, oid); err != nil {
		t.Skipf("cannot mark the index invalid (needs superuser): %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var valid bool
	again := guardIndexOID(t, pool, "idx_decision_schema_guard_key")
	if err := pool.QueryRow(ctx, "SELECT indisvalid FROM pg_index WHERE indexrelid = $1",
		again).Scan(&valid); err != nil || !valid || again == oid {
		t.Fatalf("invalid index not rebuilt: oid %d -> %d valid=%v err=%v", oid, again,
			valid, err)
	}
}
