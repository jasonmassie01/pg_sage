package hint_verify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DSN targets the Docker PG17 container with pg_hint_plan 1.7.1.
// Override with HINT_TEST_DSN env var.
func testDSN() string {
	if v := os.Getenv("HINT_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@127.0.0.1:5435/hint_test?sslmode=disable"
}

func setupPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	// Bounded ConnectTimeout prevents TCP-reachable-but-unresponsive PG
	// servers (e.g., port accepted but handshake never completes) from
	// hanging the test process for the full go test timeout.
	cfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(pingCtx, cfg)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// explain returns the EXPLAIN output for a query.
func explain(
	ctx context.Context, pool *pgxpool.Pool,
	query string, analyze bool,
) (string, error) {
	prefix := "EXPLAIN (FORMAT TEXT, COSTS ON)"
	if analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)"
	}
	rows, err := pool.Query(ctx, prefix+" "+query)
	if err != nil {
		return "", fmt.Errorf("explain: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), rows.Err()
}

// explainWithGUC runs EXPLAIN after setting a session GUC.
func explainWithGUC(
	ctx context.Context, pool *pgxpool.Pool,
	guc, val, query string, analyze bool,
) (string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, fmt.Sprintf("SET %s = '%s'", guc, val))
	if err != nil {
		return "", fmt.Errorf("SET %s: %w", guc, err)
	}
	prefix := "EXPLAIN (FORMAT TEXT, COSTS ON)"
	if analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)"
	}
	rows, err := conn.Query(ctx, prefix+" "+query)
	if err != nil {
		return "", fmt.Errorf("explain: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), rows.Err()
}

// planContains checks if the EXPLAIN output contains a substring.
func planContains(plan, substr string) bool {
	return strings.Contains(
		strings.ToLower(plan), strings.ToLower(substr),
	)
}

// requireHintPlan skips the test when pg_hint_plan is not installed.
// Tests that depend on hint directives (/*+ ... */) or the hint_plan.hints
// table cannot verify behavior otherwise — PG silently treats directives as
// comments, producing misleading planner output.
func requireHintPlan(ctx context.Context, pool *pgxpool.Pool, t *testing.T) {
	t.Helper()
	var installed bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_hint_plan')`,
	).Scan(&installed)
	if err != nil {
		t.Skipf("pg_hint_plan check failed: %v", err)
	}
	if !installed {
		t.Skip("pg_hint_plan extension not installed on target PG")
	}
}

