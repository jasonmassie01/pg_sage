package schema

import (
	"context"
	"strings"
	"testing"
)

// fleetpol-01b: ddlDiscoveredAttributes adds a candidate's tags, creator
// and provider creation time, the policy set it matched, and its
// enrollment milestones to sage.discovered_databases. No concurrent
// writers here: Bootstrap holds its advisory lock.

func TestDiscoveredAttributesStep_IsPlacedAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlDiscoveredAttributes")
	if at < 1 || steps[at-1][0] != "ddlPolicySets" || steps[at][1] != ddlDiscoveredAttributes {
		t.Fatalf("T6 steps %v, want ddlDiscoveredAttributes just after ddlPolicySets", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}'::jsonb",
		"ADD COLUMN IF NOT EXISTS creator text NOT NULL DEFAULT ''",
		"ADD COLUMN IF NOT EXISTS created_at timestamptz",
		"ADD COLUMN IF NOT EXISTS set_name text",
		"ADD COLUMN IF NOT EXISTS enrolled_at timestamptz",
		"ADD COLUMN IF NOT EXISTS grade_requested_at timestamptz",
		"ADD COLUMN IF NOT EXISTS first_graded_at timestamptz",
		"ADD COLUMN IF NOT EXISTS baseline_at timestamptz",
		"CREATE INDEX IF NOT EXISTS discovered_databases_set_idx"} {
		if !strings.Contains(ddlDiscoveredAttributes, want) {
			t.Errorf("ddlDiscoveredAttributes lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP ", "TRUNCATE"} {
		if strings.Contains(ddlDiscoveredAttributes, bad) {
			t.Errorf("ddlDiscoveredAttributes runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlDiscoveredAttributes) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestDiscoveredAttributesMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "discattrs")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlDiscoveredAttributes'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlDiscoveredAttributes: %d (%v), want 1", ledger, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.discovered_databases
		(source, external_id, owner, first_seen, last_seen)
		VALUES ('s', 'x', 'team', now(), now())`); err != nil {
		t.Fatalf("insert a pre-fleetpol-01b row: %v", err)
	}
	var tags, creator string
	var set *string
	if err := pool.QueryRow(ctx, `SELECT tags::text, creator, set_name
		FROM sage.discovered_databases WHERE external_id = 'x'`).Scan(&tags, &creator,
		&set); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if tags != "{}" || creator != "" || set != nil {
		t.Fatalf("defaults tags %q creator %q set %v", tags, creator, set)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.discovered_databases SET tags = '[]'::jsonb
		WHERE external_id = 'x'`); err == nil {
		t.Fatal("a non-object tags value was stored")
	}
}
