package schema

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// config-04: what a boot leaves in the catalog does not depend on how the
// schema got there (fresh, upgraded from v2.4.0, or two boots at once).

// v240SageDump is pg_dump --schema=sage --inserts --no-owner
// --no-privileges of a database bootstrapped by pg_sage v2.4.0 (PG 14
// pg_dump, so it restores on PG 14 to 18); see testdata/README.md.
//
//go:embed testdata/v2.4.0_sage_dump.sql
var v240SageDump string

// catalogSQL describes the sage schema: columns, indexes and constraints of
// its tables, partitions left out (their names carry the day).
const catalogSQL = `
SELECT 'column ' || c.relname || '.' || a.attname || ' ' ||
       format_type(a.atttypid, a.atttypmod) || CASE WHEN a.attnotnull
       THEN ' not null' ELSE '' END || COALESCE(' default ' ||
       pg_get_expr(d.adbin, d.adrelid), '')
  FROM pg_class c
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
  LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
 WHERE c.relnamespace = 'sage'::regnamespace AND c.relkind IN ('r', 'p', 'v')
   AND NOT c.relispartition
UNION ALL
SELECT 'index ' || i.relname || ' ' || pg_get_indexdef(i.oid) ||
       CASE WHEN x.indisvalid THEN '' ELSE ' INVALID' END
  FROM pg_index x
  JOIN pg_class i ON i.oid = x.indexrelid
  JOIN pg_class c ON c.oid = x.indrelid
 WHERE c.relnamespace = 'sage'::regnamespace AND NOT c.relispartition
UNION ALL
SELECT 'constraint ' || c.relname || '.' || k.conname || ' ' ||
       pg_get_constraintdef(k.oid) || CASE WHEN k.convalidated THEN ''
       ELSE ' NOT VALID' END
  FROM pg_constraint k
  JOIN pg_class c ON c.oid = k.conrelid
 WHERE c.relnamespace = 'sage'::regnamespace AND NOT c.relispartition
ORDER BY 1`

func sageCatalog(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), catalogSQL)
	if err != nil {
		t.Fatalf("read the sage catalog: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the sage catalog: %v", err)
	}
	if len(lines) < 500 {
		t.Fatalf("sage catalog has only %d entries: not bootstrapped", len(lines))
	}
	return lines
}

// diffCatalogs reports the entries only one side has.
func diffCatalogs(want, got []string) string {
	var b strings.Builder
	for _, w := range want {
		if !slices.Contains(got, w) {
			fmt.Fprintf(&b, "\n  missing: %s", w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			fmt.Fprintf(&b, "\n  extra:   %s", g)
		}
	}
	return b.String()
}

func newPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Concurrency: two boots of an empty database at once both succeed (one
// waits on the advisory lock) and leave the catalog a single boot leaves.
func TestBootstrap_ParallelBootsLeaveTheFreshCatalog(t *testing.T) {
	fresh := freshSchemaPool(t, "schema_cat_fresh")
	pool := newPool(t, "schema_cat_parallel")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Bootstrap(ctx, pool)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("parallel boot %d: %v", i, err)
		}
	}
	if d := diffCatalogs(sageCatalog(t, fresh), sageCatalog(t, pool)); d != "" {
		t.Fatalf("parallel boots differ from a fresh install:%s", d)
	}
}

// restoreDump loads a plain pg_dump into the database of pool on its own
// session (the dump sets search_path). psql meta-commands are dropped.
func restoreDump(t *testing.T, pool *pgxpool.Pool, dump string) {
	t.Helper()
	var keep []string
	for _, line := range strings.Split(dump, "\n") {
		if !strings.HasPrefix(line, `\`) {
			keep = append(keep, line)
		}
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("restore session: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, strings.Join(keep, "\n")); err != nil {
		t.Fatalf("restore the v2.4.0 dump: %v", err)
	}
}

// Upgrade: a v2.4.0 sage schema, with its rows, boots twice to the catalog
// of a fresh install. Phase 2 (config-04b, the migration ledger) builds on
// this test.
func TestBootstrap_UpgradeFromV240MatchesAFreshInstall(t *testing.T) {
	if !strings.Contains(v240SageDump, "CREATE TABLE sage.action_log") {
		t.Fatal("testdata dump does not hold a sage schema")
	}
	fresh := freshSchemaPool(t, "schema_up_fresh")
	pool := newPool(t, "schema_up_v240")
	restoreDump(t, pool, v240SageDump)
	before := sageCatalog(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("upgrade boot %d: %v", run, err)
		}
	}
	got := sageCatalog(t, pool)
	if d := diffCatalogs(sageCatalog(t, fresh), got); d != "" {
		t.Fatalf("v2.4.0 upgraded differs from a fresh install:%s", d)
	}
	if slices.Equal(before, got) {
		t.Fatal("the upgrade changed nothing: the dump is not from an older release")
	}
	if err := CheckReady(ctx, pool); err != nil {
		t.Fatalf("CheckReady after the upgrade: %v", err)
	}
}

// CHECK-03: on a second boot pg_stat_statements counts no ALTER TABLE and
// no CREATE INDEX in the database.
func TestBootstrap_SecondBootRunsNoAlterOrCreateIndex(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_pgss")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable (needs shared_preload_libraries): %v", err)
	}
	const ddlCalls = `SELECT COALESCE(sum(calls), 0)::bigint FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query ~* ('^\s*(ALTER\s+TABLE\s+(IF\s+EXISTS\s+)?(ONLY\s+)?sage\.' ||
		                '|CREATE\s+(UNIQUE\s+)?INDEX)')`
	var before, after int64
	if err := pool.QueryRow(ctx, ddlCalls).Scan(&before); err != nil {
		t.Fatalf("count DDL before: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if err := pool.QueryRow(ctx, ddlCalls).Scan(&after); err != nil {
		t.Fatalf("count DDL after: %v", err)
	}
	if after != before {
		t.Fatalf("second boot ran %d ALTER TABLE / CREATE INDEX statements", after-before)
	}
}
