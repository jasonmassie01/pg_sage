# pg_sage Index Optimizer v2 — Test Plan

## Scope

26 features across 7 parts. Tests organized by priority tier (P0-P3), then by type (unit → integration → live DB). Each feature gets: unit tests (mock/fixture, no DB), integration tests (live PG, no LLM), and end-to-end tests (live PG + real LLM + real DDL execution).

**Test database requirements:**
- PG17 (primary target), PG14 for backwards compat
- pg_stat_statements enabled
- HypoPG extension installed (for P1 tests)
- pg_trgm extension installed (for P2 GIN tests)
- Phase 15 test data loaded (50K/500K/1M rows)
- Gemini API key (for e2e tests)
- Opus/Pro API key (for dual-model tests)

---

## Test Data Schema

Use Phase 15 base data PLUS these additions for optimizer-specific testing:

```sql
-- JSONB column for GIN index tests
ALTER TABLE order_events ADD COLUMN metadata JSONB DEFAULT '{}';
UPDATE order_events SET metadata = jsonb_build_object(
    'source', (ARRAY['web','mobile','api','batch'])[floor(random()*4+1)::int],
    'version', (random()*5+1)::int,
    'tags', ARRAY['tag_'||(random()*20)::int, 'tag_'||(random()*20)::int]
) WHERE order_id <= 100000;

-- Full-text search column for GIN tsvector tests
ALTER TABLE products ADD COLUMN search_vector tsvector;
UPDATE products SET search_vector = to_tsvector('english', name || ' ' || sku);

-- Time-series table for BRIN tests (physically ordered by created_at)
CREATE TABLE sensor_readings (
    reading_id BIGSERIAL PRIMARY KEY,
    sensor_id INT NOT NULL,
    value NUMERIC(10,4),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO sensor_readings (sensor_id, value, created_at)
SELECT (random()*100)::int, random()*1000,
       '2025-01-01'::timestamptz + (g * interval '1 second')
FROM generate_series(1, 1000000) g;
-- Physical order matches created_at order → correlation ≈ 1.0 → BRIN candidate

-- Spatial column for GiST tests (if PostGIS installed)
-- CREATE EXTENSION IF NOT EXISTS postgis;
-- ALTER TABLE customers ADD COLUMN location geometry(Point, 4326);

-- Expression index test table
CREATE TABLE log_entries (
    id SERIAL PRIMARY KEY,
    log_date TIMESTAMPTZ NOT NULL DEFAULT now(),
    level VARCHAR(10),
    message TEXT
);
INSERT INTO log_entries (log_date, level, message)
SELECT now() - (random()*365)::int * interval '1 day',
       (ARRAY['DEBUG','INFO','WARN','ERROR'])[floor(random()*4+1)::int],
       'Log message ' || g
FROM generate_series(1, 500000) g;

-- Non-C locale table for collation tests
-- (The database itself should use en_US.UTF-8 for this test)

-- Table with high write rate for write-impact tests
CREATE TABLE high_write_table (
    id SERIAL PRIMARY KEY,
    counter INT DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT now()
);
INSERT INTO high_write_table (counter) SELECT 0 FROM generate_series(1, 100000);

-- Bloated index for reindex tests
CREATE TABLE bloat_test (id SERIAL PRIMARY KEY, val TEXT);
INSERT INTO bloat_test SELECT g, repeat('x', 100) FROM generate_series(1, 100000) g;
CREATE INDEX idx_bloat_val ON bloat_test (val);
-- Create bloat: update half the rows then delete them
UPDATE bloat_test SET val = repeat('y', 100) WHERE id <= 50000;
DELETE FROM bloat_test WHERE id <= 50000;
-- idx_bloat_val is now significantly bloated

-- Generate query workload for pg_stat_statements
DO $$ BEGIN FOR i IN 1..50 LOOP
    -- Point lookup (needs B-tree)
    PERFORM * FROM orders WHERE customer_id = i;
    -- LIKE prefix (needs pattern_ops on non-C locale)
    PERFORM * FROM customers WHERE email LIKE 'user' || i || '%';
    -- JSONB containment (needs GIN)
    PERFORM * FROM order_events WHERE metadata @> '{"source":"web"}';
    -- Full-text search (needs GIN tsvector)
    PERFORM * FROM products WHERE search_vector @@ to_tsquery('english', 'product');
    -- Time-range on sensor (BRIN candidate)
    PERFORM count(*) FROM sensor_readings WHERE created_at > '2025-06-01' AND created_at < '2025-07-01';
    -- Expression wrapped column
    PERFORM * FROM log_entries WHERE EXTRACT(YEAR FROM log_date) = 2025 AND level = 'ERROR';
    -- Aggregation (materialized view candidate)
    PERFORM status, count(*), avg(total_amount) FROM orders GROUP BY status;
    -- Sort spill (parameter tuning candidate)
    PERFORM * FROM orders ORDER BY total_amount DESC LIMIT 1000;
    -- Cross-table join
    PERFORM o.order_id, c.name FROM orders o JOIN customers c ON o.customer_id = c.customer_id WHERE c.customer_id = i;
END LOOP; END $$;

ANALYZE;
```

---

## P0 Tests: Prevents Real Damage

### P0-1: Column Existence Validation

**Unit tests (no DB):**

