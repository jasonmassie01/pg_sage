package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-07: sage.fact_events (the fact history), its triggers on
// sage.facts, sage.facts.expired_by and the fact_events audit chain. The
// migration is additive and idempotent: bootstrap runs twice cleanly, and
// a database bootstrapped before facts-07 gains all of it.

func TestFactEventsMigrationRunsTwice(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireFactEventsSchema(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireFactEventsSchema(t, ctx, pool)
	var triggers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'sage.facts'::regclass AND tgname LIKE 'fact_events_%'`).
		Scan(&triggers); err != nil || triggers != 2 {
		t.Fatalf("fact event triggers on sage.facts after two runs = %d (%v), want 2",
			triggers, err)
	}
}

func TestFactEventsMigrationUpgradesAnOlderSchema(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	serializeAcrossPackages(t, ctx, pool)
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS fact_events_insert ON sage.facts`,
		`DROP TRIGGER IF EXISTS fact_events_update ON sage.facts`,
		`ALTER TABLE sage.facts DROP COLUMN IF EXISTS expired_by`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulating the pre-facts-07 schema (%s): %v", stmt, err)
		}
	}
	bootstrapWithRetry(t, ctx, pool)
	requireFactEventsSchema(t, ctx, pool)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.facts (fact_type, subject_kind,
		subject, source, proposed_by) VALUES ('test_fixture', 'schema', 'mig_events_*',
		'detector', 'detector:mig') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed a fact: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.facts WHERE id = $1`, id)
	})
	var verb, actor string
	if err := pool.QueryRow(ctx, `SELECT verb, actor FROM sage.fact_events
		WHERE fact_id = $1`, id).Scan(&verb, &actor); err != nil ||
		verb != "proposed" || actor != "detector:mig" {
		t.Fatalf("event of a fact written after the upgrade: %q %q %v", verb, actor, err)
	}
}

func requireFactEventsSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var table, expiredBy, chained bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.fact_events') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'sage.facts'::regclass
		        AND attname = 'expired_by' AND NOT attisdropped),
		EXISTS (SELECT 1 FROM sage.audit_chain_meta WHERE chain = 'fact_events'
		        AND table_name = 'sage.fact_events')`).
		Scan(&table, &expiredBy, &chained); err != nil {
		t.Fatalf("reading the fact history schema: %v", err)
	}
	if !table || !expiredBy || !chained {
		t.Fatalf("fact_events table %v, facts.expired_by %v, chain installed %v",
			table, expiredBy, chained)
	}
}
