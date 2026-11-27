package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-02: sage.baseline_samples holds the hour-of-week baselines.
// Bootstrap creates it, a re-run is a no-op that keeps its rows, every
// column is NOT NULL and the bucket is 0-167.
func TestBaselineSamplesMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireBaselineColumns(t, ctx, pool)
	const probe = "market02.migration_probe"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.baseline_samples WHERE database = $1`, probe)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.baseline_samples
		(database, metric, key, how, week_start, bucket, n, value)
		VALUES ($1, 'm', 'k', 'gauge', DATE '2026-11-09', 9, 1, 1)`, probe); err != nil {
		t.Fatalf("insert probe row: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.baseline_samples
		(database, metric, key, how, week_start, bucket, n, value)
		VALUES ($1, 'm', 'k2', 'gauge', DATE '2026-11-09', 168, 1, 1)`, probe); err == nil {
		t.Fatal("bucket 168 was accepted")
	}
	bootstrapWithRetry(t, ctx, pool)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.baseline_samples
		WHERE database = $1`, probe).Scan(&n); err != nil || n != 1 {
		t.Fatalf("re-run kept %d probe rows (%v), want 1", n, err)
	}
}

func requireBaselineColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	want := map[string]string{
		"database": "text", "metric": "text", "key": "text", "how": "text",
		"week_start": "date", "bucket": "smallint", "n": "bigint",
		"value": "double precision", "updated_at": "timestamp with time zone",
	}
	rows, err := pool.Query(ctx, `SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'baseline_samples'`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, typ, nullable string
		if err := rows.Scan(&name, &typ, &nullable); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if nullable != "NO" {
			t.Errorf("column %s is nullable", name)
		}
		got[name] = typ
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("column %s = %q, want %q", name, got[name], typ)
		}
	}
	if len(got) != len(want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
}
