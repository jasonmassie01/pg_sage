package schema

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate (warmup offenders 8 and 13): every startup re-ran
// migrations that read whole history tables. The incident CHECK
// constraints were dropped and re-added (three validation scans of
// sage.incidents and an ACCESS EXCLUSIVE lock), the last_detected_at
// backfill scanned sage.incidents for NULLs, and the per-database scope
// backfill scanned sage.sre_autonomy_events. A re-run must leave
// constraints as they are and read neither table. Each check runs in one
// rolled-back transaction and reads that transaction's scan counters.

func beginRolledBack(t *testing.T, ctx context.Context) pgx.Tx {
	t.Helper()
	pool, _ := requireDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func seqScans(t *testing.T, ctx context.Context, tx pgx.Tx, table string) int64 {
	t.Helper()
	s, err := testdb.XactScansOf(ctx, tx, table)
	if err != nil {
		t.Fatal(err)
	}
	return s.Seq
}

func incidentChecks(t *testing.T, ctx context.Context, tx pgx.Tx) map[string]uint32 {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT conname, oid FROM pg_constraint
		WHERE conrelid = 'sage.incidents'::regclass AND contype = 'c'`)
	if err != nil {
		t.Fatalf("read incident constraints: %v", err)
	}
	out := map[string]uint32{}
	for rows.Next() {
		var name string
		var oid uint32
		if err := rows.Scan(&name, &oid); err != nil {
			t.Fatal(err)
		}
		out[name] = oid
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIncidentConstraintMigration_RerunNeitherRebuildsNorScans(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	before := incidentChecks(t, ctx, tx)
	for _, name := range []string{"incidents_severity_check", "incidents_source_check",
		"incidents_action_risk_check"} {
		if before[name] == 0 {
			t.Fatalf("constraint %s missing after bootstrap: %v", name, before)
		}
	}
	if err := migrateIncidentConstraints(ctx, tx); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	after := incidentChecks(t, ctx, tx)
	for name, oid := range before {
		if after[name] != oid {
			t.Errorf("constraint %s rebuilt on re-run (oid %d -> %d)", name, oid, after[name])
		}
	}
	if n := seqScans(t, ctx, tx, "sage.incidents"); n != 0 {
		t.Fatalf("re-run read sage.incidents sequentially %d times, want 0", n)
	}
}

// A constraint that still lacks a value pg_sage writes is widened, as
// before (an install from before v0.9.1).
func TestIncidentConstraintMigration_WidensNarrowConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	narrow := map[string]string{
		"incidents_severity_check":    "severity IN ('warning', 'critical')",
		"incidents_source_check":      "source IN ('deterministic', 'llm')",
		"incidents_action_risk_check": "action_risk IN ('safe') OR action_risk IS NULL",
	}
	for name, check := range narrow {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE sage.incidents
			DROP CONSTRAINT %[1]s, ADD CONSTRAINT %[1]s CHECK (%[2]s) NOT VALID`,
			name, check)); err != nil {
			t.Fatalf("narrow %s: %v", name, err)
		}
	}
	if err := migrateIncidentConstraints(ctx, tx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.incidents (severity, root_cause, source,
		action_risk) VALUES ('info', 'widened', 'n_plus_one', 'medium')`); err != nil {
		t.Fatalf("a widened value is still refused: %v", err)
	}
}

// Installs whose last_detected_at was added by ALTER TABLE keep it
// nullable; it was backfilled when it was added and every writer sets it,
// so a re-run must not look for NULLs again.
func TestIncidentsLastDetectedMigration_NoScanOnceTheColumnExists(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.incidents
		ALTER COLUMN last_detected_at DROP NOT NULL`); err != nil {
		t.Fatalf("legacy shape: %v", err)
	}
	if _, err := tx.Exec(ctx, ddlIncidentsLastDetected); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n := seqScans(t, ctx, tx, "sage.incidents"); n != 0 {
		t.Fatalf("re-run read sage.incidents sequentially %d times, want 0", n)
	}
}

