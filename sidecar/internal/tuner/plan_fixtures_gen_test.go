package tuner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The tuner's plan heuristics are tested against real EXPLAIN (FORMAT
// JSON) output (Phase 0 item 11): hand-written plans hid the bugs (a
// "Parallel Seq Scan" node type PostgreSQL never emits, an Alias on a
// Nested Loop node, Workers Planned on scan nodes). The fixtures under
// testdata/plans are captured by TestGeneratePlanFixtures from a live
// server and committed; regenerate them with
//
//	PG_SAGE_REGEN_PLAN_FIXTURES=1 go test -count=1 \
//	    -run TestGeneratePlanFixtures ./internal/tuner
//
// against SAGE_TEST_DATABASE_URL (any supported major version).

const planFixtureDir = "testdata/plans"

// planFixtureSetup builds a small, deterministic catalog: orders (200k
// rows, btree indexes on customer_id and email) and customers whose
// region and tier are perfectly correlated, so the planner underestimates
// "region = x AND tier = x" by 20x.
var planFixtureSetup = []string{
	"DROP SCHEMA IF EXISTS planfx CASCADE",
	"CREATE SCHEMA planfx",
	`CREATE TABLE planfx.orders (id bigint PRIMARY KEY, customer_id int NOT NULL,
		status text NOT NULL, note text NOT NULL, email text NOT NULL)`,
	`INSERT INTO planfx.orders SELECT i, i % 5000,
		CASE WHEN i % 100 = 0 THEN 'open' ELSE 'closed' END,
		md5(i::text), 'u' || i || '@x.io' FROM generate_series(1, 200000) i`,
	"CREATE INDEX orders_customer_idx ON planfx.orders (customer_id)",
	"CREATE INDEX orders_email_idx ON planfx.orders (email)",
	`CREATE TABLE planfx.customers (id int PRIMARY KEY, region int NOT NULL,
		tier int NOT NULL)`,
	`INSERT INTO planfx.customers SELECT i, i % 20, i % 20
		FROM generate_series(0, 4999) i`,
	"VACUUM ANALYZE planfx.orders",
	"VACUUM ANALYZE planfx.customers",
}

type planFixtureSpec struct {
	name     string
	settings []string
	explain  string
}

var serialScan = []string{"SET max_parallel_workers_per_gather = 0"}

var forcedParallel = []string{
	"SET max_parallel_workers_per_gather = 2", "SET parallel_setup_cost = 0",
	"SET parallel_tuple_cost = 0", "SET min_parallel_table_scan_size = 0",
}

var nestedLoopOnly = []string{
	"SET max_parallel_workers_per_gather = 0", "SET enable_hashjoin = off",
	"SET enable_mergejoin = off",
}

