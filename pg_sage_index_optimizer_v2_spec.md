# pg_sage Index Optimizer v2 — Final Spec

## Overview

The Index Optimizer is the LLM-powered feature that differentiates pg_sage from every rules-based PG monitoring tool. It takes Tier 1 findings, enriches them with execution plans, table statistics, and write rates, sends them to a reasoning-tier LLM, validates the output with HypoPG hypothetical indexes, and produces DDL that the executor acts on autonomously.

**v1 status:** Working. 5 LLM-recommended indexes created on Cloud SQL (R11), confirmed on AlloyDB (R13). Flash model truncates complex responses. No plan data. No pre-execution validation. No non-B-tree index support.

**v2 goal:** Better recommendations (plan-aware, partial-index-aware, multi-index-type), bullet-proof validation (HypoPG, column checks, cost estimation), dual-model architecture (Flash for general, Opus/Pro for optimization), and broader recommendation scope (materialized views, parameter tuning, expression indexes).

**Platform: Sidecar-only.** The C extension is frozen at rc3. All optimizer v2 features are Go code in `internal/optimizer/`.

---

## Part 1: Better Inputs (What the Optimizer Sees)

### 1.1 Plan-Aware Optimization

The single biggest quality improvement. Currently the optimizer sends query text to Gemini and Gemini guesses the plan. With EXPLAIN data, it knows.

**Plan capture strategy (priority order):**
1. `sage.explain_cache` — if the C extension is co-deployed, read actual EXPLAIN ANALYZE plans (richest data: real row counts, real buffer hits, real heap fetches)
2. `EXPLAIN (GENERIC_PLAN, FORMAT JSON)` — PG16+, works on parameterized queries, estimated plans (good enough for 90% of decisions)
3. Query-text-only — PG14-15 without extension, current v1 behavior (LLM guesses the plan)

**Plan summarization** (keep under 200 bytes per query for context budget):
```
Query: SELECT * FROM orders WHERE customer_id = $1
Stats: 500 calls/day, 2000ms mean
Plan: Seq Scan on orders → Filter: customer_id=$1 → Rows Removed: 499,999
Bottleneck: No index on customer_id. Full table scan.
```

**What plans unlock:**
- Seq Scan confirmed (not guessed) → recommend B-tree
- Index Scan with Heap Fetches: 487,000 → recommend INCLUDE columns
- Sort Method: external merge, Disk: 48MB → recommend work_mem, NOT index
- Nested Loop → identify which side is slow → index only that side
- Index exists but planner chose Seq Scan → low selectivity → partial index or skip

### 1.2 Selectivity Data from pg_stats

```sql
SELECT attname, n_distinct, most_common_vals, most_common_freqs, correlation
FROM pg_stats WHERE tablename = $1 AND schemaname = $2;
```

Feed to LLM: "status has 5 distinct values, 'active' is 5% of rows. A partial index WHERE status='active' would be 20x smaller than a full index."

Also: `correlation` close to 1.0 means data is physically ordered → BRIN might be more efficient than B-tree for range scans.

### 1.3 Cross-Table Join Context

Before sending individual tables to the LLM, identify join patterns:
```sql
SELECT queryid, query FROM pg_stat_statements
WHERE query ~* 'JOIN.*ON' AND calls > 10;
```

Group by table pairs. Send BOTH tables in the same prompt with the join pattern so the LLM can recommend indexes on both sides.

### 1.4 Workload Classification

Classify each table before sending to the LLM:
- Write ratio > 70% → OLTP-write-heavy (minimal indexing)
- Write ratio < 10%, avg scan > 100K rows → OLAP (covering indexes, BRIN)
- Mix → HTAP (balanced approach, explain tradeoffs)

Include in prompt: "This is an OLTP-write-heavy table (85% writes). Every new index costs significant write amplification."

### 1.5 Index Usage Trends

