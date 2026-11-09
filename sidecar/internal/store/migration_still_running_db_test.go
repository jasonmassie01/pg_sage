package store

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/migration"
)

// migration-05, state transitions: a DDL still running after 25 h keeps
// its finding open. The detector re-upserts on every poll in which the
// DDL is present, and that upsert moves last_seen forward, so the 24 h
// stale resolver leaves it alone; a finding that is not re-upserted is
// resolved. The "clock" is last_seen moved 25 h into the past.
func TestMigrationFinding_StillRunningDDLStaysOpen(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	st := NewMigrationSafetyFindingStore(pool)
	running, gone := "public.lq:ddl_vacuum_full:running", "public.lq:ddl_vacuum_full:gone"
	cleanupMigrationIdent(t, running)
	cleanupMigrationIdent(t, gone)
	for _, ident := range []string{running, gone} {
		if _, err := st.UpsertMigrationSafetyFinding(ctx, migrationUpsertInput(ident)); err != nil {
			t.Fatalf("upsert %s: %v", ident, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings
		SET last_seen = now() - interval '25 hours'
		WHERE object_identifier = ANY($1)`, []string{running, gone}); err != nil {
		t.Fatalf("age findings: %v", err)
	}
	escalated := migrationUpsertInput(running)
	escalated.Severity = "critical"
	if _, err := st.UpsertMigrationSafetyFinding(ctx, escalated); err != nil {
		t.Fatalf("re-upsert of the running DDL: %v", err)
	}
	if _, err := migration.ResolveStaleFindings(ctx, pool, 24*time.Hour); err != nil {
		t.Fatalf("resolve stale: %v", err)
	}
	open := readMigrationRows(t, ctx, running, "open")
	if open.rows != 1 || open.severity != "critical" || open.occurrences < 2 {
		t.Fatalf("running DDL finding = %+v, want one open critical row", open)
	}
	if r := readMigrationRows(t, ctx, gone, "resolved"); r.rows != 1 {
		t.Fatalf("finished DDL finding resolved rows = %d, want 1", r.rows)
	}
}
