package tuner

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hintTableLayout reports whether hint_plan.hints exists and whether it has
// the pg_hint_plan 1.7+ query_id column (1.4-1.6, shipped for PG14-16, key
// hints by norm_query_string instead).
func hintTableLayout(t *testing.T, pool *pgxpool.Pool) (exists, hasQueryID bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('hint_plan.hints') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = to_regclass('hint_plan.hints')
			  AND attname = 'query_id' AND NOT attisdropped)`).Scan(&exists, &hasQueryID)
	if err != nil {
		t.Fatalf("inspect hint_plan.hints: %v", err)
	}
	return exists, hasQueryID
}

// requireQueryIDHintTable skips tests of the query_id hint-table path on a
// server whose installed pg_hint_plan predates 1.7. The product never takes
// that path there: checkHintTable reports the table as not ready.
func requireQueryIDHintTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if exists, hasQueryID := hintTableLayout(t, pool); exists && !hasQueryID {
		t.Skip("pg_hint_plan < 1.7: hint_plan.hints has no query_id column")
	}
}

// pg_hint_plan 1.4-1.6 create hint_plan.hints without query_id. Treating
// that table as ready made every hint insert fail on PG14-16.
func TestCheckHintTableRequiresQueryIDLayout(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	if exists, hasQueryID := hintTableLayout(t, pool); exists {
		if got := checkHintTable(ctx, pool); got != hasQueryID {
			t.Fatalf("checkHintTable = %t with installed layout query_id=%t",
				got, hasQueryID)
		}
		return
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS hint_plan CASCADE")
	})
	for _, stmt := range []string{
		"CREATE SCHEMA hint_plan",
		`CREATE TABLE hint_plan.hints (id serial PRIMARY KEY,
			norm_query_string text NOT NULL, application_name text NOT NULL,
			hints text NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if checkHintTable(ctx, pool) {
		t.Fatal("legacy norm_query_string hint table reported ready")
	}
	if _, err := pool.Exec(ctx,
		"ALTER TABLE hint_plan.hints ADD COLUMN query_id bigint"); err != nil {
		t.Fatal(err)
	}
	if !checkHintTable(ctx, pool) {
		t.Fatal("query_id hint table not reported ready")
	}
}
