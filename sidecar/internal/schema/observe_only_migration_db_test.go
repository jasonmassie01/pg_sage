package schema

import (
	"context"
	"strings"
	"testing"
)

// fleetpol-01d: ddlObserveOnlyMembers adds the member tier, the newest
// sweep per observe-only member and the sweep rounds. No concurrent
// writers here: Bootstrap holds its advisory lock.

func TestObserveOnlyStep_IsPlacedAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlObserveOnlyMembers")
	if at < 1 || steps[at-1][0] != "ddlDiscoveredAttributes" ||
		steps[at][1] != ddlObserveOnlyMembers {
		t.Fatalf("T6 steps %v, want ddlObserveOnlyMembers just after "+
			"ddlDiscoveredAttributes", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"ADD COLUMN IF NOT EXISTS tier text NOT NULL DEFAULT 'full'",
		"CHECK (tier IN ('full', 'observe_only'))",
		"CREATE INDEX IF NOT EXISTS databases_tier_idx",
		"CREATE TABLE IF NOT EXISTS sage.observe_rounds",
		"CREATE TABLE IF NOT EXISTS sage.observe_sweeps",
		"REFERENCES sage.databases (id) ON DELETE CASCADE"} {
		if !strings.Contains(ddlObserveOnlyMembers, want) {
			t.Errorf("ddlObserveOnlyMembers lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP ", "TRUNCATE"} {
		if strings.Contains(ddlObserveOnlyMembers, bad) {
			t.Errorf("ddlObserveOnlyMembers runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlObserveOnlyMembers) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestObserveOnlyMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "observeonly") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlObserveOnlyMembers'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlObserveOnlyMembers: %d (%v), want 1", ledger, err)
	}
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.databases
		(name, host, database_name, username, password_enc)
		VALUES ('pre', 'h', 'd', 'u', '\x00') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert a pre-fleetpol-01d row: %v", err)
	}
	var tier string
	if err := pool.QueryRow(ctx, `SELECT tier FROM sage.databases WHERE id = $1`,
		id).Scan(&tier); err != nil || tier != "full" {
		t.Fatalf("default tier %q (%v), want full", tier, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.databases SET tier = 'light'
		WHERE id = $1`, id); err == nil {
		t.Fatal("an unknown tier was stored")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.observe_sweeps
		(database_id, round_id, swept_at, status) VALUES ($1, 1, now(), 'graded')`,
		id); err == nil {
		t.Fatal("an unknown sweep status was stored")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.observe_sweeps
		(database_id, round_id, swept_at, status) VALUES ($1, 1, now(), 'ok')`,
		id); err != nil {
		t.Fatalf("insert a sweep: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.databases WHERE id = $1`, id); err != nil {
		t.Fatalf("delete the member: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.observe_sweeps`).Scan(
		&left); err != nil || left != 0 {
		t.Fatalf("sweeps left after the member went: %d (%v), want 0", left, err)
	}
}