var planFixtureSpecs = []planFixtureSpec{
	{"seq_scan_indexed_filter", append([]string{"SET enable_indexscan = off",
		"SET enable_bitmapscan = off"}, serialScan...),
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o WHERE o.customer_id = 42"},
	{"seq_scan_unindexed_filter", serialScan,
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o WHERE o.note = 'abc'"},
	{"seq_scan_expression_filter", append([]string{"SET enable_indexscan = off",
		"SET enable_bitmapscan = off"}, serialScan...),
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o " +
			"WHERE lower(o.email) = 'u1@x.io'"},
	{"seq_scan_low_selectivity", serialScan,
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o WHERE o.customer_id >= 10"},
	{"seq_scan_no_filter", serialScan,
		"EXPLAIN (FORMAT JSON) SELECT count(*) FROM planfx.orders o"},
	{"serial_seq_scan", serialScan,
		"EXPLAIN (FORMAT JSON) SELECT count(*) FROM planfx.orders o " +
			"WHERE o.note LIKE '%ab%'"},
	{"parallel_gather", forcedParallel,
		"EXPLAIN (FORMAT JSON) SELECT count(*) FROM planfx.orders o " +
			"WHERE o.note LIKE '%ab%'"},
	{"parallel_gather_merge", forcedParallel,
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o " +
			"WHERE o.note LIKE '%ab%' ORDER BY o.note"},
	{"nested_loop_misestimate", nestedLoopOnly,
		"EXPLAIN (ANALYZE, FORMAT JSON) SELECT o.id FROM planfx.customers c " +
			"JOIN planfx.orders o ON o.customer_id = c.id WHERE c.region = 3 AND c.tier = 3"},
	{"nested_loop_with_subplan", nestedLoopOnly,
		"EXPLAIN (ANALYZE, FORMAT JSON) SELECT o.id, (SELECT c2.tier FROM " +
			"planfx.customers c2 WHERE c2.id = c.id) FROM planfx.customers c " +
			"JOIN planfx.orders o ON o.customer_id = c.id WHERE c.region = 3 AND c.tier = 3"},
	{"hash_join_spill", []string{"SET max_parallel_workers_per_gather = 0",
		"SET work_mem = '64kB'", "SET enable_nestloop = off", "SET enable_mergejoin = off"},
		"EXPLAIN (ANALYZE, FORMAT JSON) SELECT o.id, c.tier FROM planfx.orders o " +
			"JOIN planfx.customers c ON c.id = o.customer_id"},
	{"disk_sort", []string{"SET max_parallel_workers_per_gather = 0",
		"SET work_mem = '64kB'"},
		"EXPLAIN (ANALYZE, FORMAT JSON) SELECT o.id FROM planfx.orders o ORDER BY o.note"},
	{"sort_under_limit", serialScan,
		"EXPLAIN (FORMAT JSON) SELECT o.id FROM planfx.orders o ORDER BY o.note LIMIT 10"},
}

// planFixtureCatalogSQL captures the catalog facts the heuristics read:
// estimated rows per table and the valid, non-partial btree indexes with
// their leading key column.
const planFixtureCatalogSQL = `
SELECT n.nspname || '.' || c.relname, c.reltuples::bigint,
       COALESCE((SELECT json_agg(json_build_object('name', i.relname,
                     'leading_column', a.attname) ORDER BY i.relname)
                   FROM pg_index x
                   JOIN pg_class i ON i.oid = x.indexrelid
                   JOIN pg_am am ON am.oid = i.relam
                   JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = x.indkey[0]
                  WHERE x.indrelid = c.oid AND x.indisvalid AND x.indisready
                    AND x.indpred IS NULL AND am.amname = 'btree'), '[]')::text
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'planfx' AND c.relkind = 'r'
 ORDER BY 1`

func TestGeneratePlanFixtures(t *testing.T) {
	if os.Getenv("PG_SAGE_REGEN_PLAN_FIXTURES") != "1" {
		t.Skip("fixture regeneration runs only with PG_SAGE_REGEN_PLAN_FIXTURES=1")
	}
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, sql := range planFixtureSetup {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}
	if err := os.MkdirAll(planFixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, spec := range planFixtureSpecs {
		writePlanFixture(t, ctx, conn, spec)
	}
	writeCatalogFixture(t, ctx, conn)
	if _, err := conn.Exec(ctx, "DROP SCHEMA planfx CASCADE"); err != nil {
		t.Fatalf("drop fixture schema: %v", err)
	}
}

func writePlanFixture(t *testing.T, ctx context.Context, conn *pgx.Conn,
	spec planFixtureSpec) {
	t.Helper()
	for _, sql := range spec.settings {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %q: %v", spec.name, sql, err)
		}
	}
	var plan string
	if err := conn.QueryRow(ctx, spec.explain).Scan(&plan); err != nil {
		t.Fatalf("%s: explain: %v", spec.name, err)
	}
	if _, err := conn.Exec(ctx, "RESET ALL"); err != nil {
		t.Fatalf("%s: reset: %v", spec.name, err)
	}
	writeIndentedJSON(t, spec.name+".json", []byte(plan))
}

func writeCatalogFixture(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	rows, err := conn.Query(ctx, planFixtureCatalogSQL)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer rows.Close()
	tables := map[string]int64{}
	indexes := map[string]json.RawMessage{}
	for rows.Next() {
		var table, idx string
		var reltuples int64
		if err := rows.Scan(&table, &reltuples, &idx); err != nil {
			t.Fatalf("scan catalog: %v", err)
		}
		tables[table], indexes[table] = reltuples, json.RawMessage(idx)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catalog: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"tables": tables, "indexes": indexes})
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	writeIndentedJSON(t, "catalog.json", raw)
}

func writeIndentedJSON(t *testing.T, name string, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: not JSON: %v", name, err)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("%s: indent: %v", name, err)
	}
	path := filepath.Join(planFixtureDir, name)
	if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
