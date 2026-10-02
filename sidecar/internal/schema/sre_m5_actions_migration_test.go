package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sage SRE M5 actions: durable action proposals linked to their
// investigation, the ChatOps identity map and replay ledger, and the
// action event types on the investigation's hash chain. The migration is
// idempotent, and the event-type check keeps every type allowed by any
// event-type check present before it (other milestones add their own).

var m5EventTypes = []string{"action_proposed", "action_requested", "action_decided",
	"action_recheck", "action_refused", "action_executed", "action_failed",
	"recovery_sample", "recovery_verdict"}

func TestSREMigrationM5_TablesAreIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"sre_action_proposals", "chatops_identities",
		"chatops_replay"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			table).Scan(&ok); err != nil || !ok {
			t.Fatalf("sage.%s missing (%v)", table, err)
		}
	}
	def := eventTypeConstraint(t, ctx, pool)
	for _, typ := range append(append([]string{}, m5EventTypes...), "model_reviewed",
		"created", "evidence_purged") {
		if !strings.Contains(def, "'"+typ+"'") {
			t.Errorf("event-type check lacks %s: %s", typ, def)
		}
	}
}

// eventTypeConstraint returns the single event-type check definition.
func eventTypeConstraint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%event_type%'`)
	if err != nil {
		t.Fatalf("constraints: %v", err)
	}
	defer rows.Close()
	var defs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		defs = append(defs, d)
	}
	if len(defs) != 1 || !strings.HasPrefix(defs[0], "sre_events_event_type_m3 ") {
		t.Fatalf("event-type constraints = %v, want one sre_events_event_type_m3", defs)
	}
	return defs[0]
}

// Another milestone's extra event type survives the M5 migration.
func TestSREMigrationM5_UnionsEveryEventTypeCheck(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, stmt := range []string{
		`ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_m3`,
		`ALTER TABLE sage.sre_events ADD CONSTRAINT sre_events_event_type_m3
			CHECK (event_type IN ('created', 'claimed', 'step', 'transition',
			'concluded', 'pinned', 'unpinned', 'evidence_purged', 'model_reviewed',
			'model_rejected', 'model_disagreed'))`,
		`ALTER TABLE sage.sre_events ADD CONSTRAINT sre_events_event_type_other
			CHECK (event_type IN ('created', 'claimed', 'step', 'transition',
			'concluded', 'pinned', 'unpinned', 'evidence_purged', 'model_reviewed',
			'model_rejected', 'model_disagreed', 'slo_burn_alert'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulate a parallel milestone: %v", err)
		}
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def := eventTypeConstraint(t, ctx, pool)
	if !strings.Contains(def, "'slo_burn_alert'") || !strings.Contains(def,
		"'action_executed'") {
		t.Fatalf("union lost a type: %s", def)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const dep, db, inv = "51111111-1111-4111-8111-111111111111",
		"52222222-2222-4222-8222-222222222222", "53333333-3333-4333-8333-333333333333"
	if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for i, typ := range append([]string{"slo_burn_alert"}, m5EventTypes...) {
		if err := insertEvent(ctx, tx, dep, db, inv, i+1, typ); err != nil {
			t.Fatalf("insert %s: %v", typ, err)
		}
	}
	sp, _ := tx.Begin(ctx)
	if err := insertEvent(ctx, sp, dep, db, inv, 99, "action_terminated"); err == nil {
		t.Fatal("an unknown event type was accepted")
	}
	_ = sp.Rollback(ctx)
}

func insertEvent(ctx context.Context, tx pgx.Tx, dep, db, inv string, seq int,
	typ string) error {
	_, err := tx.Exec(ctx, `INSERT INTO sage.sre_events (deployment_id, database_id,
		investigation_id, sequence, event_type, actor, observed_at, payload,
		previous_hash, hash)
		VALUES ($1, $2, $3, $4, $5, 'worker:test', clock_timestamp(), '{}'::jsonb,
		        CASE WHEN $4 = 1 THEN NULL ELSE sha256('p'::bytea) END,
		        sha256(convert_to($5, 'UTF8')))`, dep, db, inv, seq, typ)
	return err
}

func TestSREMigrationM5_ProposalConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const dep, db, inv = "61111111-1111-4111-8111-111111111111",
		"62222222-2222-4222-8222-222222222222", "63333333-3333-4333-8333-333333333333"
	if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	insert := `INSERT INTO sage.sre_action_proposals (id, deployment_id, database_id,
		investigation_id, action_class, state, reason, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', clock_timestamp() + interval '1 hour')`
	ok := "64444444-4444-4444-8444-444444444444"
	if _, err := tx.Exec(ctx, insert, ok, dep, db, inv, "cancel_backend",
		"proposed"); err != nil {
		t.Fatalf("valid proposal: %v", err)
	}
	for name, args := range map[string][]any{
		"second proposal of the class": {"65555555-5555-4555-8555-555555555555",
			dep, db, inv, "cancel_backend", "proposed"},
		"unknown class": {"66666666-6666-4666-8666-666666666666", dep, db, inv,
			"terminate_backend", "proposed"},
		"unknown state": {"67777777-7777-4777-8777-777777777777", dep, db, inv,
			"cancel_backend", "done"},
		"no investigation": {"68888888-8888-4888-8888-888888888888", dep, db,
			"69999999-9999-4999-8999-999999999999", "cancel_backend", "proposed"},
	} {
		sp, _ := tx.Begin(ctx)
		if _, err := sp.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s accepted", name)
		}
		_ = sp.Rollback(ctx)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sage.sre_investigations WHERE id = $1`,
		inv); err != nil {
		t.Fatalf("delete investigation: %v", err)
	}
	var n int
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM sage.sre_action_proposals WHERE id = $1`,
		ok).Scan(&n)
	if n != 0 {
		t.Fatal("a proposal outlived its investigation (retention would fail)")
	}
}
