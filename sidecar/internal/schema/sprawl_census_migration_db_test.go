package schema

import (
	"context"
	"strings"
	"testing"
)

// fleetpol-03: ddlSprawlCensus adds the census runs, their rows and the
// observe-only activity readings. No concurrent writers here: Bootstrap
// holds its advisory lock.

func TestSprawlCensusStep_IsPlacedAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlSprawlCensus")
	if at < 1 || steps[at-1][0] != "ddlObserveOnlyMembers" ||
		steps[at][1] != ddlSprawlCensus {
		t.Fatalf("T6 steps %v, want ddlSprawlCensus just after ddlObserveOnlyMembers", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE TABLE IF NOT EXISTS sage.sprawl_census_runs",
		"period         date NOT NULL UNIQUE",
		"CREATE TABLE IF NOT EXISTS sage.sprawl_census (",
		"REFERENCES sage.sprawl_census_runs (id) ON DELETE CASCADE",
		"size_bytes       bigint CHECK (size_bytes >= 0)",
		"CREATE TABLE IF NOT EXISTS sage.sprawl_activity",
		"CREATE INDEX IF NOT EXISTS sprawl_census_proposal_idx"} {
		if !strings.Contains(ddlSprawlCensus, want) {
			t.Errorf("ddlSprawlCensus lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP ", "TRUNCATE"} {
		if strings.Contains(ddlSprawlCensus, bad) {
			t.Errorf("ddlSprawlCensus runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlSprawlCensus) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestSprawlCensusMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "sprawlcensus") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlSprawlCensus'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlSprawlCensus: %d (%v), want 1", ledger, err)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sprawl_census_runs (period, run_at)
		VALUES ('2026-11-01', now()) RETURNING id`).Scan(&run); err != nil {
		t.Fatalf("insert a run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sprawl_census_runs (period, run_at)
		VALUES ('2026-11-01', now())`); err == nil {
		t.Fatal("a second run of the same month was stored")
	}
	bad := map[string]string{
		"negative size": `INSERT INTO sage.sprawl_census (run_id, run_at, database_key,
			kind, status, size_bytes) VALUES ($1, now(), 'a', 'full', 'measured', -1)`,
		"unknown kind": `INSERT INTO sage.sprawl_census (run_id, run_at, database_key,
			kind, status) VALUES ($1, now(), 'b', 'branch', 'measured')`,
		"unknown status": `INSERT INTO sage.sprawl_census (run_id, run_at, database_key,
			kind, status) VALUES ($1, now(), 'c', 'full', 'idle')`,
		"negative cost": `INSERT INTO sage.sprawl_census (run_id, run_at, database_key,
			kind, status, cost_month) VALUES ($1, now(), 'd', 'full', 'measured', -0.5)`,
	}
	for what, sql := range bad {
		if _, err := pool.Exec(ctx, sql, run); err == nil {
			t.Errorf("%s was stored", what)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sprawl_census (run_id, run_at,
		database_key, kind, status) VALUES ($1, now(), 'ok', 'discovered', 'not_enrolled')`,
		run); err != nil {
		t.Fatalf("insert a row: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.sprawl_census_runs WHERE id = $1`,
		run); err != nil {
		t.Fatalf("delete the run: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.sprawl_census`).Scan(
		&left); err != nil || left != 0 {
		t.Fatalf("rows left after their run went: %d (%v), want 0", left, err)
	}
}
