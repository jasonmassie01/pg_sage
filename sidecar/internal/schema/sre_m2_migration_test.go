package schema

import (
	"context"
	"testing"
)

// CHECK-28 (Sage SRE M2): upgrading an M1 install adds the investigator
// columns and tables without touching existing investigations, twice.
func TestSREMigrationM2_UpgradesAnM1InstallIdempotently(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS sage.sre_tombstones",
		"DROP TABLE IF EXISTS sage.sre_events",
		"DROP TABLE IF EXISTS sage.sre_hypotheses",
		`ALTER TABLE sage.sre_investigations
			DROP CONSTRAINT IF EXISTS sre_investigations_m2_bounds,
			DROP COLUMN IF EXISTS source_incident_id, DROP COLUMN IF EXISTS subject,
			DROP COLUMN IF EXISTS pinned, DROP COLUMN IF EXISTS summary,
			DROP COLUMN IF EXISTS concluded_at, DROP COLUMN IF EXISTS evidence_purged_at`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulate M1 install: %v", err)
		}
	}
	const dep, db, inv = "11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	cleanup := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.sre_investigations WHERE id = $1", inv)
		_, _ = pool.Exec(bg,
			"DELETE FROM sage.sre_database_bindings WHERE database_id = $1", db)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, $2, 'm1-upgrade', 'configured', 'e1')`, dep, db); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_investigations
		(deployment_id, database_id, id, source_case_id, trigger_kind,
		 trigger_fingerprint, state, expires_at)
		VALUES ($1, $2, $3, 'case:m1', 'lock_blocking', sha256('m1'::bytea),
		        'concluded', clock_timestamp() + interval '1 hour')`,
		dep, db, inv); err != nil {
		t.Fatalf("insert M1 investigation: %v", err)
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var subject, summary string
	var pinned bool
	if err := pool.QueryRow(ctx, `SELECT subject, pinned, summary::text
		FROM sage.sre_investigations WHERE id = $1`, inv).
		Scan(&subject, &pinned, &summary); err != nil {
		t.Fatalf("M1 investigation after upgrade: %v", err)
	}
	if subject != "" || pinned || summary != "{}" {
		t.Fatalf("upgrade defaults: subject %q pinned %v summary %s", subject, pinned, summary)
	}
	var triggers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger
		WHERE tgname IN ('sre_evidence_append_only', 'sre_events_append_only')`).
		Scan(&triggers); err != nil || triggers != 2 {
		t.Fatalf("append-only triggers = %d (%v)", triggers, err)
	}
	for _, tbl := range []string{"sre_hypotheses", "sre_events", "sre_tombstones"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, tbl).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("sage.%s after upgrade: %d (%v)", tbl, n, err)
		}
	}
}