```go
func TestValidateColumns_AllExist(t *testing.T)
    // DDL: CREATE INDEX ON orders (customer_id, order_date)
    // Table DDL has both columns
    // Assert: nil error

func TestValidateColumns_Hallucinated(t *testing.T)
    // DDL: CREATE INDEX ON orders (nonexistent_col)
    // Assert: error contains "hallucinated column"

func TestValidateColumns_HallucinatedInclude(t *testing.T)
    // DDL: CREATE INDEX ON orders (customer_id) INCLUDE (fake_col)
    // Assert: error contains "hallucinated INCLUDE column"

func TestValidateColumns_CaseInsensitive(t *testing.T)
    // DDL: CREATE INDEX ON orders (Customer_ID)
    // Table has customer_id (lowercase)
    // Assert: passes (PG identifiers are case-insensitive unless quoted)

func TestValidateColumns_QuotedIdentifier(t *testing.T)
    // DDL: CREATE INDEX ON "Order-Items" ("unit price ($)")
    // Assert: handles quoted identifiers correctly

func TestValidateColumns_ExpressionIndex(t *testing.T)
    // DDL: CREATE INDEX ON log_entries (EXTRACT(YEAR FROM log_date))
    // Assert: does NOT validate expression as column name
    // (Expression indexes reference expressions, not columns directly)

func TestValidateColumns_PartialIndex(t *testing.T)
    // DDL: CREATE INDEX ON orders (customer_id) WHERE status = 'active'
    // Assert: validates customer_id AND status both exist
```

**Integration tests (live PG):**

```go
func TestValidateColumns_LivePG(t *testing.T)
    // Load table DDL from information_schema for real orders table
    // Validate a good DDL → pass
    // Validate a hallucinated DDL → error
```

### P0-2: Duplicate Recommendation Detection

**Unit tests:**

```go
func TestDuplicateDetection_ExactMatch(t *testing.T)
    // Existing: CREATE INDEX idx_a ON orders (customer_id)
    // Recommendation: CREATE INDEX idx_b ON orders (customer_id)
    // Assert: detected as duplicate

func TestDuplicateDetection_SubsetMatch(t *testing.T)
    // Existing: CREATE INDEX idx_a ON orders (customer_id, order_date)
    // Recommendation: CREATE INDEX idx_b ON orders (customer_id)
    // Assert: detected as subset (existing covers recommendation)

func TestDuplicateDetection_SupersetNotDuplicate(t *testing.T)
    // Existing: CREATE INDEX idx_a ON orders (customer_id)
    // Recommendation: CREATE INDEX idx_b ON orders (customer_id, order_date)
    // Assert: NOT duplicate (recommendation is broader)

func TestDuplicateDetection_IncludeDifference(t *testing.T)
    // Existing: CREATE INDEX idx_a ON orders (customer_id)
    // Recommendation: CREATE INDEX idx_b ON orders (customer_id) INCLUDE (total_amount)
    // Assert: NOT duplicate (INCLUDE upgrade is valid)

func TestDuplicateDetection_PartialVsFull(t *testing.T)
    // Existing: CREATE INDEX idx_a ON orders (customer_id)
    // Recommendation: CREATE INDEX idx_b ON orders (customer_id) WHERE status = 'active'
    // Assert: NOT duplicate (partial index is narrower)

func TestDuplicateDetection_DifferentOpClass(t *testing.T)
    // Existing: CREATE INDEX idx_a ON customers (email)
    // Recommendation: CREATE INDEX idx_b ON customers (email varchar_pattern_ops)
    // Assert: NOT duplicate (different operator class serves different queries)
```

**Integration tests (live PG):**

```go
func TestDuplicateDetection_LivePG(t *testing.T)
    // Create a real index on orders(customer_id)
    // Run duplicate detection against recommendation for same columns
    // Assert: duplicate detected
    // Cleanup: DROP INDEX
```

### P0-3: Cold Start Protection

**Unit tests:**

```go
func TestColdStart_ZeroSnapshots(t *testing.T)
    // snapshotCount = 0
    // Assert: shouldAnalyzeTable returns false

func TestColdStart_OneSnapshot(t *testing.T)
    // snapshotCount = 1
    // Assert: shouldAnalyzeTable returns false (need 2 for write rate delta)

func TestColdStart_TwoSnapshots(t *testing.T)
    // snapshotCount = 2
    // Assert: shouldAnalyzeTable returns true

func TestColdStart_WriteRateUnknown(t *testing.T)
    // writeRate = -1 (sentinel for unknown)
    // Assert: table skipped, logged as "need 2+ snapshots"
```

### P0-4: Post-Execution indisvalid Check

**Integration tests (live PG):**

```go
func TestIndisvalidCheck_ValidIndex(t *testing.T)
    // CREATE INDEX CONCURRENTLY on test table
    // Assert: indisvalid = true
    // Cleanup

func TestIndisvalidCheck_InvalidIndex(t *testing.T)
    // Create a scenario where CONCURRENTLY leaves INVALID index:
    // (Hard to trigger reliably — use a unique index with duplicate values)
    // CREATE TABLE inv_test (id INT);
    // INSERT INTO inv_test VALUES (1), (1);
    // CREATE UNIQUE INDEX CONCURRENTLY idx_inv ON inv_test (id);
    // Assert: indisvalid = false
    // Assert: optimizer logs critical finding
    // Assert: optimizer does NOT proceed with DROP of old index
    // Cleanup

func TestIndisvalidCheck_DropBlockedOnInvalid(t *testing.T)
    // Simulate INCLUDE upgrade scenario:
    // Create old index, create new index (invalid), verify old index NOT dropped
```

### P0-5: Write Impact Pre-Check

**Unit tests:**