Track `idx_scan` over time from sage.snapshots. Compute decay:
```
idx_orders_old: 10K scans/day (30d ago) → 200 scans/day (7d ago) → 50/day (today)
Trend: -95% over 30 days → DECAYING → recommend proactive drop
```

Tier 1 rule (no LLM needed), but LLM can enhance with: "This index likely became unused after the application migration on March 15."


---

## Part 2: Better Recommendations (What the Optimizer Produces)

### 2.1 Partial Index Detection

If >80% of queries on a column use the same constant filter AND that value matches <20% of rows (from pg_stats), recommend a partial index.

```
Query pattern: WHERE customer_id=$1 AND status='active' (80% of traffic)
pg_stats: status='active' frequency = 0.05 (5% of rows)

Recommendation: CREATE INDEX CONCURRENTLY ON orders (customer_id) WHERE status = 'active'
Benefit: 20x smaller than full index, covers 80% of queries
```

### 2.2 INCLUDE Column Intelligence (Plan-Driven)

Only recommend INCLUDE when EXPLAIN shows Heap Fetches > threshold:
- Index Scan with Heap Fetches > 1000/day → INCLUDE the columns causing fetches
- Index Only Scan already → don't touch
- Seq Scan → create base index first; INCLUDE is a second-pass optimization

### 2.3 Non-B-tree Index Types

v1 only recommends B-tree. v2 should detect when other types are appropriate:

| Pattern | Recommended Type | Detection |
|---------|-----------------|-----------|
| `WHERE col @> '{"key":"val"}'` | GIN (jsonb_ops) | JSONB containment operator in query |
| `WHERE col ? 'key'` | GIN (jsonb_path_ops) | JSONB existence operator |
| `WHERE to_tsvector(col) @@ query` | GIN (tsvector) | Full-text search in query |
| `WHERE col && ARRAY[1,2,3]` | GIN (array_ops) | Array overlap operator |
| `WHERE col LIKE '%term%'` | GIN (pg_trgm) | LIKE with leading wildcard |
| `WHERE col LIKE 'prefix%'` (non-C locale) | B-tree (text_pattern_ops) | LIKE prefix + non-C collation |
| Range scan on time-ordered column (correlation > 0.9) | BRIN | Physical correlation from pg_stats |
| `WHERE ST_DWithin(geom, point, radius)` | GiST | PostGIS spatial operators |

The LLM prompt should include the database collation (`SHOW lc_collate`) and the operators used in each query. The validator must reject non-B-tree recommendations if the required extension isn't installed (e.g., pg_trgm for trigram GIN).

### 2.4 Expression Index Detection

```sql
-- This query can't use a B-tree on order_date:
SELECT * FROM orders WHERE EXTRACT(YEAR FROM order_date) = 2024;

-- Needs an expression index:
CREATE INDEX CONCURRENTLY ON orders (EXTRACT(YEAR FROM order_date));
```

Detection: if the plan shows `Filter: (extract(year from order_date) = 2024)` instead of an Index Cond on order_date, the column is wrapped in a function. The optimizer should recommend an expression index.

**Risk:** Expression indexes break if the function is not IMMUTABLE. The validator must check function volatility.

### 2.5 Cost Estimation

Quantify both sides of every recommendation:

```
Recommendation: CREATE INDEX CONCURRENTLY ON orders (customer_id) INCLUDE (order_date, total_amount)

Cost:
  Estimated size: ~45MB (500K rows × ~90 bytes/entry)
  Write amplification: +3% on INSERT/UPDATE (~360 additional writes/day at 12K writes/day)
  Build time: ~15s (CONCURRENTLY, estimated from table size)

Benefit:
  Query 1: 2000ms → ~5ms (500 calls/day) → saves 997s/day
  Query 2: 1500ms → ~2ms (200 calls/day) → saves 300s/day
  Total: 1297 seconds/day saved (21.6 minutes)

ROI: 45MB disk + 3% write overhead → 21.6 min/day query savings
```

Compute deterministically from pg_relation_size, pg_stat_statements stats, and write rate deltas. No LLM needed for the math.

