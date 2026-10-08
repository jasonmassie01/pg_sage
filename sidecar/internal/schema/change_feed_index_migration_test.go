package schema

import (
	"strings"
	"testing"
)

// The change feed's migration detector source has its own partial index;
// bootstrap creates it once and leaves it alone on later runs.
func TestChangeFeedIndexMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var oid uint32
	var def string
	if err := pool.QueryRow(ctx, `SELECT 'sage.idx_findings_migration_feed'::regclass::oid,
		pg_get_indexdef('sage.idx_findings_migration_feed'::regclass)`).
		Scan(&oid, &def); err != nil {
		t.Fatalf("index missing: %v", err)
	}
	if !strings.Contains(def, "(id) WHERE (category = 'migration_safety'::text)") {
		t.Fatalf("index = %s, want (id) WHERE category = 'migration_safety'", def)
	}
	bootstrapWithRetry(t, ctx, pool)
	var again uint32
	if err := pool.QueryRow(ctx, `SELECT 'sage.idx_findings_migration_feed'::regclass::oid`).
		Scan(&again); err != nil || again != oid {
		t.Fatalf("a valid index was rebuilt (%d -> %d, %v)", oid, again, err)
	}
}