```go
func TestWriteImpact_LowWrite(t *testing.T)
    // writeRate: 100/day, existingIndexes: 3, newIndexes: 1
    // Assert: Acceptable = true, PctIncrease < 15%

func TestWriteImpact_HighWrite(t *testing.T)
    // writeRate: 100000/day, existingIndexes: 3, newIndexes: 3
    // Assert: Acceptable = false, PctIncrease > 15%

func TestWriteImpact_Boundary(t *testing.T)
    // Find exact boundary where PctIncrease = 15%
    // Assert: values below → Acceptable, above → not

func TestWriteImpact_ZeroExistingIndexes(t *testing.T)
    // writeRate: 50000, existingIndexes: 0, newIndexes: 1
    // Assert: no division by zero, computes correctly

func TestWriteImpact_CustomThreshold(t *testing.T)
    // threshold: 5% (stricter than default 15%)
    // Same write rate that passes at 15% → fails at 5%
```

---

## P1 Tests: Major Quality Upgrade

### P1-6: HypoPG Validation

**Unit tests (mock):**

```go
func TestHypoPG_Available(t *testing.T)
    // Mock: pg_extension has 'hypopg'
    // Assert: hasHypoPG returns true

func TestHypoPG_NotAvailable(t *testing.T)
    // Mock: pg_extension does NOT have 'hypopg'
    // Assert: hasHypoPG returns false
    // Assert: falls back to non-HypoPG validation
```

**Integration tests (live PG + HypoPG):**

```go
func TestHypoPG_CreateAndValidate(t *testing.T)
    // CREATE EXTENSION IF NOT EXISTS hypopg
    // Create hypothetical index on orders(customer_id)
    // EXPLAIN SELECT * FROM orders WHERE customer_id = 1 (with hypo)
    // Assert: plan shows Index Scan on hypothetical index
    // EXPLAIN without hypo → Seq Scan
    // Assert: improvement > 10%
    // hypopg_reset()

func TestHypoPG_RejectsUselessIndex(t *testing.T)
    // Create hypothetical index on orders(status) — low selectivity
    // EXPLAIN SELECT * FROM orders WHERE status = 'active'
    // PG planner may still choose Seq Scan (20% of rows)
    // Assert: improvement < 10% → recommendation REJECTED

func TestHypoPG_AcceptsGoodIndex(t *testing.T)
    // Create hypothetical index on orders(customer_id)
    // EXPLAIN SELECT * FROM orders WHERE customer_id = 42
    // Assert: plan switches from Seq Scan to Index Scan
    // Assert: improvement > 80% → recommendation ACCEPTED

func TestHypoPG_SizeEstimate(t *testing.T)
    // Create hypothetical index
    // hypopg_relation_size() returns estimated size
    // Assert: size > 0 and reasonable (between 1MB and 100MB for 500K rows)

func TestHypoPG_PartialIndex(t *testing.T)
    // Hypothetical: CREATE INDEX ON orders (customer_id) WHERE status = 'active'
    // Assert: planner uses it for WHERE customer_id=$1 AND status='active'
    // Assert: planner does NOT use it for WHERE customer_id=$1 (no status filter)

func TestHypoPG_IncludeColumns(t *testing.T)
    // Hypothetical: CREATE INDEX ON orders (customer_id) INCLUDE (total_amount, order_date)
    // EXPLAIN SELECT customer_id, total_amount FROM orders WHERE customer_id = 42
    // Assert: plan shows "Index Only Scan" (not "Index Scan" with heap fetches)

func TestHypoPG_GINIndex(t *testing.T)
    // Hypothetical: CREATE INDEX ON order_events USING gin (metadata jsonb_path_ops)
    // EXPLAIN SELECT * FROM order_events WHERE metadata @> '{"source":"web"}'
    // Assert: plan shows Bitmap Index Scan on hypothetical GIN

func TestHypoPG_BRINIndex(t *testing.T)
    // Hypothetical: CREATE INDEX ON sensor_readings USING brin (created_at)
    // EXPLAIN SELECT * FROM sensor_readings WHERE created_at > '2025-06-01'
    // Assert: plan shows Bitmap Index Scan on hypothetical BRIN
    // Assert: estimated size much smaller than equivalent B-tree

func TestHypoPG_MultipleQueries(t *testing.T)
    // One hypothetical index, test against 5 different queries
    // Assert: improvement computed per query, average returned
    // Assert: queries that don't benefit don't inflate the score

func TestHypoPG_Cleanup(t *testing.T)
    // Create 10 hypothetical indexes
    // hypopg_reset()
    // Assert: hypopg_list_indexes returns 0 rows
    // Assert: EXPLAIN plans no longer reference hypothetical indexes

func TestHypoPG_FallbackWhenMissing(t *testing.T)
    // DROP EXTENSION hypopg (or run on instance without it)
    // Run optimizer → falls back to column/duplicate/write checks
    // Assert: recommendations still produced (lower confidence)
    // Assert: log message recommends installing HypoPG
```

### P1-7: Plan-Aware Optimization

**Unit tests:**

```go
func TestPlanSummary_SeqScan(t *testing.T)
    // Input: EXPLAIN JSON with Seq Scan node
    // Assert: summary = "Seq Scan on orders → Filter: customer_id=$1 → Rows Removed: 499,999"

func TestPlanSummary_IndexScanWithHeapFetches(t *testing.T)
    // Input: EXPLAIN JSON with Index Scan + Heap Fetches: 487,000
    // Assert: summary includes "Heap Fetches: 487,000"
    // Assert: bottleneck = "INCLUDE columns needed"

func TestPlanSummary_IndexOnlyScan(t *testing.T)
    // Input: EXPLAIN JSON with Index Only Scan
    // Assert: summary = "Index Only Scan — no heap fetches"
    // Assert: bottleneck = "none — optimal"

func TestPlanSummary_SortSpill(t *testing.T)
    // Input: EXPLAIN JSON with Sort Method: external merge, Disk: 48MB
    // Assert: bottleneck = "Sort spill to disk — increase work_mem"

func TestPlanSummary_HashBatches(t *testing.T)
    // Input: EXPLAIN JSON with Hash Batches: 16
    // Assert: bottleneck = "Hash spill — increase work_mem"

func TestPlanSummary_NestedLoop(t *testing.T)
    // Input: EXPLAIN JSON with Nested Loop, inner=Seq Scan on orders
    // Assert: identifies orders as the slow side of the join

func TestPlanSummary_ContextBudget(t *testing.T)
    // Complex 10-way join plan (5KB JSON)
    // Assert: summary is < 300 bytes
    // Assert: key signals preserved (node types, row counts, heap fetches)

func TestPlanSummary_NullPlan(t *testing.T)
    // No plan available (PG14, no explain_cache)
    // Assert: returns empty string, no error
    // Assert: optimizer proceeds with query-text-only (v1 behavior)
```

