package schema

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// storeDB is a fresh database with the full sage schema (each test owns
// one, because the history store changes sage.snapshots).
func storeDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "histstore_schema"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func columnType(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var typ *string
	if err := pool.QueryRow(ctx, `SELECT (SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a WHERE a.attrelid = to_regclass($1)
		  AND a.attname = 'database_id' AND NOT a.attisdropped)`, table).Scan(&typ); err != nil {
		t.Fatalf("read %s.database_id: %v", table, err)
	}
	if typ == nil {
		return ""
	}
	return *typ
}

func TestHistoryStoreBootstrapAddsIdentityIndexesAndTables(t *testing.T) {
	pool, ctx := storeDB(t)
	for run := 0; run < 2; run++ { // idempotent
		if err := BootstrapHistoryStore(ctx, pool); err != nil {
			t.Fatalf("history store bootstrap run %d: %v", run, err)
		}
	}
	for _, table := range []string{"sage.snapshots", "sage.query_store"} {
		if got := columnType(t, ctx, pool, table); got != "integer" {
			t.Fatalf("%s.database_id = %q, want integer", table, got)
		}
	}
	// Every partition carries the column (the parent's ADD COLUMN reached them).
	var missing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_partition_tree('sage.snapshots') pt
		WHERE NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = pt.relid
		                  AND a.attname = 'database_id' AND NOT a.attisdropped)`).
		Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("%d snapshot partitions lack database_id (%v)", missing, err)
	}
	for _, ix := range []string{"idx_snapshots_db_category", "idx_query_store_db_qid_time",
		"idx_query_store_db_time"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`, ix).
			Scan(&ok); err != nil || !ok {
			t.Fatalf("index sage.%s missing (%v)", ix, err)
		}
	}
	for _, tbl := range []string{"history_store_databases", "history_migration",
		"history_migration_ids"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`, tbl).
			Scan(&ok); err != nil || !ok {
			t.Fatalf("table sage.%s missing (%v)", tbl, err)
		}
	}
	// The normal bootstrap afterwards keeps the store intact.
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap after the history store: %v", err)
	}
	if got := columnType(t, ctx, pool, "sage.snapshots"); got != "integer" {
		t.Fatalf("bootstrap removed database_id: %q", got)
	}
}

func TestMonitoredBootstrapLeavesHistoryTablesUnchanged(t *testing.T) {
	pool, ctx := storeDB(t)
	for _, table := range []string{"sage.snapshots", "sage.query_store"} {
		if got := columnType(t, ctx, pool, table); got != "" {
			t.Fatalf("a monitored database must not get %s.database_id (got %q)", table, got)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE ns.nspname = 'sage' AND c.relname LIKE 'history_%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a monitored database must not get the history store tables (%d found)", n)
	}
}

func TestHistoryStoreBootstrapKeepsIdentityThroughConversion(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "histstore_convert"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// A plain (never partitioned) history table, as a large legacy install
	// has until retention converts it in the background.
	if _, err := pool.Exec(ctx, "CREATE SCHEMA sage;"+ddlSnapshots+ddlQueryStore); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapHistoryStore(ctx, pool); err != nil {
		t.Fatalf("history store on plain tables: %v", err)
	}
	if got := columnType(t, ctx, pool, "sage.snapshots"); got != "integer" {
		t.Fatalf("plain snapshots table: database_id = %q", got)
	}
	if err := Bootstrap(ctx, pool); err != nil { // converts the small tables
		t.Fatalf("bootstrap: %v", err)
	}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT relkind::text FROM pg_class
		WHERE oid = 'sage.snapshots'::regclass`).Scan(&kind); err != nil || kind != "p" {
		t.Fatalf("snapshots not partitioned after bootstrap: %q %v", kind, err)
	}
	if got := columnType(t, ctx, pool, "sage.snapshots"); got != "integer" {
		t.Fatalf("conversion lost database_id: %q", got)
	}
}

func TestHistoryStoreBootstrapErrors(t *testing.T) {
	if err := BootstrapHistoryStore(context.Background(), nil); err == nil {
		t.Fatal("a nil pool must be refused")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "histstore_noschema"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	err = BootstrapHistoryStore(ctx, pool)
	if !errors.Is(err, ErrNoSageSchema) {
		t.Fatalf("a database without the sage schema: want ErrNoSageSchema, got %v", err)
	}
}

func TestEnsureHistoryMigrationTablesIsIdempotent(t *testing.T) {
	pool, ctx := storeDB(t)
	for run := 0; run < 2; run++ {
		if err := EnsureHistoryMigrationTables(ctx, pool); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'history_migration'
		  AND column_name IN ('database_id', 'direction', 'table_name', 'last_id',
		                      'last_at', 'copied', 'skipped', 'completed_at')`).
		Scan(&cols); err != nil || cols != 8 {
		t.Fatalf("history_migration columns = %d (%v), want 8", cols, err)
	}
	if columnType(t, ctx, pool, "sage.snapshots") != "" {
		t.Fatal("the migration tables alone must not touch sage.snapshots")
	}
}