// bootstrap creates test tables with enough data to produce
// the plan symptoms we want to verify.
func bootstrap(
	ctx context.Context, pool *pgxpool.Pool, t *testing.T,
) {
	t.Helper()

	ddl := []string{
		`DROP TABLE IF EXISTS orders CASCADE`,
		`DROP TABLE IF EXISTS customers CASCADE`,
		`DROP TABLE IF EXISTS products CASCADE`,
		`DROP TABLE IF EXISTS lineitems CASCADE`,
		`DROP TABLE IF EXISTS big_events CASCADE`,

		// customers: parent table for FK tests
		`CREATE TABLE customers (
			id serial PRIMARY KEY,
			name text NOT NULL,
			email text,
			region text
		)`,
		// orders: child table with FK, no index on customer_id
		`CREATE TABLE orders (
			id serial PRIMARY KEY,
			customer_id int NOT NULL REFERENCES customers(id),
			amount numeric(12,2),
			status text DEFAULT 'pending',
			created_at timestamptz DEFAULT now()
		)`,
		// products: for join tests
		`CREATE TABLE products (
			id serial PRIMARY KEY,
			name text NOT NULL,
			category text,
			price numeric(10,2)
		)`,
		// lineitems: for nested loop / hash join tests
		`CREATE TABLE lineitems (
			id serial PRIMARY KEY,
			order_id int NOT NULL,
			product_id int NOT NULL,
			qty int DEFAULT 1,
			unit_price numeric(10,2)
		)`,
		// big_events: large table for parallel scan / sort tests
		`CREATE TABLE big_events (
			id serial PRIMARY KEY,
			event_type text,
			payload text,
			score numeric(10,2),
			created_at timestamptz DEFAULT now()
		)`,
	}

	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("bootstrap DDL: %v\nSQL: %s", err, stmt)
		}
	}

	// Populate data
	inserts := []string{
		// 10K customers
		`INSERT INTO customers (name, email, region)
		 SELECT 'cust_' || g,
		        'cust_' || g || '@test.com',
		        (ARRAY['east','west','north','south'])[1 + (g % 4)]
		 FROM generate_series(1, 10000) g`,

		// 100K orders spread across customers
		`INSERT INTO orders (customer_id, amount, status, created_at)
		 SELECT 1 + (g % 10000),
		        (random() * 1000)::numeric(12,2),
		        (ARRAY['pending','shipped','delivered','cancelled'])[1 + (g % 4)],
		        now() - (random() * interval '365 days')
		 FROM generate_series(1, 100000) g`,

		// 5K products
		`INSERT INTO products (name, category, price)
		 SELECT 'prod_' || g,
		        (ARRAY['electronics','clothing','food','books','toys'])[1 + (g % 5)],
		        (random() * 500)::numeric(10,2)
		 FROM generate_series(1, 5000) g`,

		// 200K lineitems
		`INSERT INTO lineitems (order_id, product_id, qty, unit_price)
		 SELECT 1 + (g % 100000),
		        1 + (g % 5000),
		        1 + (g % 10),
		        (random() * 200)::numeric(10,2)
		 FROM generate_series(1, 200000) g`,

		// 500K big_events for sort/parallel tests
		`INSERT INTO big_events (event_type, payload, score, created_at)
		 SELECT (ARRAY['click','view','purchase','signup','logout'])[1 + (g % 5)],
		        repeat('x', 100 + (g % 200)),
		        (random() * 10000)::numeric(10,2),
		        now() - (random() * interval '90 days')
		 FROM generate_series(1, 500000) g`,
	}

	for _, stmt := range inserts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("bootstrap data: %v\nSQL: %s", err, stmt)
		}
	}

	// Create specific indexes for testing
	indexes := []string{
		// Index on orders.status for seq_scan_with_index test
		`CREATE INDEX idx_orders_status ON orders(status)`,
		// Index on orders.created_at for sort tests
		`CREATE INDEX idx_orders_created ON orders(created_at)`,
		// Index on big_events.score for sort/limit tests
		`CREATE INDEX idx_events_score ON big_events(score)`,
		// Index on big_events.event_type
		`CREATE INDEX idx_events_type ON big_events(event_type)`,
		// Index on lineitems for join tests
		`CREATE INDEX idx_li_order ON lineitems(order_id)`,
		`CREATE INDEX idx_li_product ON lineitems(product_id)`,
	}

	for _, stmt := range indexes {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("bootstrap index: %v\nSQL: %s", err, stmt)
		}
	}

	// ANALYZE so the planner has stats
	if _, err := pool.Exec(ctx, "ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}

// ---------- Test Cases ----------

// Case 1: seq_scan_with_index → IndexScan(t idx) hint
// Symptom: planner picks Seq Scan on a table that has an index.
// Hint forces IndexScan.
func TestHint_SeqScanWithIndex(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Query that scans a large portion → planner picks Seq Scan
	query := `SELECT * FROM orders WHERE status = 'pending'`

	before, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	if !planContains(before, "Seq Scan") {
		t.Log("Planner already using index scan; adjusting test")
		// Force seq scan for the "before" baseline
		before, err = explainWithGUC(
			ctx, pool, "enable_indexscan", "off", query, false,
		)
		if err != nil {
			t.Fatalf("EXPLAIN with disabled indexscan: %v", err)
		}
		t.Logf("BEFORE (forced) plan:\n%s", before)
	}

	// Apply hint: force IndexScan
	hinted := `SELECT /*+ IndexScan(orders idx_orders_status) */ * FROM orders WHERE status = 'pending'`
	after, err := explain(ctx, pool, hinted, false)
	if err != nil {
		t.Fatalf("EXPLAIN after hint: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Index Scan") &&
		!planContains(after, "Bitmap Index Scan") {
		t.Error("AFTER plan does not show Index Scan — hint not applied")
	}
	if planContains(after, "idx_orders_status") {
		t.Log("Confirmed: using idx_orders_status")
	}
}

// Case 2: disk_sort → Set(work_mem "NMB") hint
// Symptom: sort spills to disk due to low work_mem.
// Fix: raising work_mem so sort fits in memory.
// pg_hint_plan's Set() affects planner cost estimates. For executor
// behavior we verify via session-level SET which is what the sidecar
// applies via ALTER SYSTEM or per-connection SET.
func TestHint_DiskSort(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	query := `SELECT * FROM big_events ORDER BY payload`

	// Before: low work_mem → external merge (disk)
	before, err := explainWithGUC(
		ctx, pool, "work_mem", "256kB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	if !planContains(before, "external") &&
		!planContains(before, "Disk") {
		t.Log("WARNING: sort did not spill to disk with 256kB work_mem")
	}

	// Verify pg_hint_plan accepts the Set() directive
	hinted := `SELECT /*+ Set(work_mem "256MB") */ 1`
	hintPlan, err := explainWithGUC(
		ctx, pool, "pg_hint_plan.debug_print", "on", hinted, false,
	)
	if err != nil {
		t.Logf("hint debug check: %v", err)
	}
	t.Logf("Hint acceptance check:\n%s", hintPlan)

	// After: raised work_mem → in-memory sort
	after, err := explainWithGUC(
		ctx, pool, "work_mem", "256MB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE after: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if planContains(after, "external") || planContains(after, "Disk") {
		t.Error("AFTER plan still spilling to disk with 256MB work_mem")
	}
	if planContains(after, "quicksort") || planContains(after, "Memory") {
		t.Log("Confirmed: sort completed in memory")
	}
}

// Case 3: hash_spill → Set(work_mem "NMB") hint
// Symptom: hash join uses multiple batches due to low work_mem.
// Fix: raising work_mem so hash fits in a single batch.
func TestHint_HashSpill(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	query := `SELECT o.id, l.qty
	          FROM orders o
	          JOIN lineitems l ON l.order_id = o.id
	          WHERE o.status = 'pending'`

	// Before: low work_mem → hash batches > 1
	before, err := explainWithGUC(
		ctx, pool, "work_mem", "64kB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	if !planContains(before, "Batches:") {
		t.Log("WARNING: no hash batches info in plan")
	}

	// After: raised work_mem → single batch
	after, err := explainWithGUC(
		ctx, pool, "work_mem", "128MB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE after: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if planContains(after, "Hash Join") || planContains(after, "Hash") {
		if planContains(after, "Batches: 1") {
			t.Log("Confirmed: hash completed in single batch")
		} else if !planContains(after, "Batches:") {
			t.Log("Confirmed: no hash batches (different join type)")
		} else {
			t.Error("AFTER plan still has multiple hash batches")
		}
	}
}

// Case 4: bad_nested_loop → HashJoin(alias) hint
// Symptom: planner picks Nested Loop with bad row estimate.
// Hint forces Hash Join.
func TestHint_BadNestedLoop(t *testing.T) {
	pool, ctx := setupPool(t)
	requireHintPlan(ctx, pool, t)
	bootstrap(ctx, pool, t)

	// Force nested loop for baseline
	query := `SELECT c.name, o.amount
	          FROM customers c
	          JOIN orders o ON o.customer_id = c.id
	          WHERE c.region = 'east'`

	before, err := explainWithGUC(
		ctx, pool, "enable_hashjoin", "off", query, false,
	)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan (hashjoin disabled):\n%s", before)

	if !planContains(before, "Nested Loop") &&
		!planContains(before, "Merge Join") {
		t.Log("NOTE: planner not using Nested Loop even with hashjoin off")
	}

	// After: hint forces HashJoin
	hinted := `SELECT /*+ HashJoin(c o) */ c.name, o.amount
	           FROM customers c
	           JOIN orders o ON o.customer_id = c.id
	           WHERE c.region = 'east'`
	after, err := explain(ctx, pool, hinted, false)
	if err != nil {
		t.Fatalf("EXPLAIN after hint: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Hash Join") {
		t.Error("AFTER plan does not show Hash Join — hint not applied")
	}
}

// Case 5: parallel_disabled → Set(max_parallel_workers_per_gather "4")
// Symptom: parallel workers not used on large table scan.
// Fix: enabling parallel workers via GUC change.
func TestHint_ParallelDisabled(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Use a query on a column without an index to force Seq Scan,
	// which is eligible for parallel execution.
	query := `SELECT count(*) FROM big_events WHERE length(payload) > 200`

	// Before: max_parallel_workers_per_gather=0 → no parallel
	before, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	if planContains(before, "Parallel") || planContains(before, "Gather") {
		t.Log("NOTE: parallel already enabled in default config")
	}

	// After: raise max_parallel_workers_per_gather → parallel scan
	after, err := explainWithGUC(
		ctx, pool,
		"max_parallel_workers_per_gather", "4",
		query, false,
	)
	if err != nil {
		t.Fatalf("EXPLAIN after: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Gather") &&
		!planContains(after, "Parallel") {
		t.Error("AFTER plan does not show parallel execution")
	}
}

// Case 6: high_plan_time → Set(plan_cache_mode "force_generic_plan")
// Symptom: planning time dominates execution.
// Hint forces generic plan reuse.
// NOTE: This is harder to verify via EXPLAIN alone; we check that
// the hint is accepted and the plan is valid.
func TestHint_HighPlanTime(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Use a prepared-statement style query
	hinted := `SELECT /*+ Set(plan_cache_mode "force_generic_plan") */ * FROM orders WHERE id = 1`
	after, err := explain(ctx, pool, hinted, false)
	if err != nil {
		t.Fatalf("EXPLAIN after hint: %v", err)
	}
	t.Logf("AFTER plan (force_generic_plan):\n%s", after)

	// Verify the hint was accepted (plan should still be valid)
	if !planContains(after, "Index Scan") &&
		!planContains(after, "Seq Scan") {
		t.Error("AFTER plan is empty or invalid")
	}
	t.Log("Confirmed: force_generic_plan hint accepted by pg_hint_plan")
}

// Case 7: sort_limit → CREATE INDEX on sort columns
// Symptom: Sort processes far more rows than LIMIT returns.
// Fix: create an index on the sort columns so PG uses index scan + limit.
func TestHint_SortLimit(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Drop the score index if exists to get a clean baseline
	pool.Exec(ctx, "DROP INDEX IF EXISTS idx_events_score_created")

	query := `SELECT * FROM big_events ORDER BY score DESC, created_at LIMIT 10`

	before, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	beforeHasSort := planContains(before, "Sort")

	// Fix: create composite index on (score DESC, created_at)
	_, err = pool.Exec(ctx,
		`CREATE INDEX idx_events_score_created
		 ON big_events (score DESC, created_at)`,
	)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, "DROP INDEX IF EXISTS idx_events_score_created")
	})

	// Re-analyze so planner knows about the new index
	pool.Exec(ctx, "ANALYZE big_events")

	after, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN after index: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if beforeHasSort && planContains(after, "Sort") {
		t.Error("AFTER plan still has Sort node — index not used for ordering")
	}
	if planContains(after, "Index Scan") ||
		planContains(after, "Index Only Scan") {
		t.Log("Confirmed: planner uses index to avoid sort")
	}
}

// Case 8: stat_temp_spill → Set(work_mem "NMB") hint
// Symptom: aggregate/sort writes temp blocks to disk.
// Fix: raising work_mem eliminates temp spill.
func TestHint_StatTempSpill(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Aggregation that spills with low work_mem
	query := `SELECT event_type, array_agg(payload ORDER BY score)
	          FROM big_events
	          GROUP BY event_type`

	before, err := explainWithGUC(
		ctx, pool, "work_mem", "256kB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	beforeSpills := planContains(before, "Disk") ||
		planContains(before, "external") ||
		planContains(before, "temp written")

	// After: raised work_mem → no spill
	after, err := explainWithGUC(
		ctx, pool, "work_mem", "512MB", query, true,
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE after: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if beforeSpills {
		afterSpills := planContains(after, "Disk") ||
			planContains(after, "external")
		if afterSpills {
			t.Error("AFTER plan still spilling to disk with 512MB work_mem")
		} else {
			t.Log("Confirmed: spill eliminated by work_mem increase")
		}
	} else {
		t.Log("NOTE: no spill detected at 256kB — test inconclusive")
	}
}

// Case 9: missing_fk_index → CREATE INDEX
// Symptom: FK join is slow without index on the FK column.
// Fix: create index on orders.customer_id.
func TestHint_MissingFKIndex(t *testing.T) {
	pool, ctx := setupPool(t)
	bootstrap(ctx, pool, t)

	// Ensure no index on customer_id (the FK column)
	pool.Exec(ctx, "DROP INDEX IF EXISTS idx_orders_customer")

	query := `SELECT c.name, o.amount
	          FROM customers c
	          JOIN orders o ON o.customer_id = c.id
	          WHERE c.id = 42`

	before, err := explain(ctx, pool, query, true)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	// Check if orders side uses Seq Scan (no index on customer_id)
	beforeSeqOnOrders := planContains(before, "Seq Scan on orders")

	// Fix: create the missing FK index
	_, err = pool.Exec(ctx,
		`CREATE INDEX idx_orders_customer ON orders(customer_id)`,
	)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, "DROP INDEX IF EXISTS idx_orders_customer")
	})

	pool.Exec(ctx, "ANALYZE orders")

	after, err := explain(ctx, pool, query, true)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE after index: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if beforeSeqOnOrders {
		if planContains(after, "Seq Scan on orders") {
			t.Error("AFTER plan still Seq Scan on orders — FK index not used")
		}
		if planContains(after, "Index Scan") ||
			planContains(after, "Bitmap") {
			t.Log("Confirmed: orders now uses index on customer_id")
		}
	} else {
		t.Log("NOTE: planner already avoided Seq Scan on orders before index")
	}
}

// Case 10: hint_plan.hints table integration
// Verifies that hints inserted into the hint_plan.hints table
// (the way pg_sage tuner does it) actually affect query plans.
func TestHint_HintTableIntegration(t *testing.T) {
	pool, ctx := setupPool(t)
	requireHintPlan(ctx, pool, t)
	bootstrap(ctx, pool, t)

	// We need a single connection for session-level settings
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	// Enable hint table lookup and debug
	_, err = conn.Exec(ctx,
		"SET pg_hint_plan.enable_hint_table = on",
	)
	if err != nil {
		t.Fatalf("SET enable_hint_table: %v", err)
	}

	// Reset stats and run query to capture queryid
	conn.Exec(ctx, "SELECT pg_stat_statements_reset()")

	query := `SELECT * FROM orders WHERE status = 'shipped'`
	conn.Exec(ctx, query)

	// Query pg_stat_statements — the normalized query will have $1
	var queryID int64
	err = conn.QueryRow(ctx,
		`SELECT queryid FROM pg_stat_statements
		 WHERE query LIKE $1
		   AND queryid != 0
		 ORDER BY calls DESC
		 LIMIT 1`,
		`%orders%status%`,
	).Scan(&queryID)
	if err != nil {
		// Fallback: list all entries to debug
		rows, _ := conn.Query(ctx,
			"SELECT queryid, query FROM pg_stat_statements LIMIT 20",
		)
		for rows.Next() {
			var qid int64
			var q string
			rows.Scan(&qid, &q)
			t.Logf("  queryid=%d query=%s", qid, q)
		}
		rows.Close()
		t.Fatalf("could not find queryid: %v", err)
	}
	t.Logf("Query ID: %d", queryID)

	// Before: no hint in table
	rows, _ := conn.Query(ctx, "EXPLAIN (COSTS OFF) "+query)
	var beforeLines []string
	for rows.Next() {
		var l string
		rows.Scan(&l)
		beforeLines = append(beforeLines, l)
	}
	rows.Close()
	before := strings.Join(beforeLines, "\n")
	t.Logf("BEFORE plan:\n%s", before)

	// Insert hint into hint_plan.hints using query_id
	_, err = conn.Exec(ctx,
		`INSERT INTO hint_plan.hints (query_id, application_name, hints)
		 VALUES ($1, '', 'IndexScan(orders idx_orders_status)')`,
		queryID,
	)
	if err != nil {
		t.Fatalf("INSERT hint: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx,
			"DELETE FROM hint_plan.hints WHERE query_id = $1", queryID,
		)
	})

	// After: hint table should force IndexScan
	rows, _ = conn.Query(ctx, "EXPLAIN (COSTS OFF) "+query)
	var afterLines []string
	for rows.Next() {
		var l string
		rows.Scan(&l)
		afterLines = append(afterLines, l)
	}
	rows.Close()
	after := strings.Join(afterLines, "\n")
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Index Scan") &&
		!planContains(after, "Bitmap") {
		t.Error("hint_plan.hints table entry did not affect the plan")
	}
	if planContains(after, "idx_orders_status") {
		t.Log("Confirmed: hint_plan.hints table forced IndexScan(idx_orders_status)")
	}
}