**Integration tests (live PG):**

```go
func TestPlanCapture_GenericPlan_PG17(t *testing.T)
    // Run: EXPLAIN (GENERIC_PLAN, FORMAT JSON) SELECT * FROM orders WHERE customer_id = $1
    // Assert: returns valid plan JSON
    // Assert: shows Seq Scan (no index on customer_id)

func TestPlanCapture_GenericPlan_PG14(t *testing.T)
    // PG14 doesn't have GENERIC_PLAN
    // Assert: falls back gracefully (no error, no plan)

func TestPlanCapture_ExplainCache(t *testing.T)
    // Extension mode: check sage.explain_cache for queryid
    // Assert: plan JSON present for slow queries
    // (Sidecar mode: N/A — uses GENERIC_PLAN instead)
```

### P1-8: INCLUDE Column Intelligence

**Unit tests:**

```go
func TestIncludeDecision_HeapFetchesHigh(t *testing.T)
    // Plan: Index Scan, Heap Fetches: 487,000
    // SELECT columns: customer_id (in index), total_amount (NOT in index)
    // Assert: recommends INCLUDE (total_amount)

func TestIncludeDecision_IndexOnlyScanAlready(t *testing.T)
    // Plan: Index Only Scan, Heap Fetches: 0
    // Assert: no INCLUDE recommendation (already optimal)

func TestIncludeDecision_SeqScanFirst(t *testing.T)
    // Plan: Seq Scan (no index at all)
    // Assert: recommends base index first, NOT INCLUDE
    // INCLUDE is second-pass after base index exists

func TestIncludeDecision_TooManyColumns(t *testing.T)
    // SELECT has 15 columns not in index
    // max_include_columns = 5
    // Assert: INCLUDE limited to top 5 by frequency/benefit

func TestIncludeDecision_WidenExisting(t *testing.T)
    // Existing: idx ON orders (customer_id)
    // Plan shows Heap Fetches for total_amount, order_date
    // Assert: recommends DROP old + CREATE new with INCLUDE
    // Assert: DDL has both drop_ddl and create_ddl
```

### P1-9: Partial Index Detection

**Unit tests:**

```go
func TestPartialIndex_HighFrequencyFilter(t *testing.T)
    // 400/500 queries filter status='active'
    // pg_stats: 'active' frequency = 0.05 (5% of rows)
    // Assert: recommends WHERE status = 'active'
    // Assert: estimated size = ~5% of full index

func TestPartialIndex_LowFrequencyFilter(t *testing.T)
    // 50/500 queries filter status='active' (10%)
    // Assert: does NOT recommend partial index (< 80% threshold)

func TestPartialIndex_HighSelectivity(t *testing.T)
    // 400/500 queries filter status='active'
    // pg_stats: 'active' frequency = 0.80 (80% of rows)
    // Assert: does NOT recommend partial (index wouldn't be smaller)

func TestPartialIndex_MultipleConstants(t *testing.T)
    // 300/500 filter status='active', 150/500 filter status='pending'
    // Assert: recommends partial WHERE status='active' (highest frequency)
    // Assert: does NOT recommend partial WHERE status='pending' (below 80%)

func TestPartialIndex_NullFilter(t *testing.T)
    // Queries filter WHERE deleted_at IS NULL (90% of traffic)
    // Assert: recommends WHERE deleted_at IS NULL
```

**Integration tests (live PG + HypoPG):**

```go
func TestPartialIndex_PlannerUsesIt(t *testing.T)
    // HypoPG: CREATE INDEX ON orders (customer_id) WHERE status = 'active'
    // EXPLAIN SELECT * FROM orders WHERE customer_id=42 AND status='active'
    // Assert: planner uses the partial hypothetical index
    // EXPLAIN SELECT * FROM orders WHERE customer_id=42 AND status='pending'
    // Assert: planner does NOT use it (wrong predicate)
```

### P1-10: Dual-Model Architecture

**Unit tests:**

```go
func TestDualModel_OptimizerConfigured(t *testing.T)
    // optimizer_llm.endpoint set, optimizer_llm.model set
    // Assert: GetOptimizerClient() returns separate client

func TestDualModel_OptimizerNotConfigured(t *testing.T)
    // optimizer_llm section empty
    // Assert: GetOptimizerClient() returns general client (v1 behavior)

func TestDualModel_OptimizerModelOnly(t *testing.T)
    // Same endpoint, different model
    // Assert: GetOptimizerClient() uses general endpoint + optimizer model

func TestDualModel_SeparateBudgets(t *testing.T)
    // General budget: 100K, Optimizer budget: 50K
    // Exhaust optimizer budget (50K tokens)
    // Assert: optimizer calls rejected
    // Assert: general calls still work (separate budget)

func TestDualModel_FallbackOnBudgetExhausted(t *testing.T)
    // optimizer budget exhausted, fallback_to_general=true
    // Assert: optimizer call uses general model
    // Assert: general model still tracks its own budget

func TestDualModel_FallbackDisabled(t *testing.T)
    // optimizer budget exhausted, fallback_to_general=false
    // Assert: optimizer call returns error
    // Assert: finding created with Tier 1 data (no LLM)

func TestDualModel_SeparateCircuitBreakers(t *testing.T)
    // Break optimizer endpoint (invalid URL)
    // Assert: optimizer circuit opens
    // Assert: general circuit stays closed
    // Assert: briefings still use general model

func TestDualModel_PrometheusMetrics(t *testing.T)
    // Make calls to both models
    // Assert: pg_sage_llm_calls_total{model="flash",purpose="briefing"} > 0
    // Assert: pg_sage_llm_calls_total{model="opus",purpose="index_optimization"} > 0
    // Assert: separate token counters for each model
```

