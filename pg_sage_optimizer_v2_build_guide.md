# pg_sage Index Optimizer v2 — Build Guide

## Purpose

This document bridges the v2 spec (26 features) with the existing codebase (1,309 lines, 90 tests). Read this BEFORE the spec. It tells you what exists, what to add, what to modify, and how things wire together.

---

## 1. Current Code Map

### Source Files (10 files, 1,309 lines)

| File | Lines | What It Does | Status |
|------|-------|-------------|--------|
| `types.go` | 90 | Recommendation, TableContext, ConfidenceScore, WriteImpact, HypoPGResult structs | **KEEP** — extend with new fields |
| `validate.go` | 175 | checkConcurrently, checkColumnExistence, checkDuplicate, checkWriteImpact, checkMaxIndexes | **KEEP** — add new validators |
| `confidence.go` | 63 | ComputeConfidence, ActionLevel | **KEEP** — add HypoPG factor |
| `prompt.go` | 153 | FormatPrompt, parseRecommendations, stripMarkdownFences | **MODIFY** — expand prompt template |
| `context_builder.go` | 260 | BuildTableContexts, fetchCollation, classifyWorkload, computeWriteRate | **MODIFY** — add plan data, selectivity |
| `plancapture.go` | 180 | summarizePlan, CapturePlans (explain_cache + GENERIC_PLAN) | **MODIFY** — wire into context builder |
| `hypopg.go` | 156 | extractTotalCost, isExplainable, IsAvailable, Validate, EstimateSize | **MODIFY** — complete the validation pipeline |
| `optimizer.go` | 179 | Analyze (orchestrator), analyzeTable, enrichWithHypoPG | **MODIFY** — add dual-model routing |
| `coldstart.go` | 24 | CheckColdStart | **KEEP** — no changes needed |
| `postcheck.go` | 29 | CheckIndexValid | **KEEP** — no changes needed |

### Test Files (7 files, 1,334 lines, 90 test functions)

All in `sidecar/internal/optimizer/`. All passing. DO NOT break these.

### What Has Full Unit Coverage (DO NOT REWRITE)
- `checkConcurrently` (5 tests)
- `checkColumnExistence` (6 tests)
- `checkDuplicate` (5 tests)
- `checkWriteImpact` (5 tests)
- `checkMaxIndexes` (4 tests)
- `ComputeConfidence` + `ActionLevel` (16 tests)
- `parseRecommendations` + `stripMarkdownFences` (15 tests)
- `classifyWorkload` + `computeWriteRate` + `extractTablesFromQuery` (16 tests)
- `summarizePlan` (8 scenarios)
- `extractTotalCost` + `isExplainable` (6 tests)

### What Has Zero Coverage (DB-dependent, needs integration tests)
- `optimizer.go`: Analyze, analyzeTable, enrichWithHypoPG, scoreConfidence
- `context_builder.go`: BuildTableContexts, fetchCollation, groupQueriesByTable, fetchColumns, fetchColStats
- `plancapture.go`: CapturePlans, fromExplainCache, fromGenericPlan
- `hypopg.go`: IsAvailable, Validate, EstimateSize, measureCosts
- `coldstart.go`: CheckColdStart
- `postcheck.go`: CheckIndexValid

---

## 2. How the Optimizer Hooks Into the Sidecar

### Call Chain

```
main.go
  └→ sidecar.Run()
       ├→ collector.Run() [goroutine, every 60s]
       │   └→ snapshots written to sage.snapshots
       ├→ analyzer.Run() [goroutine, every 120-300s]
       │   ├→ Tier 1 rules (missing FK, duplicates, bloat, etc.)
       │   ├→ optimizer.Analyze(ctx, pool, snapshots, config) ← THIS IS THE HOOK
       │   │   ├→ context_builder.BuildTableContexts()
       │   │   ├→ plancapture.CapturePlans()
       │   │   ├→ LLM call (FormatPrompt → Chat → parseRecommendations)
       │   │   ├→ validate.Validate()
       │   │   ├→ hypopg.Validate() (if available)
       │   │   ├→ confidence.ComputeConfidence()
       │   │   └→ Write findings to sage.findings
       │   └→ findings written to sage.findings
       ├→ executor.Run() [goroutine, every 120-300s]
       │   ├→ Read actionable findings from sage.findings
       │   ├→ Trust gate check (ShouldExecute)
       │   ├→ Execute DDL on raw pgx.Conn (CONCURRENTLY)
       │   └→ Write results to sage.action_log
       ├→ mcp.Serve() [HTTP server on :8080]
       └→ prometheus.Serve() [HTTP server on :9187]
```

### Where Optimizer Findings Go