// The backfill still runs when the column is added.
func TestIncidentsLastDetectedMigration_BackfillsAnAddedColumn(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.incidents DROP COLUMN last_detected_at;
		INSERT INTO sage.incidents (detected_at, severity, root_cause, source)
		VALUES ('2026-01-02 03:04:05+00', 'warning', 'legacy row', 'deterministic')`); err != nil {
		t.Fatalf("pre-v0.9.2 shape: %v", err)
	}
	if _, err := tx.Exec(ctx, ddlIncidentsLastDetected); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var missing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sage.incidents
		WHERE root_cause = 'legacy row'
		  AND last_detected_at IS DISTINCT FROM detected_at`).Scan(&missing); err != nil {
		t.Fatalf("read backfill: %v", err)
	}
	if missing != 0 {
		t.Fatalf("%d legacy incidents not backfilled", missing)
	}
}

// The per-database scope backfill reads the carry-over events only when
// a deployment-wide carried-over level exists, and then by its family.
func TestAutonomyScopeMigration_NoEventScanWithoutLegacyLevels(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_autonomy_events (deployment_id,
		family, action_class, event_type, from_level, to_level, actor, reason,
		database_name)
		SELECT gen_random_uuid(), (ARRAY['lock_blocking','wal_retention'])[1 + g % 2],
		       'freeze', (ARRAY['carried_over','capped'])[1 + g % 2], 1, 2, 'perf',
		       'perf history', 'db_' || g % 7
		FROM generate_series(1, 6000) g;
		ANALYZE sage.sre_autonomy_events`); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	before := seqScans(t, ctx, tx, "sage.sre_autonomy_events")
	if _, err := tx.Exec(ctx, ddlSREAutonomyDatabaseScope); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n := seqScans(t, ctx, tx, "sage.sre_autonomy_events") - before; n != 0 {
		t.Fatalf("re-run read sage.sre_autonomy_events sequentially %d times, want 0", n)
	}
}

// With a deployment-wide carried-over level present, the backfill looks
// up that level's own family and class (an index range), not the whole
// event history, and still assigns the database its event names.
func TestAutonomyScopeMigration_LegacyLevelReadsItsPairOnly(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx := beginRolledBack(t, ctx)
	var dep string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&dep); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_autonomy_events (deployment_id,
		family, action_class, event_type, from_level, to_level, actor, reason,
		database_name)
		SELECT gen_random_uuid(), 'lock_blocking', 'freeze', 'carried_over', 1, 2,
		       'perf', 'perf history', 'db_' || g % 7
		FROM generate_series(1, 6000) g`); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_autonomy_events (deployment_id,
		family, action_class, event_type, from_level, to_level, actor, reason,
		database_name)
		VALUES ($1, 'wraparound_runway', 'freeze', 'carried_over', 1, 3, 'pg_sage',
		        'carried over', 'orders');
		INSERT INTO sage.sre_family_autonomy (deployment_id, database_name, family,
		  action_class, level, changed_by, change_reason, provenance, carried_ref)
		VALUES ($1, '', 'wraparound_runway', 'freeze', 3, 'pg_sage', 'carried',
		        'carried_over', 'spec F3')`, dep); err != nil {
		t.Fatalf("seed legacy level: %v", err)
	}
	if _, err := tx.Exec(ctx, `ANALYZE sage.sre_autonomy_events`); err != nil {
		t.Fatal(err)
	}
	before := seqScans(t, ctx, tx, "sage.sre_autonomy_events")
	if _, err := tx.Exec(ctx, ddlSREAutonomyDatabaseScope); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n := seqScans(t, ctx, tx, "sage.sre_autonomy_events") - before; n != 0 {
		t.Fatalf("backfill read sage.sre_autonomy_events sequentially %d times, want 0", n)
	}
	var db string
	if err := tx.QueryRow(ctx, `SELECT database_name FROM sage.sre_family_autonomy
		WHERE deployment_id = $1`, dep).Scan(&db); err != nil || db != "orders" {
		t.Fatalf("legacy level assigned to %q (%v), want orders", db, err)
	}
}