### 2.6 Materialized View Candidates

Detect aggregation queries that can't be fixed with indexes:

```sql
SELECT status, count(*), avg(total_amount) FROM orders GROUP BY status
-- 200 calls/day, 3000ms, scans all 500K rows every time
-- No index helps — it reads ALL rows regardless
```

Recommend `CREATE MATERIALIZED VIEW` + `REFRESH CONCURRENTLY` schedule. Flag as `category: materialized_view_candidate`, severity `info` (advisory only — never auto-execute, changes application semantics).

### 2.7 Parameter Tuning Detection

With plan data, detect non-index bottlenecks:

| Plan Signal | Recommendation |
|-------------|---------------|
| Sort: external merge, Disk > 10MB | Increase work_mem |
| Hash Batches > 1 | Increase work_mem |
| Buffers: shared read >> shared hit | Increase shared_buffers or effective_cache_size |
| Seq Scan chosen over existing index | Reduce random_page_cost (SSD storage) |
| Nested Loop with high inner rows | Consider SET enable_nestloop = off for this query |

Flag as `category: parameter_tuning`, severity `info`. Never auto-execute global parameter changes.

### 2.8 Reindex Detection

Indexes bloat over time (B-tree page splits, dead tuples in index). The current spec handles table bloat but not index bloat.

```sql
-- Detect index bloat (ratio of actual size to estimated minimum size):
SELECT indexrelname, pg_relation_size(indexrelid) AS actual_size,
       pg_relation_size(indrelid) * 0.4 AS estimated_min  -- heuristic
FROM pg_stat_user_indexes sui
JOIN pg_index USING (indexrelid)
WHERE pg_relation_size(indexrelid) > pg_relation_size(indrelid) * 0.8;
-- If actual_size > 2x estimated_min → recommend REINDEX CONCURRENTLY
```