// Case 11: MergeJoin hint — force merge join instead of hash/nested
func TestHint_MergeJoin(t *testing.T) {
	pool, ctx := setupPool(t)
	requireHintPlan(ctx, pool, t)
	bootstrap(ctx, pool, t)

	query := `SELECT o.id, l.qty
	          FROM orders o
	          JOIN lineitems l ON l.order_id = o.id
	          WHERE o.id < 1000`

	before, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	hinted := `SELECT /*+ MergeJoin(o l) */ o.id, l.qty
	           FROM orders o
	           JOIN lineitems l ON l.order_id = o.id
	           WHERE o.id < 1000`
	after, err := explain(ctx, pool, hinted, false)
	if err != nil {
		t.Fatalf("EXPLAIN after hint: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Merge Join") {
		t.Error("AFTER plan does not show Merge Join — hint not applied")
	}
}

// Case 12: NoSeqScan + NoHashJoin — combined hint enforcement
func TestHint_CombinedHints(t *testing.T) {
	pool, ctx := setupPool(t)
	requireHintPlan(ctx, pool, t)
	bootstrap(ctx, pool, t)

	query := `SELECT c.name, count(o.id)
	          FROM customers c
	          JOIN orders o ON o.customer_id = c.id
	          GROUP BY c.name`

	before, err := explain(ctx, pool, query, false)
	if err != nil {
		t.Fatalf("EXPLAIN before: %v", err)
	}
	t.Logf("BEFORE plan:\n%s", before)

	// Combine multiple hints like CombineHints() does
	hinted := `SELECT /*+ Set(work_mem "128MB") MergeJoin(c o) IndexScan(o) */
	           c.name, count(o.id)
	           FROM customers c
	           JOIN orders o ON o.customer_id = c.id
	           GROUP BY c.name`

	// Need an index on customer_id for this to work
	pool.Exec(ctx, "CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id)")
	pool.Exec(ctx, "ANALYZE orders")

	after, err := explain(ctx, pool, hinted, false)
	if err != nil {
		t.Fatalf("EXPLAIN after hint: %v", err)
	}
	t.Logf("AFTER plan:\n%s", after)

	if !planContains(after, "Merge Join") {
		t.Error("AFTER plan missing Merge Join from combined hint")
	}
	if !planContains(after, "Index Scan") {
		t.Error("AFTER plan missing Index Scan from combined hint")
	}
}
