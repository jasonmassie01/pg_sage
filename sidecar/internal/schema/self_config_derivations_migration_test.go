package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// footprint-04: sage.config_derived_setting keeps each pass's raw
// derivations (a jsonb list, empty by default) and the derivation ledger
// accepts a "clamped" event. Re-running bootstrap is a no-op, and a
// pre-footprint-04 schema is upgraded in place.

const legacySelfConfigShape = `
ALTER TABLE sage.config_derived_setting DROP COLUMN IF EXISTS derivations;
ALTER TABLE sage.config_derivation DROP CONSTRAINT IF EXISTS config_derivation_event_v2;
ALTER TABLE sage.config_derivation DROP CONSTRAINT IF EXISTS config_derivation_event_check;
ALTER TABLE sage.config_derivation ADD CONSTRAINT config_derivation_event_check
    CHECK (event IN ('shadow', 'promoted', 'applied', 'held', 'cleared', 'operator_set',
        'resumed', 'pinned', 'unpinned'));
`

func requireDerivationsShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var typ, def string
	var notNull bool
	err := pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull,
		pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = 'sage.config_derived_setting'::regclass
		  AND a.attname = 'derivations' AND NOT a.attisdropped`).Scan(&typ, &notNull, &def)
	if err != nil {
		t.Fatalf("derivations column: %v", err)
	}
	if typ != "jsonb" || !notNull || !strings.Contains(def, "'[]'") {
		t.Fatalf("derivations column is %s not null=%v default %s", typ, notNull, def)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.config_derivation'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%event%'`).Scan(&checks); err != nil {
		t.Fatalf("event checks: %v", err)
	}
	if checks != 1 {
		t.Fatalf("%d event checks on sage.config_derivation, want exactly 1", checks)
	}
}

func insertDerivationEvent(ctx context.Context, pool *pgxpool.Pool, event string) error {
	_, err := pool.Exec(ctx, `INSERT INTO sage.config_derivation (key, event, rule,
		rule_version) VALUES ('footprint04.probe', $1, 'r', 1)`, event)
	return err
}

func TestSelfConfigDerivationsMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.config_derivation WHERE key = 'footprint04.probe'`)
	})
	if _, err := pool.Exec(ctx, legacySelfConfigShape); err != nil {
		t.Fatalf("simulating the pre-footprint-04 schema: %v", err)
	}
	if err := insertDerivationEvent(ctx, pool, "clamped"); err == nil {
		t.Fatal("the legacy schema accepted a clamped event; the simulation is wrong")
	}
	bootstrapWithRetry(t, ctx, pool)
	requireDerivationsShape(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // a re-run is a no-op
	requireDerivationsShape(t, ctx, pool)

	if err := insertDerivationEvent(ctx, pool, "clamped"); err != nil {
		t.Fatalf("a clamped event was refused: %v", err)
	}
	for _, ev := range []string{"shadow", "promoted", "held", "cleared", "unpinned"} {
		if err := insertDerivationEvent(ctx, pool, ev); err != nil {
			t.Fatalf("existing event %s refused after the migration: %v", ev, err)
		}
	}
	err := insertDerivationEvent(ctx, pool, "bogus")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an unknown event was not refused by the check: %v", err)
	}
}