```go
// In analyzer.Run(), after Tier 1 rules:
optimizerFindings, err := optimizer.Analyze(ctx, pool, latestSnapshots, cfg)
if err != nil {
    log.Printf("optimizer: %v", err)
    // Don't fail the whole cycle — Tier 1 findings still valid
}

// Merge optimizer findings into the findings table:
for _, f := range optimizerFindings {
    upsertFinding(ctx, pool, f)
}
```

### Finding Schema for Optimizer Output

```sql
INSERT INTO sage.findings (
    category,           -- 'index_optimization', 'partial_index', 'include_upgrade',
                        -- 'expression_index', 'materialized_view_candidate', 'parameter_tuning'
    object_identifier,  -- 'public.orders' (table the recommendation is for)
    severity,           -- 'warning' (actionable) or 'info' (advisory)
    status,             -- 'open'
    detail,             -- JSONB (see below)
    recommended_sql,    -- 'CREATE INDEX CONCURRENTLY ...'
    action_risk,        -- 'safe' or 'moderate' (from confidence score)
    first_seen,
    last_seen,
    occurrence_count
)
```

**detail JSONB structure:**
```json
{
    "llm_rationale": "Converts Q1 from Seq Scan (2000ms) to Index Scan (~5ms). Consolidates with Q4 join condition.",
    "affected_queries": ["SELECT * FROM orders WHERE customer_id = $1", "..."],
    "estimated_improvement_pct": 95,
    "index_type": "btree",
    "confidence": "high",
    "confidence_score": 0.87,
    "action_level": "autonomous",
    "hypopg_validated": true,
    "hypopg_improvement_pct": 92.3,
    "hypopg_estimated_size": "45 MB",
    "write_impact_pct": 3.2,
    "drop_ddl": null,
    "model_used": "claude-opus-4-6",
    "plan_data_available": true,
    "workload_class": "HTAP"
}
```

### How the Executor Handles Optimizer Findings

The executor already handles `category = 'index_optimization'` from v1. For v2, two additions:

**1. INCLUDE upgrades (DROP + CREATE):**
```go
// If detail.drop_ddl is not null, this is a replace operation:
// Step 1: Execute recommended_sql (CREATE new index with INCLUDE)
// Step 2: Verify indisvalid = true on new index
// Step 3: Only then execute detail.drop_ddl (DROP old index)
// If step 1 fails: don't do step 3
// If step 2 shows invalid: don't do step 3, log critical finding
```

**2. Advisory-only findings (materialized_view_candidate, parameter_tuning):**
```go
// If severity = 'info', skip execution entirely
// These are advisory — shown in MCP/briefing but never auto-executed
```

---

## 3. LLM Client Wiring

### Existing Client

```go
// In internal/llm/client.go:
type Client struct {
    endpoint    string
    model       string
    apiKey      string
    httpClient  *http.Client
    budget      *TokenBudget
    breaker     *CircuitBreaker
    // ...
}

func (c *Client) Chat(ctx context.Context, messages []Message) (string, int, error)
```

The optimizer currently uses the same `*llm.Client` as briefings and diagnose.

### Dual-Model Addition

```go
// In internal/llm/manager.go (NEW FILE):
type Manager struct {
    General   *Client   // Flash/Haiku — briefings, explain, diagnose
    Optimizer *Client   // Opus/Pro — index optimization (may be nil)
}

func NewManager(cfg config.LLMConfig) *Manager {
    general := NewClient(cfg.Endpoint, cfg.Model, cfg.APIKey, cfg.Timeout, cfg.TokenBudget)
    
    var optimizer *Client
    if cfg.OptimizerEndpoint != "" || cfg.OptimizerModel != "" {
        endpoint := cfg.OptimizerEndpoint
        if endpoint == "" { endpoint = cfg.Endpoint }
        model := cfg.OptimizerModel
        if model == "" { model = cfg.Model }
        apiKey := cfg.OptimizerAPIKey
        if apiKey == "" { apiKey = cfg.APIKey }
        optimizer = NewClient(endpoint, model, apiKey, cfg.OptimizerTimeout, cfg.OptimizerTokenBudget)
    }
    
    return &Manager{General: general, Optimizer: optimizer}
}

func (m *Manager) ChatForPurpose(ctx context.Context, purpose string, messages []Message) (string, int, error) {
    if purpose == "index_optimization" && m.Optimizer != nil {
        resp, tokens, err := m.Optimizer.Chat(ctx, messages)
        if err != nil && m.Config.OptimizerFallbackToGeneral {
            return m.General.Chat(ctx, messages)
        }
        return resp, tokens, err
    }
    return m.General.Chat(ctx, messages)
}
```

