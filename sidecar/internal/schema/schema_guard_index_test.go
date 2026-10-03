package schema

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema guard's history lookup is served by a partial GIN index on
// sage.decision.target_objects (dogfood lifeos: 0.2 ms per lookup instead
// of a scan). It is built CONCURRENTLY, so it is ensured outside the
// bootstrap transaction and advisory lock.

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

func TestEnsureDecisionIndexesCreatesThePartialGINIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dropSchemaGuardIndex(t, pool)
	if err := EnsureDecisionIndexes(ctx, pool); err != nil {
		t.Fatalf("EnsureSchemaGuardIndex: %v", err)
	}
	oid, valid, def := schemaGuardIndex(t, pool)
	if !valid || !strings.Contains(def, "USING gin (target_objects)") ||
		!strings.Contains(def, "WHERE (feature = 'schema_guard'::text)") {
		t.Fatalf("index valid=%v def=%s, want a valid partial GIN index", valid, def)
	}
	if err := EnsureDecisionIndexes(ctx, pool); err != nil {
		t.Fatalf("second EnsureSchemaGuardIndex: %v", err)
	}
	if again, _, _ := schemaGuardIndex(t, pool); again != oid {
		t.Fatalf("re-run rebuilt the index (oid %d -> %d), want a no-op", oid, again)
	}
}

// The coordinator already created the index by hand on lifeos: the
// migration must leave it alone.
func TestEnsureDecisionIndexesKeepsAnExistingIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dropSchemaGuardIndex(t, pool)
	if _, err := pool.Exec(ctx, coordinatorIndexDDL); err != nil {
		t.Fatalf("create index the coordinator's way: %v", err)
	}
	oid, _, _ := schemaGuardIndex(t, pool)
	if err := EnsureDecisionIndexes(ctx, pool); err != nil {
		t.Fatalf("EnsureSchemaGuardIndex: %v", err)
	}
	if again, valid, _ := schemaGuardIndex(t, pool); again != oid || !valid {
		t.Fatalf("existing index replaced (oid %d -> %d, valid=%v)", oid, again, valid)
	}
}

// A failed concurrent build leaves an INVALID index that IF NOT EXISTS
// would skip forever; it is rebuilt.
func TestEnsureDecisionIndexesRebuildsAnInvalidIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if err := EnsureDecisionIndexes(ctx, pool); err != nil {
		t.Fatalf("EnsureSchemaGuardIndex: %v", err)
	}
	oid, _, _ := schemaGuardIndex(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = $1`, oid); err != nil {
		t.Skipf("cannot mark the index invalid (needs superuser): %v", err)
	}
	if err := EnsureDecisionIndexes(ctx, pool); err != nil {
		t.Fatalf("EnsureSchemaGuardIndex on an invalid index: %v", err)
	}
	again, valid, _ := schemaGuardIndex(t, pool)
	if !valid || again == oid {
		t.Fatalf("invalid index not rebuilt: oid %d -> %d valid=%v", oid, again, valid)
	}
}

func TestEnsureDecisionIndexesConcurrentCallers(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dropSchemaGuardIndex(t, pool)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- EnsureDecisionIndexes(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureSchemaGuardIndex: %v", err)
		}
	}
	if _, valid, _ := schemaGuardIndex(t, pool); !valid {
		t.Fatal("index invalid after concurrent ensures")
	}
}

func TestEnsureDecisionIndexesRejectsANilConnection(t *testing.T) {
	err := EnsureDecisionIndexes(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("nil database error = %v, want a named connection error", err)
	}
}