Flag as `category: index_bloat`, recommend `REINDEX CONCURRENTLY`. This IS safe for Tier 3 auto-execution (CONCURRENTLY doesn't block).


---

## Part 3: HypoPG Validation (The Critical Missing Piece)

**This is what PlanetScale does and what we don't.** Before creating any real index, create a hypothetical one with HypoPG, run EXPLAIN on the target queries, and verify the planner would actually use it. This eliminates hallucinated indexes, useless indexes (planner ignores them due to selectivity), and indexes the planner uses but with negligible improvement.

### 3.1 The Validation Pipeline

```
LLM recommends index
       ↓
Column existence check (2.1 from bullet-proofing)
       ↓
Duplicate check (2.2 from bullet-proofing)
       ↓
HypoPG: CREATE hypothetical index
       ↓
For each affected query:
  EXPLAIN without hypothetical → cost_before
  EXPLAIN with hypothetical → cost_after
       ↓
improvement_pct = (cost_before - cost_after) / cost_before * 100
       ↓
IF improvement_pct < 10%: REJECT (planner won't meaningfully use it)
IF improvement_pct >= 10%: ACCEPT
       ↓
hypopg_reset() (clean up)
       ↓
Accepted recommendations → executor
```

### 3.2 Implementation

```go
func (o *Optimizer) validateWithHypoPG(ctx context.Context, pool *pgxpool.Pool, rec Recommendation) (float64, error) {
    conn, err := pool.Acquire(ctx)
    if err != nil {
        return 0, err
    }
    defer conn.Release()

    // Create hypothetical index
    var indexOID int64
    err = conn.QueryRow(ctx,
        "SELECT indexrelid FROM hypopg_create_index($1)", rec.DDL).Scan(&indexOID)
    if err != nil {
        // HypoPG not installed or DDL is invalid
        return 0, fmt.Errorf("hypopg_create_index failed: %w", err)
    }
    defer conn.Exec(ctx, "SELECT hypopg_reset()")

    // Estimate improvement for each affected query
    var totalImprovement float64
    for _, queryText := range rec.AffectedQueries {
        // Cost WITHOUT hypothetical index (hypopg_reset first, then re-create)
        // Actually: HypoPG indexes are used by EXPLAIN automatically
        // So we need: EXPLAIN before create, EXPLAIN after create

        // Get cost WITH hypothetical index (it's already created)
        var planJSON []byte
        err = conn.QueryRow(ctx,
            "EXPLAIN (FORMAT JSON) "+queryText).Scan(&planJSON)
        if err != nil {
            continue
        }
        costWith := extractTotalCost(planJSON)

        // Get cost WITHOUT: reset, EXPLAIN, re-create
        conn.Exec(ctx, "SELECT hypopg_reset()")
        err = conn.QueryRow(ctx,
            "EXPLAIN (FORMAT JSON) "+queryText).Scan(&planJSON)
        if err != nil {
            continue
        }
        costWithout := extractTotalCost(planJSON)

        // Re-create for remaining queries
        conn.QueryRow(ctx,
            "SELECT indexrelid FROM hypopg_create_index($1)", rec.DDL).Scan(&indexOID)

        if costWithout > 0 {
            improvement := (costWithout - costWith) / costWithout * 100
            totalImprovement += improvement
        }
    }

    avgImprovement := totalImprovement / float64(len(rec.AffectedQueries))
    return avgImprovement, nil
}
```

### 3.3 Estimated Size from HypoPG

HypoPG can estimate the size of a hypothetical index without creating it:

```sql
SELECT pg_size_pretty(hypopg_relation_size(indexrelid))
FROM hypopg_list_indexes
WHERE index_name = '<hypothetical_index_name>';
```

Use this for cost estimation instead of guessing from row count × estimated bytes.

### 3.4 HypoPG Availability

HypoPG is an extension — it may not be installed. The optimizer must handle this gracefully:

```go
func (o *Optimizer) hasHypoPG(ctx context.Context, pool *pgxpool.Pool) bool {
    var exists bool
    pool.QueryRow(ctx,
        "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'hypopg')").Scan(&exists)
    return exists
}
```

**If HypoPG is available:** Full validation pipeline (create hypothetical → EXPLAIN → compare costs → accept/reject).

**If HypoPG is NOT available:** Fall back to current behavior (column check, duplicate check, write impact check — no plan-based validation). Log a recommendation: "Install HypoPG for more accurate index recommendations."

**HypoPG availability by platform:**
- Self-managed: Install from PGDG repo or compile from source
- Cloud SQL: Available via `CREATE EXTENSION hypopg`
- AlloyDB: Available via `CREATE EXTENSION hypopg`
- Aurora/RDS: Available via `CREATE EXTENSION hypopg` (in `shared_preload_libraries` since RDS supports it)
- Azure Flexible Server: Available

### 3.5 Minimum Improvement Threshold

Configurable, default 10%:

```yaml
optimizer_llm:
  hypopg_min_improvement_pct: 10  # reject if EXPLAIN cost improves < 10%
```

Recommendations below this threshold are logged as `severity: info` (advisory) but not sent to the executor.


---

## Part 4: Bullet-Proofing

### 4.1 Column Existence Validation

Before submitting any LLM recommendation to the executor (or HypoPG), verify every referenced column exists in the target table. Catches the most common LLM hallucination.

```go
func validateColumns(rec Recommendation, tableDDL TableDDL) error {
    columns := extractColumnsFromDDL(rec.DDL)
    for _, col := range columns {
        if !tableDDL.HasColumn(col) {
            return fmt.Errorf("hallucinated column %q on table %s", col, tableDDL.Name)
        }
    }
    for _, col := range extractIncludeColumns(rec.DDL) {
        if !tableDDL.HasColumn(col) {
            return fmt.Errorf("hallucinated INCLUDE column %q", col)
        }
    }
    return nil
}
```

### 4.2 Duplicate Recommendation Detection

Before executing CREATE INDEX, check if an equivalent index already exists (same columns, possibly different name).

```sql
-- Find indexes with identical column set on the same table:
SELECT indexname, indexdef FROM pg_indexes
WHERE tablename = $1 AND schemaname = $2;
-- Parse indexdef to extract column list, compare with recommendation
```

Must handle: same columns different order (for non-leading), same columns with/without INCLUDE, partial indexes (WHERE clause comparison).

### 4.3 Cold Start Protection

Never send a table to the LLM until 2+ collector snapshots exist (so write rate can be computed from deltas). First cycle = zero optimizer recommendations. This is correct — you shouldn't auto-create indexes based on 60 seconds of observation.

### 4.4 Write Impact Pre-Check

Estimate write amplification BEFORE executing. If impact exceeds configurable threshold (default 15%), downgrade to advisory (don't auto-execute).

```go
type WriteImpact struct {
    AdditionalWritesPerDay int64
    PctIncrease           float64
    Acceptable            bool  // true if < threshold
}
```

### 4.5 Post-Execution Validation

After CREATE INDEX CONCURRENTLY completes, verify the index is valid:

```sql
SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass;
-- MUST be true. CONCURRENTLY can leave INVALID indexes on failure.
```

If `indisvalid = false`: log error, do NOT proceed to drop the old index (if this was an INCLUDE upgrade), and flag for manual review. Add to findings as `category: invalid_index`, severity `critical`.

### 4.6 Enhanced Regression Detection

Monitor multiple signals post-execution, not just query latency:

- Query latency (mean_exec_time delta) — existing
- INSERT latency on affected table — catches write amplification
- WAL bytes/sec delta — catches I/O increase
- Checkpoint frequency delta — catches system-wide impact

If any signal regresses > threshold, trigger rollback.

### 4.7 Confidence Scoring

Score each recommendation before the executor sees it:

```go
type ConfidenceScore struct {
    QueryVolume     float64  // 0-1: more calls = higher confidence
    PlanDataAvail   float64  // 1.0 if EXPLAIN available, 0.5 if query-text-only
    WriteRateKnown  float64  // 1.0 if 2+ snapshots, 0.0 if cold start
    HypoPGValidated float64  // 1.0 if HypoPG confirmed improvement, 0.5 if not available
    SelectivityKnown float64 // 1.0 if pg_stats data available
    Overall         float64  // weighted average
}

func (c ConfidenceScore) ActionLevel() string {
    if c.Overall > 0.8 { return "autonomous" }
    if c.Overall > 0.5 { return "advisory" }
    return "informational"
}
```

High confidence → Tier 3 auto-execute. Medium → show in findings, don't execute. Low → informational only.

### 4.8 Query Fingerprinting (ORM Dedup)

ORMs generate many query strings for the same logical pattern. pg_stat_statements normalizes parameters to $1/$2 but can't merge different IN-list lengths or column subsets.

Secondary fingerprint: collapse IN-lists, normalize literals, group by fingerprint, send ONE representative query to LLM with aggregated stats.

### 4.9 Per-Table Circuit Breaker

If the LLM gives 3 consecutive bad recommendations for a specific table (fail validation or cause regression), stop sending that table to the LLM. Cool down 24 hours, then half-open. If it fails again, cool down 7 days.

### 4.10 Extension/Operator Class Validation

For non-B-tree recommendations:
- GIN (pg_trgm) → verify `pg_trgm` extension installed
- GIN (jsonb_path_ops) → verify column is actually JSONB type
- GiST (PostGIS) → verify PostGIS installed
- BRIN → verify correlation > 0.8 in pg_stats (otherwise BRIN is useless)
- Expression index → verify function is IMMUTABLE

```go
func validateIndexType(rec Recommendation, pool *pgxpool.Pool) error {
    if rec.IndexType == "gin" && rec.OpClass == "gin_trgm_ops" {
        if !extensionInstalled(pool, "pg_trgm") {
            return fmt.Errorf("pg_trgm extension required for trigram GIN index")
        }
    }
    if rec.IndexType == "brin" {
        corr := getCorrelation(pool, rec.Table, rec.Columns[0])
        if corr < 0.8 {
            return fmt.Errorf("BRIN ineffective: correlation %.2f < 0.8 for %s.%s", corr, rec.Table, rec.Columns[0])
        }
    }
    return nil
}
```

### 4.11 Collation Awareness

B-tree indexes on text columns with non-C locale don't support `LIKE 'prefix%'` queries unless created with `text_pattern_ops` or `varchar_pattern_ops`. The optimizer must:

1. Check `SHOW lc_collate` on the database
2. If non-C and the query uses LIKE prefix: recommend `varchar_pattern_ops` operator class
3. Include collation context in the LLM prompt

This is a real-world gotcha that catches experienced DBAs — Cubbit's engineering team recently published about losing hours to this exact issue.


---

## Part 5: Dual-Model Architecture

### 5.1 Why Two Models

| Task | Volume | Stakes | Right Model |
|------|--------|--------|-------------|
| Briefing summary | 24/day | Low (text for humans) | Flash/Haiku |
| Explain narration | 50-200/day | Low (read-only) | Flash/Haiku |
| diagnose() ReAct | On-demand | Low (read-only SQL) | Flash/Haiku |
| **Index optimization** | **10-50/day** | **High (produces DDL that modifies the database)** | **Opus/Pro** |

Index optimization is the ONLY LLM task that produces DDL the executor acts on autonomously. The quality bar for "DDL that runs unattended at 3am" is higher than "text a DBA reads."

R11 proved this: Gemini Flash truncated index optimization responses (P-5). A reasoning-tier model completes the response.

### 5.2 Configuration

```yaml
llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 60
  token_budget_daily: 100000
  max_output_tokens: 2048

optimizer_llm:
  enabled: true
  endpoint: "https://api.anthropic.com/v1"
  model: "claude-opus-4-6"
  api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}
  timeout_seconds: 120
  token_budget_daily: 50000
  max_output_tokens: 4096
  fallback_to_general: true
  hypopg_min_improvement_pct: 10
```

CLI overrides:
```bash
./pg_sage --database-url "postgres://..." \
    --llm-model gemini-2.5-flash \
    --llm-api-key "AIza..." \
    --optimizer-llm-model claude-opus-4-6 \
    --optimizer-llm-api-key "sk-..."
```

**Recommended models:**

| Provider | Optimizer | General | Why |
|----------|-----------|---------|-----|
| Anthropic | claude-opus-4-6 | claude-haiku-4-5 | Best reasoning for structured DDL output |
| Google | gemini-2.5-pro | gemini-2.5-flash | Pro for deep analysis, Flash for speed |
| xAI | grok-3 | grok-3-mini | Expert reasoning tier |
| OpenAI | o3 | gpt-4.1-mini | Reasoning model for complex analysis |

### 5.3 Fallback Chain

```
1. optimizer_llm configured + budget available → use optimizer model
2. optimizer_llm budget exhausted + fallback_to_general → use general model
3. optimizer_llm not configured → use general model (v1 behavior, zero config change)
4. all budgets exhausted → Tier 1 findings only
```

### 5.4 Separate Budgets, Circuit Breakers, Prometheus

Each model gets independent: token budget, circuit breaker, and Prometheus metrics.

```
pg_sage_llm_calls_total{model="gemini-2.5-flash",purpose="briefing"}        847
pg_sage_llm_calls_total{model="claude-opus-4-6",purpose="index_optimization"}  23
pg_sage_llm_tokens_used_today{model="gemini-2.5-flash"}                    47,230
pg_sage_llm_tokens_used_today{model="claude-opus-4-6"}                      8,400
pg_sage_llm_latency_seconds{model="claude-opus-4-6",purpose="index_optimization"} 8.3
```

### 5.5 Cost Estimation

| Scenario (per day) | General (Flash) | Optimizer (Opus) | Total |
|---------------------|----------------|------------------|-------|
| Small DB (10 tables) | $0.20 | $0.50 | **$0.70** |
| Medium DB (50 tables) | $1.00 | $2.50 | **$3.50** |
| Large DB (100 tables) | $2.00 | $5.00 | **$7.00** |

One correct composite index recommendation on a 50-table DB saves 20+ minutes/day of query time. The optimizer pays for itself immediately.

---

## Part 6: The Optimizer Prompt (v2)

### 6.1 Context Assembly

For each table with findings, assemble this context packet:

```
TABLE: public.orders
  Rows: 500,000
  Size: 65MB (data) + 18MB (indexes) + 12MB (toast)
  Classification: HTAP (35% writes, 65% reads)
  Collation: en_US.UTF-8 (non-C — LIKE prefix requires pattern_ops)
  
  Columns:
    order_id SERIAL PRIMARY KEY
    customer_id INT NOT NULL (FK → customers, NO INDEX)
    product_id INT NOT NULL (FK → products, NO INDEX)
    total_amount NUMERIC(12,2) NOT NULL
    order_date TIMESTAMPTZ NOT NULL
    status VARCHAR(20) (5 distinct values, 'active'=5%, correlation=0.02)
    notes TEXT
    
  Existing indexes (3):
    orders_pkey (order_id) — 500K scans/day, 0 heap fetches
    idx_orders_status (status) — 0 scans/day [UNUSED, low selectivity]
    
  Write rate: 12,000 writes/day (10K INSERT, 2K UPDATE)
  
  Top queries hitting this table (from pg_stat_statements):
  
  Q1: SELECT * FROM orders WHERE customer_id = $1
    Stats: 500 calls/day, 2000ms mean
    Plan: Seq Scan → Filter: customer_id=$1 → Rows Removed: 499,999
    Bottleneck: No index on customer_id
    
  Q2: SELECT order_id, total_amount FROM orders WHERE customer_id=$1 ORDER BY order_date DESC LIMIT 10
    Stats: 200 calls/day, 1500ms mean
    Plan: Index Scan on (none) → Heap Fetches: 487,000 → Sort: external merge 12MB
    Bottleneck: No covering index. Heap fetches + disk sort.
    
  Q3: SELECT status, count(*), avg(total_amount) FROM orders GROUP BY status
    Stats: 200 calls/day, 3000ms mean
    Plan: Seq Scan → GroupAggregate (500K rows)
    Bottleneck: Full table scan for aggregation. No index can help — consider materialized view.
    
  Q4: SELECT * FROM orders o JOIN customers c ON o.customer_id = c.customer_id WHERE c.name = $1
    Stats: 100 calls/day, 4000ms mean
    Plan: Nested Loop → Seq Scan on orders (inner) → Index Scan on customers (outer)
    Bottleneck: orders side of join — needs index on customer_id
```

### 6.2 The Prompt

```
You are a PostgreSQL indexing expert. Given the table context below, recommend the optimal set of indexes.

RULES:
1. Every CREATE INDEX must use CONCURRENTLY and schema-qualified names.
2. Max 3 new indexes per table.
3. Never recommend more indexes than the table has columns.
4. Consider write amplification: this table has {write_rate} writes/day.
5. Consolidate where possible: prefer one composite index over two single-column indexes.
6. Use INCLUDE columns when the plan shows Heap Fetches and specific columns can eliminate them.
7. Use partial indexes (WHERE clause) when >80% of queries filter on the same constant value.
8. For LIKE prefix queries on non-C locale databases, use varchar_pattern_ops or text_pattern_ops.
9. For JSONB containment queries, recommend GIN with appropriate operator class.
10. If a query bottleneck is sort-spill or hash-batching, recommend work_mem increase, NOT an index.
11. If a query scans all rows for aggregation, recommend a materialized view, NOT an index.
12. For each recommendation, explain what plan change it enables (Seq Scan → Index Scan, Index Scan → Index Only Scan, etc.).

Respond with ONLY a JSON array. No markdown, no explanation outside the JSON. Each element:
{
  "ddl": "CREATE INDEX CONCURRENTLY ...",
  "drop_ddl": "DROP INDEX CONCURRENTLY ... (or null if not replacing)",
  "rationale": "Converts Q1 from Seq Scan (2000ms) to Index Scan (~5ms). Covers Q4 join condition.",
  "affected_queries": ["Q1", "Q2", "Q4"],
  "estimated_improvement_pct": 95,
  "index_type": "btree",
  "confidence": "high",
  "category": "index_optimization | partial_index | include_upgrade | expression_index | materialized_view_candidate | parameter_tuning"
}

TABLE CONTEXT:
{context_packet}
```

---

## Part 7: Implementation Priority

### P0 — Prevents real damage (ship in patch release)
1. **Column existence validation** (4.1)
2. **Duplicate recommendation detection** (4.2)
3. **Cold start protection** (4.3)
4. **Post-execution indisvalid check** (4.5)
5. **Write impact pre-check** (4.4)

### P1 — Major quality upgrade (next minor version)
6. **HypoPG validation** (3.1-3.5) — the single biggest quality improvement
7. **Plan-aware optimization** (1.1) — EXPLAIN integration
8. **INCLUDE column intelligence** (2.2) — plan-driven heap fetch detection
9. **Partial index detection** (2.1) — 20x smaller indexes
10. **Dual-model architecture** (5.1-5.5) — Opus/Pro for optimization
11. **Confidence scoring** (4.7)

### P2 — Broader coverage (following minor version)
12. **Non-B-tree index types** (2.3) — GIN, GiST, BRIN
13. **Expression index detection** (2.4)
14. **Collation awareness** (4.11)
15. **Extension/operator class validation** (4.10)
16. **Cost estimation** (2.5)
17. **Query fingerprinting** (4.8)
18. **Cross-table join optimization** (1.3)
19. **Workload classification** (1.4)

### P3 — Advanced features (future)
20. **Selectivity data from pg_stats** (1.2)
21. **Index usage decay tracking** (1.5)
22. **Enhanced regression detection** (4.6)
23. **Materialized view detection** (2.6)
24. **Parameter tuning detection** (2.7)
25. **Reindex detection** (2.8)
26. **Per-table circuit breaker** (4.9)

---

## Appendix: Research References

- **PlanetScale Insights** — LLM + HypoPG validation pipeline. The closest commercial implementation to what we're building. They use HypoPG to verify planner would use the recommended index before suggesting it to users. Key insight: "validate LLM generated solutions before shipping them to production."

- **LLMIdxAdvis (2025 paper)** — Academic research on LLM-based index recommendation. Uses HypoPG "what-if caller" for cost estimation. Tested on TPC-H, JOB, and TPC-DS benchmarks. Achieved competitive results with reduced runtime vs traditional approaches.

- **Percona: pg_qualstats + HypoPG** — Extension-based automatic index recommendation. pg_qualstats captures WHERE clause statistics (more granular than pg_stat_statements). HypoPG validates. Automation loop: capture quals → recommend → validate → implement.

- **pganalyze Index Advisor** — Commercial cluster-aware index recommendations. Analyzes workloads across primaries and replicas. Key feature: considers the combined effect of multiple indexes, not just individual recommendations.

- **AlloyDB Index Advisor** — Google's built-in advisor. Uses EXPLAIN internally. Recommends indexes based on captured workload.

- **Cloud SQL Index Advisor** — Google's managed advisor for Cloud SQL. Tracks queries periodically and recommends new indexes.

- **Cubbit Engineering (2026)** — Real-world case study: 12 indexing pitfalls in PostgreSQL. Key lesson: collation (en_US.UTF-8) breaks LIKE prefix index usage. Requires varchar_pattern_ops.

- **pganalyze: GIN Indexes** — Deep dive on GIN write overhead. GitLab case study: GIN trigram index caused slow merge requests due to gin_pending_list_limit flushes. Key lesson: GIN indexes have bursty write overhead.
