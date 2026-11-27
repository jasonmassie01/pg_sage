package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-01: revalidation records pg_stat_statements_info.stats_reset per
// hint (pgss_reset_at) and stamps hints due for re-verification
// (reverify_due). Bootstrap adds both, idempotently.

func queryHintsColumnType(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	column string) string {
	t.Helper()
	var typ string
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(data_type), '')
FROM information_schema.columns
WHERE table_schema = 'sage' AND table_name = 'query_hints' AND column_name = $1`,
		column).Scan(&typ)
	if err != nil {
		t.Fatalf("read sage.query_hints.%s: %v", column, err)
	}
	return typ
}

func TestQueryHintsLifecycleMigration_AddsColumnsTwice(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, col := range []string{"pgss_reset_at", "reverify_due"} {
		if got := queryHintsColumnType(t, ctx, pool, col); got != "timestamp with time zone" {
			t.Fatalf("sage.query_hints.%s type = %q, want timestamp with time zone", col, got)
		}
	}
}

// The migration itself re-runs without error on a migrated schema, and
// bounds its own lock wait.
func TestQueryHintsLifecycleMigration_RerunIsCleanAndBounded(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for run := 0; run < 2; run++ {
		if _, err := pool.Exec(ctx, ddlQueryHintsLifecycle); err != nil {
			t.Fatalf("re-run %d: %v", run, err)
		}
	}
	if !strings.Contains(ddlQueryHintsLifecycle, "lock_timeout") {
		t.Fatal("ddlQueryHintsLifecycle must set a lock_timeout before its ALTERs")
	}
}
