package schema

import (
	"context"
	"strings"
	"testing"
)

// drill-01a on a real server: bootstrap twice is clean, and
// sage.restore_drills holds one row per drill with its partial index on
// passed drills. No concurrent-writer test: Bootstrap holds its advisory
// lock.
func TestRestoreDrillsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "restoredrills")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'restore_drills'
		  AND column_name IN ('id', 'database_id', 'database_name', 'provider', 'kind',
		      'receipt_id', 'requested_at', 'ready_at', 'finished_at', 'recovery_point',
		      'rto_seconds', 'rpo_seconds', 'invariants', 'passed', 'failure',
		      'cost_label')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 16 {
		t.Fatalf("sage.restore_drills has %d of the 16 columns", cols)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'sage'
		AND indexname = 'restore_drills_passed'`).Scan(&def); err != nil {
		t.Fatalf("passed index: %v", err)
	}
	if !strings.Contains(def, "WHERE passed") ||
		!strings.Contains(def, "database_name, finished_at DESC") {
		t.Fatalf("passed index = %s", def)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.restore_drills (database_name,
		provider, kind, requested_at, finished_at, invariants, passed)
		VALUES ('a', 'dle', 'thin_clone', now(), now() - interval '1 hour', '[]', true)`)
	if err == nil {
		t.Fatal("a drill that finished before it was requested was stored")
	}
}

func TestRestoreDrillsStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS sage.restore_drills",
		"CREATE INDEX IF NOT EXISTS restore_drills_passed", "SET LOCAL lock_timeout"} {
		if !strings.Contains(ddlRestoreDrills, want) {
			t.Errorf("ddlRestoreDrills lacks %q", want)
		}
	}
}