**Integration tests (live LLM):**

```go
func TestDualModel_OpusProducesValidDDL(t *testing.T)
    // Send real table context to Opus/Pro
    // Assert: response is valid JSON array
    // Assert: DDL uses CONCURRENTLY
    // Assert: no truncation (response complete)
    // Compare: same prompt to Flash — check if Flash truncates

func TestDualModel_CostTracking(t *testing.T)
    // Run optimizer with Opus for 5 tables
    // Assert: tokens_used_today for Opus model > 0
    // Assert: tokens_used_today for Flash model unchanged
```

### P1-11: Confidence Scoring

**Unit tests:**

```go
func TestConfidence_HighConfidence(t *testing.T)
    // queryVolume=500/day, planAvail=true, writeRateKnown=true, hypoPGValidated=true, selectivityKnown=true
    // Assert: Overall > 0.8, ActionLevel = "autonomous"

func TestConfidence_MediumConfidence(t *testing.T)
    // queryVolume=50/day, planAvail=false, writeRateKnown=true, hypoPGValidated=false
    // Assert: Overall 0.5-0.8, ActionLevel = "advisory"

func TestConfidence_LowConfidence(t *testing.T)
    // queryVolume=3/day, planAvail=false, writeRateKnown=false, hypoPGValidated=false
    // Assert: Overall < 0.5, ActionLevel = "informational"

func TestConfidence_ColdStartPenalty(t *testing.T)
    // writeRateKnown = false
    // Assert: Overall drops significantly (cold start protection)

func TestConfidence_HypoPGBoost(t *testing.T)
    // Same inputs but hypoPGValidated true vs false
    // Assert: HypoPG validation adds significant confidence
```

---

## P2 Tests: Broader Coverage

### P2-12: Non-B-tree Index Types

**Unit tests:**

```go
func TestIndexTypeDetection_GINJsonb(t *testing.T)
    // Query: WHERE metadata @> '{"source":"web"}'
    // Assert: recommends GIN with jsonb_ops or jsonb_path_ops

func TestIndexTypeDetection_GINArray(t *testing.T)
    // Query: WHERE tags && ARRAY['tag_1']
    // Assert: recommends GIN with array_ops

func TestIndexTypeDetection_GINTrgm(t *testing.T)
    // Query: WHERE name LIKE '%search%' (leading wildcard)
    // Assert: recommends GIN with gin_trgm_ops
    // Assert: requires pg_trgm extension

func TestIndexTypeDetection_GINTsvector(t *testing.T)
    // Query: WHERE search_vector @@ to_tsquery('english', 'product')
    // Assert: recommends GIN on tsvector column

func TestIndexTypeDetection_BRIN(t *testing.T)
    // Query: WHERE created_at > X on sensor_readings
    // pg_stats: correlation = 0.99 for created_at
    // Assert: recommends BRIN (not B-tree)

func TestIndexTypeDetection_BRINLowCorrelation(t *testing.T)
    // Query: WHERE created_at > X on orders
    // pg_stats: correlation = 0.02 for created_at (random insertion order)
    // Assert: does NOT recommend BRIN (correlation too low)
    // Assert: recommends B-tree instead

func TestIndexTypeDetection_DefaultBTree(t *testing.T)
    // Query: WHERE customer_id = $1 (equality on INT)
    // Assert: recommends B-tree (default, not GIN/BRIN)
```

**Integration tests (live PG + HypoPG):**

```go
func TestGINHypoPG_JsonbContainment(t *testing.T)
    // HypoPG: CREATE INDEX USING gin (metadata jsonb_path_ops)
    // EXPLAIN: WHERE metadata @> '{"source":"web"}'
    // Assert: planner uses hypothetical GIN index
    // Assert: improvement > 10%

func TestBRINHypoPG_TimeRange(t *testing.T)
    // HypoPG: CREATE INDEX USING brin (created_at) ON sensor_readings
    // EXPLAIN: WHERE created_at > '2025-06-01'
    // Assert: planner uses hypothetical BRIN
    // Assert: estimated size << B-tree size
```

### P2-13: Expression Index Detection

**Unit tests:**

```go
func TestExpressionIndex_ExtractYear(t *testing.T)
    // Plan: Filter: (extract(year from log_date) = 2025)
    // Assert: recommends CREATE INDEX ON log_entries (EXTRACT(YEAR FROM log_date))

func TestExpressionIndex_Lower(t *testing.T)
    // Plan: Filter: (lower(email) = 'test@example.com')
    // Assert: recommends CREATE INDEX ON customers (lower(email))

func TestExpressionIndex_VolatileFunction(t *testing.T)
    // Expression uses random() or now() (VOLATILE)
    // Assert: REJECTED — expression indexes require IMMUTABLE functions

func TestExpressionIndex_ImmutableCheck(t *testing.T)
    // Expression uses EXTRACT (IMMUTABLE)
    // Assert: ACCEPTED
```

### P2-14: Collation Awareness

**Unit tests:**