### Config Addition

```yaml
# Existing (no change):
llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 60
  token_budget_daily: 100000

# NEW:
optimizer_llm:
  enabled: true
  endpoint: "https://api.anthropic.com/v1"        # or "" to use general endpoint
  model: "claude-opus-4-6"                         # or "" to use general model
  api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}           # or "" to use general key
  timeout_seconds: 120
  token_budget_daily: 50000
  max_output_tokens: 4096
  fallback_to_general: true
  hypopg_min_improvement_pct: 10
```

### Prometheus Metrics Addition

```go
// Per-model, per-purpose labels:
llmCallsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
    Name: "pg_sage_llm_calls_total",
}, []string{"model", "purpose"})

llmTokensUsed := prometheus.NewGaugeVec(prometheus.GaugeOpts{
    Name: "pg_sage_llm_tokens_used_today",
}, []string{"model"})

llmLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
    Name: "pg_sage_llm_latency_seconds",
    Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60},
}, []string{"model", "purpose"})
```

---

## 4. Config Defaults

| Config Key | Default | Type | Notes |
|-----------|---------|------|-------|
| `optimizer_llm.enabled` | `false` | bool | Opt-in |
| `optimizer_llm.endpoint` | `""` | string | Falls back to general |
| `optimizer_llm.model` | `""` | string | Falls back to general |
| `optimizer_llm.api_key` | `""` | string | Falls back to general |
| `optimizer_llm.timeout_seconds` | `120` | int | Longer than general (60s) |
| `optimizer_llm.token_budget_daily` | `50000` | int | Separate from general |
| `optimizer_llm.max_output_tokens` | `4096` | int | Higher than general (2048) |
| `optimizer_llm.fallback_to_general` | `true` | bool | Use general if optimizer fails |
| `optimizer_llm.hypopg_min_improvement_pct` | `10` | float | Reject if < 10% improvement |
| `llm.index_optimizer.min_query_calls` | `100` | int | Min calls before optimizing |
| `llm.index_optimizer.max_indexes_per_table` | `3` | int | Anti-proliferation |
| `llm.index_optimizer.max_include_columns` | `5` | int | INCLUDE column limit |
| `llm.index_optimizer.over_indexed_ratio` | `1.0` | float | Skip if indexes >= columns × ratio |
| `llm.index_optimizer.write_heavy_ratio` | `0.7` | float | Skip if write ratio > 70% |
| `llm.index_optimizer.write_impact_threshold_pct` | `15` | float | Downgrade to advisory if > 15% |
| `llm.index_optimizer.cold_start_min_snapshots` | `2` | int | Need 2+ for write rate |

---

## 5. Error Handling Matrix

| Situation | Expected Behavior |
|-----------|------------------|
| LLM returns unparseable JSON | Log warning, skip this table, continue to next |
| LLM returns empty array `[]` | No recommendations for this table, continue |
| LLM returns hallucinated column | `checkColumnExistence` rejects, log, skip recommendation |
| LLM returns duplicate of existing index | `checkDuplicate` rejects, log, skip |
| LLM returns missing CONCURRENTLY | `checkConcurrently` rejects, log, skip |
| HypoPG not installed | Log info "Install HypoPG for better validation", skip HypoPG step, use lower confidence |
| HypoPG create fails (unsupported type) | Log warning, skip this recommendation, continue |
| HypoPG improvement < threshold | Downgrade to `severity: info` (advisory), don't execute |
| GENERIC_PLAN fails (PG14) | Fall back to query-text-only, lower confidence |
| explain_cache empty | Fall back to GENERIC_PLAN or query-text-only |
| Cold start (< 2 snapshots) | Skip entire optimizer cycle, log "waiting for 2+ snapshots" |
| Write impact > threshold | Downgrade to `severity: info` (advisory), don't execute |
| Optimizer LLM budget exhausted | Fall back to general LLM if `fallback_to_general: true`, else skip LLM |
| Both budgets exhausted | Tier 1 findings only, no LLM calls |
| Optimizer circuit open | Fall back to general LLM or Tier 1 |
| `CREATE INDEX CONCURRENTLY` leaves INVALID | Log critical finding, do NOT drop old index |
| Per-table circuit breaker (3 failures) | Stop optimizing that table for 24h |

---

## 6. Implementation Order

Follow the spec's priority tiers. Each tier is a PR or set of commits.

### PR 1: P0 Bullet-Proofing (add to existing validators)

Files to modify: `validate.go`, `validate_test.go`

