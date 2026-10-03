package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Startup runs Bootstrap under a 10 s deadline (cmd metadb.go). Converting
// a large history table there (lifeos: 852 MB query_store, 9.3 GB snapshots)
// would fail startup; a busy table must not fail it either. Bootstrap
// converts only small tables and leaves the rest, plain and untouched, to
// retention's background conversion.

// legacyQueryStore recreates the plain sage.query_store of v1.8.2 with rows.
func legacyQueryStore(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rows int) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE sage.query_store CASCADE`,
		ddlQueryStore, ddlQueryStoreStatsEpoch,
		`ALTER TABLE sage.query_store ADD COLUMN IF NOT EXISTS plan_hash text`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("legacy setup %q: %v", stmt, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time) SELECT now() - g * interval '1 minute', g % 7, g, g, 1
		FROM generate_series(1, $1::int) g`, rows); err != nil {
		t.Fatalf("legacy rows: %v", err)
	}
}

func cutoverLeftovers(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_constraint
		WHERE conname LIKE 'query_store_cutover%') + (SELECT count(*) FROM pg_class
		WHERE relnamespace = 'sage'::regnamespace AND relname LIKE 'query_store_cutover%')`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStorageMigration_LargePlainTableIsLeftToRetention(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	serializeAcrossPackages(t, ctx, pool)
	legacyQueryStore(t, ctx, pool, 2000)
	prev := bootstrapConvertMaxBytes
	bootstrapConvertMaxBytes = 8192 // the 2,000-row heap is larger
	t.Cleanup(func() { bootstrapConvertMaxBytes = prev })

	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := Bootstrap(bctx, pool); err != nil {
		t.Fatalf("Bootstrap with a large plain query_store: %v", err)
	}
	if k := relkindOf(t, ctx, pool, "sage.query_store"); k != "r" {
		t.Fatalf("relkind = %q, want the plain table left for retention", k)
	}
	if n := cutoverLeftovers(t, ctx, pool); n != 0 {
		t.Fatalf("%d cutover objects created by a skipped conversion", n)
	}
	bootstrapConvertMaxBytes = prev
	bootstrapWithRetry(t, ctx, pool)
	if k := relkindOf(t, ctx, pool, "sage.query_store"); k != "p" {
		t.Fatalf("relkind = %q, want a small table converted at bootstrap", k)
	}
}

func TestStorageMigration_FailedConversionDoesNotFailBootstrap(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	serializeAcrossPackages(t, ctx, pool)
	legacyQueryStore(t, ctx, pool, 200)

	// A reader in a long transaction on query_store.
	conn, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM sage.query_store LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = Bootstrap(bctx, pool)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("Bootstrap failed because query_store was busy: %v", err)
	}
	if k := relkindOf(t, ctx, pool, "sage.query_store"); k != "r" {
		t.Fatalf("relkind = %q, want the plain table", k)
	}
	if n := cutoverLeftovers(t, ctx, pool); n != 0 {
		t.Fatalf("%d cutover objects left by the failed conversion", n)
	}
	bootstrapWithRetry(t, ctx, pool)
	if k := relkindOf(t, ctx, pool, "sage.query_store"); k != "p" {
		t.Fatalf("relkind = %q after the reader ended, want partitioned", k)
	}
}
