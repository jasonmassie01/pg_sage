package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetpol-01a: ddlPolicySets is a T6 step after review-04a's, bounded and classified;
// on a real server it bootstraps twice cleanly, seeds the three sets once
// and records every per-database document of a v2.5.0 install as an
// exception against default without touching the document. Concurrent
// writers: none here, Bootstrap holds its advisory lock.
// fleetpol-01b appended ddlDiscoveredAttributes after it, and review-04b's
// ddlReviewFleet merged before it (right after ddlReviewTriggers, whose
// table it alters), so the pin is its place: just after ddlReviewFleet.
func TestPolicySetsStep_IsPlacedIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	if at := stepIndex(steps, "ddlPolicySets"); at < 1 || steps[at-1][0] != "ddlReviewFleet" {
		t.Fatalf("T6 steps %v, want ddlPolicySets just after ddlReviewFleet", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS set_name",
		"CREATE TABLE IF NOT EXISTS sage.policy_sets",
		"CREATE TABLE IF NOT EXISTS sage.policy_exceptions",
		"CREATE TABLE IF NOT EXISTS sage.policy_set_events",
		"ON CONFLICT (name) DO NOTHING", "ON CONFLICT (database_id) DO NOTHING"} {
		if !strings.Contains(ddlPolicySets, want) {
			t.Errorf("ddlPolicySets lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlPolicySets) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// legacyPolicyRows are a v2.5.0 install's per-database documents.
const legacyPolicyRows = `
INSERT INTO sage.policy (database_id, version, scope, profile, doc, status, proposed_by,
    ratified_by, ratified_at, activated_at)
VALUES (11, 1, 'database', 'staffed', '{"a":1}', 'active', 'v250', 'alice', now(), now()),
       (12, 3, 'database', 'staffed', '{"b":2}', 'active', 'v250', 'bob', now(), now()),
       (12, 2, 'database', 'staffed', '{"b":1}', 'superseded', 'v250', 'bob', now(), now())`

func TestPolicySetsMigration_SeedsOnceAndAdoptsLegacyDocuments(t *testing.T) {
	pool := freshSchemaPool(t, "policysets")
	ctx := context.Background()
	var seeds string
	if err := pool.QueryRow(ctx, `SELECT string_agg(name || ':' || priority, ',' ORDER BY
		priority) FROM sage.policy_sets`).Scan(&seeds); err != nil {
		t.Fatal(err)
	}
	if seeds != "default:0,prod:10,agent-created:20" {
		t.Fatalf("seeded sets = %s", seeds)
	}
	if _, err := pool.Exec(ctx, legacyPolicyRows); err != nil {
		t.Fatal(err)
	}
	for run := range 2 {
		if err := applyPolicySetsStep(ctx, pool); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	if got := adoptedExceptions(t, pool); got != "default/alice,default/bob" {
		t.Fatalf("adopted exceptions = %v", got)
	}
	var docs string
	if err := pool.QueryRow(ctx, `SELECT string_agg(doc::text, ',' ORDER BY database_id)
		FROM sage.policy WHERE status = 'active' AND database_id IS NOT NULL`).
		Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != `{"a": 1},{"b": 2}` {
		t.Fatalf("adopted documents changed: %s", docs)
	}
}

// adoptedExceptions lists sage.policy_exceptions as set/set_by by database,
// failing on one that expires (an adopted document holds until cleared).
func adoptedExceptions(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT database_id, set_name, set_by,
		expires_at IS NULL FROM sage.policy_exceptions ORDER BY database_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int64
		var set, by string
		var forever bool
		if err := rows.Scan(&id, &set, &by, &forever); err != nil {
			t.Fatal(err)
		}
		if !forever {
			t.Errorf("database %d's adopted exception expires", id)
		}
		got = append(got, set+"/"+by)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, ",")
}

// applyPolicySetsStep runs the step as the runner does: one transaction,
// so its SET LOCAL lock_timeout holds.
func applyPolicySetsStep(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, ddlPolicySets); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