No new files. Add validators that run BEFORE any LLM call or HypoPG validation:
- Cold start check is already in `coldstart.go` — wire into `Analyze()` if not already
- `indisvalid` check is already in `postcheck.go` — wire into executor after DDL
- Write impact already in `validate.go` — add configurable threshold

**Tests:** Should already pass (90 existing). Add any missing edge cases.

### PR 2: P1-6 HypoPG Validation Pipeline

Files to modify: `hypopg.go`, `hypopg_test.go`

The file exists with `extractTotalCost`, `isExplainable`, and stubs for `IsAvailable`, `Validate`, `EstimateSize`, `measureCosts`. Complete the DB-dependent functions.

**Key implementation:**
```go
func (h *HypoPG) Validate(ctx context.Context, conn *pgxpool.Conn, rec Recommendation, queries []string) (float64, error) {
    // 1. hypopg_create_index(rec.DDL)
    // 2. For each query: EXPLAIN without hypo → cost_before
    // 3. EXPLAIN with hypo → cost_after
    // 4. hypopg_reset()
    // 5. Return average improvement %
}
```

**Tests:** Integration tests (need live PG + HypoPG extension).

### PR 3: P1-7 Plan-Aware Optimization

Files to modify: `context_builder.go`, `plancapture.go`, `prompt.go`

`plancapture.go` already has `summarizePlan` (8 scenarios tested) and stubs for `CapturePlans`, `fromExplainCache`, `fromGenericPlan`. Complete the DB-dependent functions and wire plan summaries into the prompt context.

### PR 4: P1-10 Dual-Model Architecture

New file: `internal/llm/manager.go`
Modify: `internal/config/config.go` (add optimizer_llm section)
Modify: `optimizer.go` (use Manager instead of Client)

### PR 5: P2 Non-B-tree + Expression + Collation

Files to modify: `validate.go` (extension checks), `context_builder.go` (collation, correlation), `prompt.go` (expanded prompt)

New validators: BRIN correlation check, pg_trgm installed check, function immutability check.

### PR 6: P2 Cost Estimation + Fingerprinting + Cross-Table

New files: `cost.go`, `fingerprint.go`, `joins.go`

### PR 7: P3 Advanced (decay, matview, param tuning, reindex, circuit breaker)

New files: `decay.go`, `circuitbreaker.go`
Modify: `prompt.go` (matview and param tuning categories)

---

## 7. Prompt Assembly Logic (Conditional Sections)

The prompt in the spec (Part 6) is the best case. In practice, sections are conditional:

```go
func FormatPrompt(table TableContext, config OptimizerConfig) string {
    var sb strings.Builder
    
    // Always present:
    sb.WriteString(formatTableHeader(table))     // name, rows, size, indexes
    sb.WriteString(formatColumns(table))          // column list with types
    sb.WriteString(formatExistingIndexes(table))  // index definitions + scan counts
    
    // Conditional — only if write rate known (2+ snapshots):
    if table.WriteRate >= 0 {
        sb.WriteString(formatWriteRate(table))
        sb.WriteString(formatWorkloadClass(table))
    }
    
    // Conditional — only if collation is non-C:
    if table.Collation != "C" && table.Collation != "POSIX" {
        sb.WriteString(fmt.Sprintf("\nCollation: %s (non-C — LIKE prefix requires pattern_ops)\n", table.Collation))
    }
    
    // Conditional — only if selectivity data available:
    if len(table.ColumnStats) > 0 {
        sb.WriteString(formatSelectivity(table))
    }
    
    // Always present (queries hitting this table):
    sb.WriteString(formatQueries(table))
    
    // Conditional — only if plan data available:
    for i, q := range table.Queries {
        if q.PlanSummary != "" {
            sb.WriteString(fmt.Sprintf("  Plan: %s\n  Bottleneck: %s\n", q.PlanSummary, q.Bottleneck))
        }
        // If no plan: just query text + stats (v1 behavior)
    }
    
    return sb.String()
}
```

The prompt RULES section is always the same (the 12 rules from spec Part 6.2). The context packet varies based on data availability.

---

## 8. What NOT to Build

These are explicitly out of scope for the optimizer (handled elsewhere in the sidecar):

- **Tier 1 rules** (missing FK, duplicates, bloat, sequence) — already in `internal/analyzer/`
- **MCP tool responses** — already in `sidecar/resources.go` and `sidecar/tools.go`
- **Executor trust gating** — already in `internal/executor/`
- **Schema bootstrap** — already in `internal/schema/`
- **Reconnection** — already in `internal/ha/`
- **Prometheus metrics registration** — already in main, just add new labels

The optimizer package ONLY does: context assembly → LLM call → parse → validate → HypoPG → confidence → return findings. The analyzer writes them. The executor acts on them.
