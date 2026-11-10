package migration

import (
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// migration-01 review follow-up: on the active-DDL path the DDL's own
// backend is in pg_stat_activity and its query names the table. It is not
// load the DDL competes with, so active_queries leaves it out. Its queued
// lock is the DDL's own position in the lock queue and stays in
// pending_locks.
func TestRisk_ActiveQueriesLeaveOutTheDDLsOwnBackend(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".ob_orders"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s AS
		SELECT g AS id FROM generate_series(1, 1000) g`, table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	analyzeTable(t, pool, ctx, schema, "ob_orders")
	ddlPID := int(holdAndQueueVacuumFull(t, ctx, table))
	sql := "VACUUM FULL " + table
	advisor := newTestAdvisor(t, pool)

	// Boundary: without a pid nothing is left out, so the running VACUUM
	// counts itself. This pins that the pid, not the table, makes the
	// difference below.
	_, anon, err := advisor.AnalyzeRisk(ctx, sql)
	if err != nil || anon == nil {
		t.Fatalf("AnalyzeRisk = %v, %v", anon, err)
	}
	if anon.ActiveQueries != 1 {
		t.Fatalf("active queries without a pid = %d, want 1 (the VACUUM itself)",
			anon.ActiveQueries)
	}

	_, own, err := advisor.AnalyzeSessionRisk(ctx, sql, "", ddlPID)
	if err != nil || own == nil {
		t.Fatalf("AnalyzeSessionRisk = %v, %v", own, err)
	}
	if own.ActiveQueries != 0 || !own.Measured.Activity {
		t.Errorf("active queries with the DDL's pid = %d (measured %v), want 0 measured",
			own.ActiveQueries, own.Measured.Activity)
	}
	if own.PendingLocks < 1 {
		t.Errorf("pending locks = %d, want >= 1 (the DDL's own queued lock)",
			own.PendingLocks)
	}
}

// The detector hands the advisor the pid of the DDL it saw running.
func TestDetector_AnalyzePassesTheDDLsPID(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".dp_orders"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s AS
		SELECT g AS id FROM generate_series(1, 1000) g`, table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	analyzeTable(t, pool, ctx, schema, "dp_orders")
	ddlPID := int(holdAndQueueVacuumFull(t, ctx, table))
	cfg := &config.MigrationConfig{Enabled: true, Mode: "advisory"}
	d := NewDetector(pool, newTestAdvisor(t, pool), cfg, testLogFn(t))

	inc, risk, err := d.analyze(ctx, activeDDL{PID: ddlPID, Query: "VACUUM FULL " + table})
	if err != nil || inc == nil || risk == nil {
		t.Fatalf("analyze = %v, %v, %v", inc, risk, err)
	}
	if risk.ActiveQueries != 0 {
		t.Errorf("detector active queries = %d, want 0 (its own DDL left out)",
			risk.ActiveQueries)
	}
}
