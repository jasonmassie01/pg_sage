package schema

import (
	"strings"
	"testing"
)

// Dogfood lifeos-1: legacy open incidents are backfilled and merged by
// identity at hydration. The partial index over open incidents keeps that
// (and the engine's per-identity lookups) bounded by the open set, not by
// the whole incident history. Re-running Bootstrap is a no-op.
func TestIncidentOpenIdentityMigration_PartialIndexIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var n int
	var def string
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), indexdef
		FROM pg_indexes WHERE schemaname = 'sage'
		  AND indexname = 'idx_incidents_identity_open'`).Scan(&n, &def); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if n != 1 || !strings.Contains(def, "(database_name, identity_key)") ||
		!strings.Contains(def, "WHERE (resolved_at IS NULL)") {
		t.Fatalf("idx_incidents_identity_open = %q (%d), want a partial index of open "+
			"incidents by database and identity", def, n)
	}
	var registered bool
	for _, stmt := range migrationStatements() {
		if strings.Contains(stmt, "idx_incidents_identity_open") {
			registered = true
		}
	}
	if !registered {
		t.Fatal("the open-identity index is not a registered migration")
	}
}
