package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-08: sage.facts gains missing_since, the persisted start of a fact
// subject's absence. Additive (a nullable column, no rewrite), idempotent,
// and kept across re-runs.

func TestFactsMissingSinceMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireMissingSinceColumn(t, ctx, pool)
	// A database bootstrapped before facts-08 has no column.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE sage.facts DROP COLUMN IF EXISTS missing_since`); err != nil {
		t.Fatalf("simulating the pre-facts-08 schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source) VALUES ('test_fixture', 'schema', 'mig_missing_*', 'detector')`); err != nil {
		t.Fatalf("seed a fact: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.facts WHERE subject = 'mig_missing_*'`)
	})
	bootstrapWithRetry(t, ctx, pool)
	requireMissingSinceColumn(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE sage.facts SET missing_since = '2026-11-01Z'
		WHERE subject = 'mig_missing_*'`); err != nil {
		t.Fatalf("set missing_since: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool) // a re-run keeps the column and its values
	var kept *string
	if err := pool.QueryRow(ctx, `SELECT to_char(missing_since AT TIME ZONE 'UTC',
		'YYYY-MM-DD') FROM sage.facts WHERE subject = 'mig_missing_*'`).Scan(&kept); err != nil ||
		kept == nil || *kept != "2026-11-01" {
		t.Fatalf("missing_since after a re-run: %v %v", kept, err)
	}
}

func requireMissingSinceColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var typ string
	var notNull, hasDefault bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull,
		atthasdef FROM pg_attribute WHERE attrelid = 'sage.facts'::regclass
		  AND attname = 'missing_since' AND NOT attisdropped`).
		Scan(&typ, &notNull, &hasDefault); err != nil {
		t.Fatalf("reading sage.facts.missing_since: %v", err)
	}
	if typ != "timestamp with time zone" || notNull || hasDefault {
		t.Fatalf("missing_since is %s (not null %v, default %v), want nullable timestamptz",
			typ, notNull, hasDefault)
	}
}
