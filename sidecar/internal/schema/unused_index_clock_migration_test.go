package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rules-04: sage.unused_index_clock persists the unused_index zero-scan
// clock across sidecar restarts. Bootstrap creates it, a re-run is a
// no-op that keeps its rows, and every column is NOT NULL.
func TestUnusedIndexClockMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireClockColumns(t, ctx, pool)

	var pk string
	if err := pool.QueryRow(ctx, `SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = 'sage.unused_index_clock'::regclass AND i.indisprimary`).
		Scan(&pk); err != nil || pk != "ident" {
		t.Fatalf("primary key = %q (%v), want ident", pk, err)
	}
	const probe = "rules04.migration_probe"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.unused_index_clock WHERE ident = $1`, probe)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.unused_index_clock
		(ident, indexrelid, zero_since, updated_at) VALUES ($1, 42, now(), now())
		ON CONFLICT (ident) DO NOTHING`, probe); err != nil {
		t.Fatalf("insert probe row: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.unused_index_clock
		WHERE ident = $1`, probe).Scan(&n); err != nil || n != 1 {
		t.Fatalf("re-run kept %d probe rows (%v), want 1", n, err)
	}
}

func requireClockColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	want := map[string]string{
		"ident": "text", "indexrelid": "oid",
		"zero_since": "timestamp with time zone", "updated_at": "timestamp with time zone",
	}
	rows, err := pool.Query(ctx, `SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'unused_index_clock'`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var name, typ, nullable string
		if err := rows.Scan(&name, &typ, &nullable); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if nullable != "NO" {
			t.Fatalf("column %s is nullable", name)
		}
		got[name] = typ
	}
	rows.Close()
	if len(got) != len(want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	for c, typ := range want {
		if got[c] != typ {
			t.Fatalf("column %s type %q, want %q", c, got[c], typ)
		}
	}
}