```go
func TestCollation_NonCLocale_LikePrefix(t *testing.T)
    // Database: en_US.UTF-8
    // Query: WHERE email LIKE 'user42%'
    // Assert: recommends varchar_pattern_ops (not default text_ops)

func TestCollation_CLocale_LikePrefix(t *testing.T)
    // Database: C
    // Query: WHERE email LIKE 'user42%'
    // Assert: recommends default B-tree (pattern_ops not needed)

func TestCollation_NonCLocale_Equality(t *testing.T)
    // Database: en_US.UTF-8
    // Query: WHERE email = 'test@example.com'
    // Assert: recommends default B-tree (equality works with any collation)

func TestCollation_IncludedInPrompt(t *testing.T)
    // Assert: LLM prompt context includes database collation
```

**Integration tests (live PG):**

```go
func TestCollation_LiveDetection(t *testing.T)
    // SHOW lc_collate
    // Assert: collation string detected and included in context assembly
```

### P2-15: Extension/Operator Class Validation

**Unit tests:**

```go
func TestExtensionValidation_TrgmMissing(t *testing.T)
    // Recommendation: GIN with gin_trgm_ops
    // pg_extension does NOT have pg_trgm
    // Assert: validation error "pg_trgm extension required"

func TestExtensionValidation_TrgmPresent(t *testing.T)
    // Same recommendation, pg_trgm IS installed
    // Assert: passes validation

func TestExtensionValidation_PostGISMissing(t *testing.T)
    // Recommendation: GiST on geometry column
    // postgis NOT installed
    // Assert: validation error

func TestExtensionValidation_BRINCorrelationLow(t *testing.T)
    // Recommendation: BRIN on orders(created_at)
    // correlation = 0.02
    // Assert: validation error "BRIN ineffective: correlation 0.02 < 0.8"

func TestExtensionValidation_BRINCorrelationHigh(t *testing.T)
    // Recommendation: BRIN on sensor_readings(created_at)
    // correlation = 0.99
    // Assert: passes
```

### P2-16: Cost Estimation

**Unit tests:**

```go
func TestCostEstimate_IndexSize(t *testing.T)
    // 500K rows, 90 bytes/entry estimated
    // Assert: estimated size ≈ 45MB

func TestCostEstimate_WriteAmplification(t *testing.T)
    // 12K writes/day, 3 existing indexes, 1 new
    // Assert: additional writes = 12K/day
    // Assert: pct increase ≈ 25% (12K / (12K*4))

func TestCostEstimate_QueryTimeSaved(t *testing.T)
    // Q1: 2000ms → 5ms estimated, 500 calls/day
    // Assert: savings = (2000-5) * 500 / 1000 = 997.5 seconds/day

func TestCostEstimate_BreakEven(t *testing.T)
    // High benefit (997s saved), low cost (3% write overhead)
    // Assert: ROI = "immediate"
    // Low benefit (10s saved), high cost (15% write overhead)
    // Assert: ROI = "marginal — advisory only"

func TestCostEstimate_HypoPGSize(t *testing.T)
    // Use hypopg_relation_size() instead of estimation
    // Assert: uses HypoPG size when available, falls back to estimation
```

### P2-17: Query Fingerprinting

**Unit tests:**

```go
func TestFingerprint_NormalizeINList(t *testing.T)
    // Input: WHERE id IN ($1, $2, $3, $4, $5)
    // Assert: fingerprint = WHERE id IN ($...)

func TestFingerprint_CollapseWhitespace(t *testing.T)
    // Input: "SELECT  *   FROM   orders    WHERE  id = $1"
    // Assert: "select * from orders where id = $1"

func TestFingerprint_GroupByFingerprint(t *testing.T)
    // 3 queries with same fingerprint, different queryids
    // Assert: grouped into 1 representative query
    // Assert: aggregated stats (total calls = sum of all 3)

func TestFingerprint_DifferentPatterns(t *testing.T)
    // SELECT * FROM orders WHERE customer_id = $1
    // SELECT * FROM orders WHERE order_date > $1
    // Assert: different fingerprints (different columns)

func TestFingerprint_ORMVariants(t *testing.T)
    // IN ($1,$2) and IN ($1,$2,$3,$4,$5) → same fingerprint
    // SELECT id, name and SELECT id, name, email → different fingerprints
```

### P2-18: Cross-Table Join Optimization

**Unit tests:**

```go
func TestJoinDetection_SimpleJoin(t *testing.T)
    // Query: SELECT * FROM orders o JOIN customers c ON o.customer_id = c.customer_id
    // Assert: detects join pair (orders, customers) on customer_id

func TestJoinDetection_MultiJoin(t *testing.T)
    // Query with 3-way join
    // Assert: detects all table pairs

func TestJoinContext_BothTablesInPrompt(t *testing.T)
    // Join pair (orders, customers) with 500 calls/day
    // Assert: BOTH tables sent in same LLM prompt
    // Assert: join pattern included in context

func TestJoinContext_OneSideAlreadyIndexed(t *testing.T)
    // customers has PK on customer_id (indexed)
    // orders has no index on customer_id
    // Assert: only recommends index on orders side
```

### P2-19: Workload Classification

**Unit tests:**

```go
func TestClassification_WriteHeavy(t *testing.T)
    // writeRatio = 0.85
    // Assert: classification = "OLTP-write-heavy"
    // Assert: prompt includes "minimal indexing" guidance

func TestClassification_OLAP(t *testing.T)
    // writeRatio = 0.05, avgScanRows = 500000
    // Assert: classification = "OLAP"
    // Assert: prompt includes "covering indexes, BRIN" guidance

func TestClassification_HTAP(t *testing.T)
    // writeRatio = 0.35, mix of point lookups and range scans
    // Assert: classification = "HTAP"

func TestClassification_UnknownWriteRate(t *testing.T)
    // writeRate = -1 (cold start)
    // Assert: classification = "unknown" — no classification guidance sent
```

