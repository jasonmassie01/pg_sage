package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sage SRE M3: the event chain accepts the model turn's events
// (model_reviewed, model_rejected, model_disagreed). Upgrading an M2
// install replaces the event-type check twice without error, keeps the
// old types, and still refuses unknown types.
func TestSREMigrationM3_ModelEventTypes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, stmt := range []string{
		`ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_m3`,
		`ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_check`,
		`ALTER TABLE sage.sre_events ADD CONSTRAINT sre_events_event_type_check
			CHECK (event_type IN ('created', 'claimed', 'step', 'transition',
			'concluded', 'pinned', 'unpinned', 'evidence_purged'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulate M2 install: %v", err)
		}
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var defs []string
	rows, err := pool.Query(ctx, `SELECT conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%event_type%'`)
	if err != nil {
		t.Fatalf("constraints: %v", err)
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		defs = append(defs, d)
	}
	rows.Close()
	if len(defs) != 1 || !strings.HasPrefix(defs[0], "sre_events_event_type_m3 ") {
		t.Fatalf("event type constraints = %v, want only sre_events_event_type_m3", defs)
	}
	for _, typ := range []string{"model_reviewed", "model_rejected", "model_disagreed",
		"created", "evidence_purged"} {
		if !strings.Contains(defs[0], "'"+typ+"'") {
			t.Errorf("constraint does not allow %s: %s", typ, defs[0])
		}
	}
	assertEventTypes(t, ctx, pool)
}

// assertEventTypes inserts events of every model type into a throwaway
// investigation (rolled back) and checks an unknown type is refused.
func assertEventTypes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const dep, db, inv = "41111111-1111-4111-8111-111111111111",
		"42222222-2222-4222-8222-222222222222", "43333333-3333-4333-8333-333333333333"
	if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	insert := `INSERT INTO sage.sre_events (deployment_id, database_id,
		investigation_id, sequence, event_type, actor, observed_at, payload,
		previous_hash, hash)
		VALUES ($1, $2, $3, $4, $5, 'worker:test', clock_timestamp(), '{}'::jsonb,
		        CASE WHEN $4 = 1 THEN NULL ELSE sha256('p'::bytea) END,
		        sha256(convert_to($5, 'UTF8')))`
	for i, typ := range []string{"model_reviewed", "model_rejected", "model_disagreed"} {
		if _, err := tx.Exec(ctx, insert, dep, db, inv, i+1, typ); err != nil {
			t.Fatalf("insert %s event: %v", typ, err)
		}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err = sp.Exec(ctx, insert, dep, db, inv, 4, "model_executed_sql")
	if err == nil || !strings.Contains(err.Error(), "sre_events_event_type_m3") {
		t.Fatalf("unknown event type = %v, want the check to refuse it", err)
	}
	_ = sp.Rollback(ctx)
}

func insertM3Fixture(ctx context.Context, tx pgx.Tx, dep, db, inv string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, $2, 'm3-events', 'configured', 'e1')`, dep, db); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO sage.sre_investigations (deployment_id,
		 database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		 expires_at)
		VALUES ($1, $2, $3, 'case:m3', 'lock_blocking', sha256('m3'::bytea),
		        'evaluating', clock_timestamp() + interval '1 hour')`, dep, db, inv)
	return err
}
