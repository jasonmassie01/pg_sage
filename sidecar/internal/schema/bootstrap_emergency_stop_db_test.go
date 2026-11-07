package schema

import (
	"context"
	"testing"
	"time"
)

// fleet-09: bootstrap seeds emergency_stop='false' so the documented
// UPDATE sage.config ... WHERE key='emergency_stop' matches a row. The
// UPDATE writes no sage.config_audit row (sage.emergency_stop does).
// (executor imports schema, so executor.CheckEmergencyStop on the updated
// row is asserted in fleet's TestDocumentedUpdateStopsExecutorsOnPostgres.)
func TestBootstrapSeedsEmergencyStopForDocumentedUpdate(t *testing.T) {
	pool := freshPool(t, "emergency_stop_seed")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := countStatement(migrationStatements(), ddlEmergencyStopSeed); got != 1 {
		t.Fatalf("emergency stop seed registered %d times, want once", got)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE key = 'emergency_stop'`).Scan(&rows); err != nil {
		t.Fatalf("count emergency_stop rows: %v", err)
	}
	if value, by := readStopRow(t, ctx, pool); rows != 1 || value != "false" || by != "bootstrap" {
		t.Fatalf("seed: %d row(s), value=%q by=%q, want one 'false' by bootstrap",
			rows, value, by)
	}
	audits := stopAuditCount(t, ctx, pool)

	tag, err := pool.Exec(ctx, `UPDATE sage.config SET value = 'true'
		WHERE key = 'emergency_stop'`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("documented UPDATE: %v rows=%d, want 1 row", err, tag.RowsAffected())
	}
	if value, _ := readStopRow(t, ctx, pool); value != "true" {
		t.Fatalf("after the documented UPDATE: value=%q, want true", value)
	}
	if got := stopAuditCount(t, ctx, pool); got != audits {
		t.Fatalf("the plain UPDATE wrote %d audit row(s); it writes none", got-audits)
	}
}

// A second bootstrap neither duplicates the seed nor overwrites a stop.
func TestBootstrapSeedKeepsExistingStop(t *testing.T) {
	pool := freshPool(t, "emergency_stop_seed_rerun")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT sage.emergency_stop(true, 'dba')`); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE key = 'emergency_stop'`).Scan(&rows); err != nil {
		t.Fatalf("count emergency_stop rows: %v", err)
	}
	if value, by := readStopRow(t, ctx, pool); rows != 1 || value != "true" || by != "dba" {
		t.Fatalf("after rerun: %d row(s), value=%q by=%q, want the stop by dba kept",
			rows, value, by)
	}
}