---

## P3 Tests: Advanced Features

### P3-20: Selectivity Data

```go
func TestSelectivity_FeedToPrompt(t *testing.T)
    // pg_stats: n_distinct=5, most_common_vals={active,shipped,...}, most_common_freqs={0.05,0.25,...}
    // Assert: prompt includes "status has 5 distinct values, 'active'=5% of rows"

func TestSelectivity_HighDistinct(t *testing.T)
    // n_distinct = -0.99 (99% unique)
    // Assert: prompt says "near-unique column — B-tree point lookup efficient"

func TestSelectivity_LowDistinct(t *testing.T)
    // n_distinct = 3
    // Assert: prompt warns "only 3 distinct values — index may not help unless partial"
```

### P3-21: Index Usage Decay

```go
func TestDecay_Declining(t *testing.T)
    // idx_scan history: [10000, 5000, 1000, 200, 50]
    // Assert: trend = "DECAYING", decline = -95%

func TestDecay_Stable(t *testing.T)
    // idx_scan history: [10000, 10200, 9800, 10100, 10000]
    // Assert: trend = "STABLE"

func TestDecay_Growing(t *testing.T)
    // idx_scan history: [100, 500, 2000, 8000, 15000]
    // Assert: trend = "GROWING"

func TestDecay_ZeroRecent(t *testing.T)
    // idx_scan history: [5000, 2000, 500, 0, 0]
    // Assert: trend = "DEAD" (not just DECAYING)
```

### P3-22: Enhanced Regression Detection

```go
func TestRegression_QueryLatencyOnly(t *testing.T)
    // queryLatencyPctChange = 25% (above 10% threshold)
    // Assert: shouldRollback = true

func TestRegression_WriteAmplification(t *testing.T)
    // queryLatencyPctChange = 0% (reads fine)
    // insertLatencyDelta = 30% (writes degraded)
    // Assert: shouldRollback = true

func TestRegression_WALSpike(t *testing.T)
    // walBytesPerSecDelta = 60% (above 50% threshold)
    // Assert: shouldRollback = true

func TestRegression_AllStable(t *testing.T)
    // All signals < threshold
    // Assert: shouldRollback = false
```

### P3-23: Materialized View Detection

```go
func TestMatView_FullTableAggregation(t *testing.T)
    // Query: SELECT status, count(*), avg(total_amount) FROM orders GROUP BY status
    // Plan: Seq Scan → GroupAggregate, 500K rows, 3000ms, 200 calls/day
    // Assert: category = "materialized_view_candidate"
    // Assert: severity = "info" (advisory only, NOT auto-executed)

func TestMatView_NotForIndexableQuery(t *testing.T)
    // Query: SELECT * FROM orders WHERE customer_id = $1
    // Assert: NOT a materialized view candidate (fixable with index)

func TestMatView_RefreshSchedule(t *testing.T)
    // 200 calls/day at 3000ms, estimated refresh = 3s
    // Assert: recommendation includes refresh schedule estimate
```

### P3-24: Parameter Tuning

```go
func TestParamTuning_SortSpill(t *testing.T)
    // Plan: Sort Method: external merge, Disk: 48MB
    // Assert: category = "parameter_tuning"
    // Assert: recommendation = "SET work_mem = '64MB'"

func TestParamTuning_HashBatches(t *testing.T)
    // Plan: Hash Batches: 16
    // Assert: recommendation = increase work_mem

func TestParamTuning_NotAutoExecuted(t *testing.T)
    // Assert: severity = "info" (advisory)
    // Assert: action_risk = "high" (never auto-execute global params)
```

### P3-25: Reindex Detection

```go
func TestReindex_BloatedIndex(t *testing.T)
    // pg_relation_size(index) > 2x estimated minimum
    // Assert: recommends REINDEX CONCURRENTLY
    // Assert: action_risk = "safe" (CONCURRENTLY is non-blocking)

func TestReindex_NotBloated(t *testing.T)
    // pg_relation_size(index) ≈ estimated minimum
    // Assert: no reindex recommendation

func TestReindex_LivePG(t *testing.T)
    // Use bloat_test table from test data (deliberately bloated)
    // Assert: idx_bloat_val detected as bloated
    // Assert: REINDEX CONCURRENTLY recommended
```

### P3-26: Per-Table Circuit Breaker

```go
func TestTableCircuit_OpensAfter3Failures(t *testing.T)
    // 3 consecutive failed recommendations for table "orders"
    // Assert: circuit open, table skipped on next cycle

func TestTableCircuit_CooldownAndHalfOpen(t *testing.T)
    // Circuit open, wait 24 hours (mock time)
    // Assert: half-open — one attempt allowed
    // If it succeeds: circuit closes
    // If it fails: circuit opens for 7 days

func TestTableCircuit_DifferentTablesIndependent(t *testing.T)
    // Circuit open for "orders"
    // Assert: "customers" still processed normally

func TestTableCircuit_SuccessResets(t *testing.T)
    // 2 failures, then 1 success
    // Assert: failure count reset to 0 (not 3, so circuit doesn't open)
```

---

## End-to-End Tests (Live PG + Real LLM + Real DDL)

These run the full pipeline against a real database with a real LLM.

