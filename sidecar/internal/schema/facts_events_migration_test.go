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

// Upgrade: facts decided before facts-07 have no event, so the first
// revocation after the upgrade would lose who confirmed them. The first
// install records each such fact's state once (verb baseline, by its
// decider); a fact that already has events, and a re-run, add nothing.
func TestFactEventsMigrationRecordsTheStateOfOlderFacts(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	serializeAcrossPackages(t, ctx, pool)
	tracked := seedFact(t, ctx, pool, `INSERT INTO sage.facts (fact_type, subject_kind,
		subject, source, proposed_by) VALUES ('test_fixture', 'schema',
		'mig_base_tracked_*', 'detector', 'detector:mig') RETURNING id`)
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS fact_events_insert ON sage.facts`,
		`DROP TRIGGER IF EXISTS fact_events_update ON sage.facts`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulating the pre-facts-07 schema (%s): %v", stmt, err)
		}
	}
	confirmed := seedFact(t, ctx, pool, `INSERT INTO sage.facts (fact_type, subject_kind,
		subject, source, proposed_by, status, decided_by, decided_at, decision_note)
		VALUES ('test_fixture', 'schema', 'mig_base_confirmed_*', 'detector',
		'detector:mig', 'confirmed', 'alice', now() - interval '30 days', 'fixtures')
		RETURNING id`)
	expired := seedFact(t, ctx, pool, `INSERT INTO sage.facts (fact_type, subject_kind,
		subject, source, proposed_by, status, decided_by, decided_at, expired_reason)
		VALUES ('test_fixture', 'schema', 'mig_base_expired_*', 'detector',
		'detector:mig', 'expired', 'bob', now() - interval '9 days',
		'expired by carol: moved') RETURNING id`)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	if ev := readFactEvents(t, ctx, pool, tracked); len(ev) != 1 || ev[0].verb != "proposed" {
		t.Fatalf("a fact with history gained a baseline: %+v", ev)
	}
	ev := readFactEvents(t, ctx, pool, confirmed)
	if len(ev) != 1 || ev[0] != (factEvent{"baseline", "alice", "", "confirmed",
		"state when the fact history began (facts-07): fixtures"}) {
		t.Fatalf("baseline of a confirmed fact = %+v", ev)
	}
	ev = readFactEvents(t, ctx, pool, expired)
	if len(ev) != 1 || ev[0] != (factEvent{"baseline", "unknown", "", "expired",
		"state when the fact history began (facts-07): decided by bob; " +
			"expired by carol: moved"}) {
		t.Fatalf("baseline of an expired fact = %+v", ev)
	}
}

// seedFact runs an INSERT ... RETURNING id and deletes the fact afterwards.
func seedFact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, sql).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", sql, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.facts WHERE id = $1`, id)
	})
	return id
}

type factEvent struct{ verb, actor, from, to, note string }

// readFactEvents returns the events of fact id whose hash matches the
// fact's current sage.fact_hash, in order.
func readFactEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id int64) []factEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT e.verb, e.actor, e.from_status, e.to_status,
		e.note FROM sage.fact_events e JOIN sage.facts f ON f.id = e.fact_id
		WHERE e.fact_id = $1 AND e.fact_hash = sage.fact_hash(f) ORDER BY e.id`, id)
	if err != nil {
		t.Fatalf("read events of fact %d: %v", id, err)
	}
	defer rows.Close()
	var out []factEvent
	for rows.Next() {
		var e factEvent
		if err := rows.Scan(&e.verb, &e.actor, &e.from, &e.to, &e.note); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events of fact %d: %v", id, err)
	}
	return out
}
