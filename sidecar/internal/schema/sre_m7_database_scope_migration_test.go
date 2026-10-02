package schema

import (
	"context"
	"strings"
	"testing"
)

// P0-5 (2026-10-02 roadmap): the earned-autonomy levels and proposals
// are keyed by database as well as deployment. Existing rows keep the
// database they came from when it can be determined (a carried-over
// level names the database that seeded it in its carried_over event);
// otherwise they stay deployment-wide legacy rows (database_name '')
// that each database adopts when it binds (internal/earned). Legacy
// pending proposals are superseded: pg_sage re-evaluates per database.
// Outcomes gain the 'unverified' result (P0-6) and the history the
// 'database_scoped' adoption event. Additive and idempotent.

const scopeDeployment = "5c0de000-0000-4000-8000-000000000005"

func scopeCleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		bg := context.Background()
		for _, table := range []string{"sre_autonomy_proposals", "sre_family_autonomy",
			"sre_autonomy_events", "sre_autonomy_outcomes"} {
			_, _ = pool.Exec(bg, "DELETE FROM sage."+table+" WHERE deployment_id = $1",
				scopeDeployment)
		}
	}
	clean()
	t.Cleanup(clean)
}

func TestSREMigrationScope_LevelsAndProposalsAreKeyedByDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	scopeCleanup(t, ctx)
	level := func(database string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
			(deployment_id, database_name, family, action_class, level, changed_by,
			 change_reason) VALUES ($1, $2, 'wal_retention', 'wal_bound', 1, 'test', 'scope')`,
			scopeDeployment, database)
		return err
	}
	for _, db := range []string{"orders", "billing"} {
		if err := level(db); err != nil {
			t.Fatalf("level for %s refused: %v", db, err)
		}
	}
	if err := level("orders"); err == nil {
		t.Fatal("a second level row for the same database and pair was accepted")
	}
	if err := level(strings.Repeat("d", 201)); err == nil {
		t.Fatal("a 201-character database name was accepted")
	}
	propose := func(id, database string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_proposals
			(deployment_id, database_name, id, family, action_class, from_level, to_level,
			 evidence, evidence_sha256, status, expires_at)
			VALUES ($1, $2, $3, 'wal_retention', 'wal_bound', 1, 2, '{}',
			        sha256('e'::bytea), 'pending', clock_timestamp() + interval '1 day')`,
			scopeDeployment, database, id)
		return err
	}
	if err := propose("b1111111-1111-4111-8111-111111111111", "orders"); err != nil {
		t.Fatal(err)
	}
	if err := propose("b2222222-2222-4222-8222-222222222222", "billing"); err != nil {
		t.Fatalf("a pending proposal of another database refused: %v", err)
	}
	err := propose("b3333333-3333-4333-8333-333333333333", "orders")
	if err == nil || !strings.Contains(err.Error(), "sre_autonomy_one_pending") {
		t.Fatalf("second pending proposal of one database and pair: %v", err)
	}
}

func TestSREMigrationScope_UnverifiedOutcomesAndScopeEvents(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	scopeCleanup(t, ctx)
	outcome := func(result string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
			(deployment_id, database_name, family, action_class, level, result, source,
			 actor) VALUES ($1, 'orders', 'wraparound_runway', 'freeze', 2, $2, 'executor',
			 'pg_sage')`, scopeDeployment, result)
		return err
	}
	for _, ok := range []string{"unverified", "verified_recovery", "not_recovered",
		"harmful", "safety_violation"} {
		if err := outcome(ok); err != nil {
			t.Errorf("result %s refused: %v", ok, err)
		}
	}
	if err := outcome("probably_fine"); err == nil {
		t.Fatal("an unknown outcome result was accepted")
	}
	event := func(eventType string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
			(deployment_id, family, action_class, event_type, actor, reason, database_name)
			VALUES ($1, 'wal_retention', 'wal_bound', $2, 'pg_sage', 'scope', 'orders')`,
			scopeDeployment, eventType)
		return err
	}
	for _, ok := range []string{"database_scoped", "carried_over", "deadline_override",
		"auto_executed"} {
		if err := event(ok); err != nil {
			t.Errorf("event %s refused: %v", ok, err)
		}
	}
	if err := event("database_guessed"); err == nil {
		t.Fatal("an unknown event type was accepted")
	}
}

// Re-running the migration assigns a legacy carried-over level to the
// database its carry-over event names, keeps an undeterminable level as
// a legacy row, and supersedes legacy pending proposals.
func TestSREMigrationScope_AssignsLegacyRowsWhenDeterminable(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	scopeCleanup(t, ctx)
	stmts := []string{
		`INSERT INTO sage.sre_family_autonomy (deployment_id, database_name, family,
		  action_class, level, changed_by, change_reason, provenance, carried_ref)
		 VALUES ($1, '', 'wraparound_runway', 'freeze', 3, 'pg_sage', 'carried', 'carried_over',
		  'spec F3')`,
		`INSERT INTO sage.sre_autonomy_events (deployment_id, family, action_class,
		  event_type, from_level, to_level, actor, reason, database_name)
		 VALUES ($1, 'wraparound_runway', 'freeze', 'carried_over', 1, 3, 'pg_sage',
		  'carried over', 'orders')`,
		`INSERT INTO sage.sre_family_autonomy (deployment_id, database_name, family,
		  action_class, level, changed_by, change_reason)
		 VALUES ($1, '', 'wal_retention', 'wal_bound', 2, 'user:1:a@e', 'approved')`,
		`INSERT INTO sage.sre_autonomy_proposals (deployment_id, database_name, id, family,
		  action_class, from_level, to_level, evidence, evidence_sha256, status, expires_at)
		 VALUES ($1, '', 'c1111111-1111-4111-8111-111111111111', 'lock_blocking',
		  'backend_cancel', 1, 2, '{}', sha256('e'::bytea), 'pending',
		  clock_timestamp() + interval '1 day')`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s, scopeDeployment); err != nil {
			t.Fatalf("seed legacy state: %v", err)
		}
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var freezeDB, walDB string
	if err := pool.QueryRow(ctx, `SELECT
		max(database_name) FILTER (WHERE action_class = 'freeze'),
		max(database_name) FILTER (WHERE action_class = 'wal_bound')
		FROM sage.sre_family_autonomy WHERE deployment_id = $1`, scopeDeployment).
		Scan(&freezeDB, &walDB); err != nil {
		t.Fatal(err)
	}
	if freezeDB != "orders" || walDB != "" {
		t.Fatalf("after migration: freeze on %q (want orders), wal_bound on %q (want legacy)",
			freezeDB, walDB)
	}
	var status, by string
	if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM sage.sre_autonomy_proposals
		WHERE deployment_id = $1`, scopeDeployment).Scan(&status, &by); err != nil ||
		status != "superseded" || by != "pg_sage" {
		t.Fatalf("legacy pending proposal = %s by %s (%v)", status, by, err)
	}
}