```go
func TestE2E_FullCycle_Flash(t *testing.T)
    // General model (Flash): send table context for orders
    // Assert: receives JSON recommendations
    // Assert: at least 1 recommendation uses CONCURRENTLY
    // Validate with HypoPG
    // Execute via executor
    // Verify: index exists, indisvalid=true

func TestE2E_FullCycle_Opus(t *testing.T)
    // Optimizer model (Opus): same table context
    // Assert: response not truncated (unlike Flash)
    // Assert: rationale is more detailed than Flash
    // Assert: DDL is valid

func TestE2E_DualModel_Routing(t *testing.T)
    // Configure both models
    // Trigger briefing → assert Flash used
    // Trigger optimization → assert Opus used
    // Verify Prometheus metrics show both models

func TestE2E_HypoPG_RejectsUseless(t *testing.T)
    // LLM recommends index on low-selectivity column
    // HypoPG shows planner won't use it
    // Assert: recommendation rejected, NOT executed

func TestE2E_PartialIndex_Created(t *testing.T)
    // LLM recommends partial index based on filter frequency
    // HypoPG validates planner uses it
    // Executor creates it with CONCURRENTLY
    // Assert: index exists with WHERE clause

func TestE2E_IncludeUpgrade(t *testing.T)
    // Existing index on orders(customer_id)
    // LLM recommends INCLUDE upgrade
    // Assert: new index created, old index dropped AFTER validation
    // Assert: new index has INCLUDE columns, indisvalid=true

func TestE2E_GINIndex(t *testing.T)
    // JSONB queries on order_events.metadata
    // LLM recommends GIN with jsonb_path_ops
    // Assert: GIN index created, EXPLAIN shows Bitmap Index Scan

func TestE2E_ColdStartBlocked(t *testing.T)
    // Fresh database, 0 snapshots
    // Assert: optimizer produces 0 recommendations
    // Wait for 2 collector cycles
    // Assert: optimizer now produces recommendations

func TestE2E_WriteImpactBlocked(t *testing.T)
    // High-write table (100K writes/day)
    // LLM recommends 3 indexes (>15% write impact)
    // Assert: recommendations downgraded to advisory
    // Assert: NOT auto-executed

func TestE2E_RegressionRollback(t *testing.T)
    // Create an index that causes write regression
    // Assert: rollback monitor detects regression
    // Assert: index dropped, action_log shows outcome='rolled_back'
```

---

## Test Execution Strategy

### Unit tests (no DB, no LLM):
```bash
go test ./internal/optimizer/... -count=1 -timeout 60s
# Expected: ~80 tests, < 5s
```

### Integration tests (live PG + HypoPG, no LLM):
```bash
SAGE_DATABASE_URL="postgres://..." go test ./internal/optimizer/... -tags=integration -count=1 -timeout 120s
# Expected: ~30 tests, < 30s
```

### End-to-end tests (live PG + real LLM):
```bash
SAGE_DATABASE_URL="postgres://..." SAGE_GEMINI_API_KEY="..." SAGE_OPTIMIZER_LLM_API_KEY="..." \
go test ./internal/optimizer/... -tags=e2e -count=1 -timeout 600s -p 1
# Expected: ~12 tests, < 5 minutes (dominated by LLM latency)
```

### Full suite:
```bash
go test ./... -count=1 -timeout 600s -p 1
# Runs unit + integration + e2e if env vars present
# Skip integration/e2e if SAGE_DATABASE_URL not set
```

---

## Definition of Done

### P0 (must pass before any optimizer code ships)
- [ ] Column validation: 7 unit tests, 1 integration
- [ ] Duplicate detection: 6 unit tests, 1 integration
- [ ] Cold start: 4 unit tests
- [ ] indisvalid check: 3 integration tests
- [ ] Write impact: 5 unit tests

### P1 (must pass before v2 release)
- [ ] HypoPG: 2 unit, 12 integration tests
- [ ] Plan-aware: 8 unit, 3 integration tests
- [ ] INCLUDE: 5 unit tests
- [ ] Partial index: 5 unit, 1 integration
- [ ] Dual-model: 8 unit, 2 integration
- [ ] Confidence: 5 unit tests
- [ ] E2E: 10 end-to-end tests passing

### P2 (must pass before broad coverage release)
- [ ] Non-B-tree: 7 unit, 2 integration
- [ ] Expression index: 4 unit
- [ ] Collation: 4 unit, 1 integration
- [ ] Extension validation: 5 unit
- [ ] Cost estimation: 5 unit
- [ ] Fingerprinting: 5 unit
- [ ] Cross-table: 4 unit
- [ ] Workload classification: 4 unit

### P3 (must pass before advanced features release)
- [ ] Selectivity: 3 unit
- [ ] Decay tracking: 4 unit
- [ ] Enhanced regression: 4 unit
- [ ] Materialized view: 3 unit
- [ ] Parameter tuning: 3 unit
- [ ] Reindex: 3 unit + 1 integration
- [ ] Per-table circuit breaker: 4 unit

### Total: ~175 tests (130 unit + 30 integration + 12 e2e)

All tests are Go code in `sidecar/internal/optimizer/`. No C extension tests — the optimizer is sidecar-only.

---

## Plan Capture: Extension Co-Deployment Bonus

When the C extension is co-deployed, the optimizer reads from `sage.explain_cache` for richer plan data. This needs 3 integration tests:

```go
func TestPlanCapture_ExplainCacheAvailable(t *testing.T)
    // sage.explain_cache table exists and has plans
    // Assert: optimizer reads plans from cache
    // Assert: plans include actual row counts (not just estimates)

func TestPlanCapture_ExplainCacheMissing(t *testing.T)
    // sage.explain_cache table does NOT exist (standalone sidecar, no extension)
    // Assert: falls back to GENERIC_PLAN (PG16+) or query-text-only
    // Assert: no error, no crash

func TestPlanCapture_ExplainCacheStale(t *testing.T)
    // sage.explain_cache has plan from 48h ago, table schema changed since
    // Assert: plan used but confidence scored as "stale"
```

These run in the standard Go test suite. No C test framework needed.
