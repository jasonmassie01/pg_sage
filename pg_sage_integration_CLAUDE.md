# CLAUDE.md — pg_sage Optimizer v2 Integration Wiring

## Mission

Wire the completed optimizer v2 package (4,640 lines, 132 tests, 18 source files in `internal/optimizer/`) into the running sidecar pipeline. When done: the sidecar collects → analyzes (Tier 1 rules) → optimizes (LLM + HypoPG) → executes (trust-gated CONCURRENTLY DDL) → monitors (rollback), and all 261+ tests pass.

**This is NOT a feature-building task.** All optimizer features are implemented and tested. This is plumbing — connecting the optimizer to the analyzer, config, LLM client, Prometheus, and executor.

---

## Architecture Context

```
main.go
  └→ sidecar.Run()
       ├→ collector.Run()       [goroutine, every 60s]
       ├→ analyzer.Run()        [goroutine, every 120-300s]
       │   ├→ Tier 1 rules (missing FK, duplicates, bloat, etc.)
       │   ├→ optimizer.Analyze()  ← WIRE THIS IN (Step 1)
       │   └→ upsert findings to sage.findings
       ├→ executor.Run()        [goroutine, every 120-300s]
       │   ├→ Read actionable findings
       │   ├→ Trust gate → Execute DDL → Monitor regression
       │   └→ Handle INCLUDE upgrades (detail->>'drop_ddl')  ← NEW
       ├→ mcp.Serve()           [:8080]
       └→ prometheus.Serve()    [:9187]
```

**Advisory lock:** `hashtext('pg_sage')` = 710190109. Single instance enforced.

**Gemini endpoint:** `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`
**Auth:** `Authorization: Bearer $SAGE_GEMINI_API_KEY`
**Model:** `gemini-2.5-flash` (NOT preview — returns 404)

---

## Existing Codebase (DO NOT BREAK)

### Sidecar Core (129 tests PASS — R10)

| Package | Tests | What It Covers |
|---------|-------|---------------|
| `sidecar` (root) | 11 | MCP, SSE, Prometheus |
| `internal/analyzer` | 5 | Bloat, plan time, regression, slow queries |
| `internal/briefing` | 11 | Generate with findings |
| `internal/collector` | 11 | SQL variants, circuit breaker, snapshots |
| `internal/config` | 9 | Defaults, precedence, hot-reload, validation |
| `internal/executor` | 4 | Trust gates (12 subtests), CONCURRENTLY, categorize |
| `internal/ha` | 10 | Advisory lock, recovery check |
| `internal/llm` | 24 | Chat, budget, circuit breaker, timeout, optimizer |
| `internal/retention` | 6 | Purge old data |
| `internal/schema` | 15 | Bootstrap, migrations, idempotent |
| `internal/startup` | 5 | Prereq checks |

### Optimizer v2 (132 tests PASS — current session)

| File | Tests | What It Covers |
|------|-------|---------------|
| `validate_test.go` | 28 | 5 validators + integration |
| `detection_test.go` | 22 | INCLUDE, partial, joins, matview, bloat, BRIN |
| `prompt_test.go` | 20 | Parse, format, fences, system prompt |
| `confidence_test.go` | 18 | 6 signals, ActionLevel, cap |
| `context_builder_test.go` | 13 | Workload, write rate, table extraction |
| `optimizer_test.go` | 13 | Query calls, extract columns, confidence integration |
| `fingerprint_test.go` | 13 | Normalize, group, ORM dedup |
| `cost_test.go` | 10 | Size, time, write amp, savings |
| `plancapture_test.go` | 8 | Summarize plan (8 scenarios) |
| `decay_test.go` | 7 | Decay percentage, trend analysis |
| `circuitbreaker_test.go` | 6 | Open, close, half-open, escalation |
| `hypopg_test.go` | 2 | extractTotalCost, isExplainable |

**Target after integration: 261+ tests, 0 failures.**

---

## Step 1: Wire Optimizer Into Sidecar Pipeline

### 1.1 Add `optimizer.Analyze()` Call in Analyzer

**File:** `internal/analyzer/analyzer.go`

Find the analyzer's main cycle function (likely `Run()` or `runCycle()`). After Tier 1 rules complete and findings are written, add the optimizer call:

```go
// After Tier 1 rules complete:
if cfg.LLM.Enabled && cfg.LLM.IndexOptimizer.Enabled {
    optCfg := optimizer.Config{
        MinQueryCalls:          cfg.LLM.IndexOptimizer.MinQueryCalls,
        MaxIndexesPerTable:     cfg.LLM.IndexOptimizer.MaxIndexesPerTable,
        MaxNewPerTable:         cfg.LLM.IndexOptimizer.MaxNewPerTable,
        MaxIncludeColumns:      cfg.LLM.IndexOptimizer.MaxIncludeColumns,
        WriteHeavyRatioPct:     cfg.LLM.IndexOptimizer.WriteHeavyRatioPct,
        WriteImpactThresholdPct: cfg.LLM.IndexOptimizer.WriteImpactThresholdPct,
        MinSnapshots:           cfg.LLM.IndexOptimizer.MinSnapshots,
        HypoPGMinImprovementPct: cfg.LLM.IndexOptimizer.HypoPGMinImprovementPct,
        ConfidenceThreshold:    cfg.LLM.IndexOptimizer.ConfidenceThreshold,
        PlanSource:             cfg.LLM.IndexOptimizer.PlanSource,
    }

    opt := optimizer.New(optCfg, llmManager.ForPurpose("index_optimization"), pool)
    optimizerResults, err := opt.Analyze(ctx, pool, latestSnapshots)
    if err != nil {
        log.Printf("optimizer: %v", err)
        // Don't fail cycle — Tier 1 findings are still valid
    }

    for _, result := range optimizerResults {
        for _, rec := range result.Recommendations {
            upsertOptimizerFinding(ctx, pool, result.Table, rec)
        }
    }
}
```

**The `upsertOptimizerFinding` function** writes to `sage.findings` with this schema:

```go
func upsertOptimizerFinding(ctx context.Context, pool *pgxpool.Pool, table string, rec optimizer.Recommendation) error {
    detail := map[string]interface{}{
        "llm_rationale":          rec.Rationale,
        "affected_queries":       rec.AffectedQueries,
        "estimated_improvement_pct": rec.EstimatedImprovementPct,
        "index_type":             rec.IndexType,
        "confidence":             rec.Confidence,
        "confidence_score":       rec.ConfidenceScore,
        "action_level":           rec.ActionLevel,
        "hypopg_validated":       rec.HypoPGValidated,
        "hypopg_improvement_pct": rec.HypoPGImprovementPct,
        "write_impact_pct":       rec.WriteImpactPct,
        "model_used":             rec.ModelUsed,
        "plan_data_available":    rec.PlanDataAvailable,
        "workload_class":         rec.WorkloadClass,
    }
    if rec.DropDDL != "" {
        detail["drop_ddl"] = rec.DropDDL
    }

    detailJSON, _ := json.Marshal(detail)

    severity := "warning"  // actionable
    if rec.ActionLevel == "informational" || rec.ActionLevel == "advisory" {
        severity = "info"  // advisory only
    }

    actionRisk := "safe"
    if rec.ActionLevel == "advisory" {
        actionRisk = "moderate"
    }

    _, err := pool.Exec(ctx, `
        INSERT INTO sage.findings (category, object_identifier, severity, status,
            detail, recommended_sql, first_seen, last_seen, occurrence_count)
        VALUES ($1, $2, $3, 'open', $4, $5, now(), now(), 1)
        ON CONFLICT (category, object_identifier)
        DO UPDATE SET detail = $4, recommended_sql = $5,
            last_seen = now(), occurrence_count = sage.findings.occurrence_count + 1
        WHERE sage.findings.status = 'open'`,
        rec.Category, "public."+table, severity, detailJSON, rec.DDL)

    return err
}
```

**Important:** The optimizer package's `Analyze()` function signature must match what the analyzer expects. Check the actual signature in `optimizer.go` and adapt the call accordingly. The optimizer needs:
- `context.Context`
- `*pgxpool.Pool` (for DB queries: pg_stats, columns, indexes, HypoPG, plans)
- Snapshot data (latest snapshots from collector, specifically "queries" and "tables" categories)
- An LLM client interface (for Chat calls)
- Config values

### 1.2 Add `optimizer_llm` Config Section

**File:** `internal/config/config.go`

Add to the config struct:

```go
type OptimizerLLMConfig struct {
    Enabled              bool    `yaml:"enabled"`
    Endpoint             string  `yaml:"endpoint"`              // "" = use general
    Model                string  `yaml:"model"`                 // "" = use general
    APIKey               string  `yaml:"api_key"`               // "" = use general
    TimeoutSeconds       int     `yaml:"timeout_seconds"`       // default 120
    TokenBudgetDaily     int     `yaml:"token_budget_daily"`    // default 50000
    MaxOutputTokens      int     `yaml:"max_output_tokens"`     // default 4096
    FallbackToGeneral    bool    `yaml:"fallback_to_general"`   // default true
}

type IndexOptimizerConfig struct {
    Enabled                 bool    `yaml:"enabled"`
    MinQueryCalls           int     `yaml:"min_query_calls"`           // default 100
    MaxIndexesPerTable      int     `yaml:"max_indexes_per_table"`     // default 10
    MaxNewPerTable          int     `yaml:"max_new_per_table"`         // default 3
    MaxIncludeColumns       int     `yaml:"max_include_columns"`       // default 5
    WriteHeavyRatioPct      int     `yaml:"write_heavy_ratio_pct"`     // default 70
    WriteImpactThresholdPct int     `yaml:"write_impact_threshold_pct"` // default 15
    MinSnapshots            int     `yaml:"min_snapshots"`             // default 2
    HypoPGMinImprovementPct int     `yaml:"hypopg_min_improvement_pct"` // default 10
    ConfidenceThreshold     float64 `yaml:"confidence_threshold"`      // default 0.5
    PlanSource              string  `yaml:"plan_source"`               // "auto" | "generic_plan" | "explain_cache"
}
```

Add these to the parent LLM config struct. Set defaults in `loadDefaults()` or equivalent. Add validation in `validate()`:
- `optimizer_llm.timeout_seconds` ≥ 30
- `optimizer_llm.token_budget_daily` ≥ 0
- `index_optimizer.min_query_calls` ≥ 1
- `index_optimizer.max_new_per_table` ≥ 1
- `index_optimizer.min_snapshots` ≥ 2

### 1.3 Create LLM Manager for Dual-Model Routing

**File:** `internal/llm/manager.go` (NEW)

```go
package llm

import "context"

type Manager struct {
    General   *Client
    Optimizer *Client // nil if not configured
    fallback  bool
}

func NewManager(generalCfg ClientConfig, optCfg *ClientConfig, fallback bool) *Manager {
    m := &Manager{
        General:  NewClient(generalCfg),
        fallback: fallback,
    }
    if optCfg != nil {
        m.Optimizer = NewClient(*optCfg)
    }
    return m
}

// ForPurpose returns the right client. The optimizer uses the dedicated
// client if configured, otherwise falls back to general.
func (m *Manager) ForPurpose(purpose string) *Client {
    if purpose == "index_optimization" && m.Optimizer != nil {
        return m.Optimizer
    }
    return m.General
}

// ChatForPurpose routes to the right model and handles fallback.
func (m *Manager) ChatForPurpose(ctx context.Context, purpose string, messages []Message) (string, int, error) {
    client := m.ForPurpose(purpose)
    resp, tokens, err := client.Chat(ctx, messages)
    if err != nil && client == m.Optimizer && m.fallback {
        // Optimizer failed — fall back to general
        return m.General.Chat(ctx, messages)
    }
    return resp, tokens, err
}
```

**Integration:** In `main.go` or wherever the LLM client is constructed, create a `Manager` instead of a bare `Client`. Pass the Manager to the analyzer, which passes it to the optimizer.

### 1.4 Add Per-Model Prometheus Metrics

**File:** Wherever Prometheus metrics are registered (likely `main.go` or `internal/metrics/`)

Add labels to existing LLM metrics:

```go
// Existing (update with labels):
pg_sage_llm_calls_total{model="gemini-2.5-flash",purpose="briefing"}
pg_sage_llm_calls_total{model="claude-opus-4-6",purpose="index_optimization"}
pg_sage_llm_tokens_used_today{model="gemini-2.5-flash"}
pg_sage_llm_tokens_used_today{model="claude-opus-4-6"}
pg_sage_llm_latency_seconds{model="claude-opus-4-6",purpose="index_optimization"}
pg_sage_llm_circuit_open{model="gemini-2.5-flash"}
pg_sage_llm_circuit_open{model="claude-opus-4-6"}

// New optimizer-specific:
pg_sage_optimizer_tables_analyzed_total
pg_sage_optimizer_recommendations_total{action_level="autonomous|advisory|informational"}
pg_sage_optimizer_hypopg_validations_total{outcome="accepted|rejected|unavailable"}
pg_sage_optimizer_circuit_breaker_open{table="public.orders"}
```

The `Client` struct already tracks calls and tokens internally. Add the model name as a label dimension when incrementing counters.

### 1.5 Executor: Handle INCLUDE Upgrades (DROP + CREATE)

**File:** `internal/executor/executor.go`

When the executor processes a finding with `category = 'index_optimization'`, check if `detail->>'drop_ddl'` is present:

```go
func (e *Executor) executeAction(ctx context.Context, finding Finding) error {
    // ... existing trust gate, DDL timeout, etc. ...

    // Execute the primary DDL (CREATE INDEX CONCURRENTLY)
    err := e.executeDDL(ctx, finding.RecommendedSQL)
    if err != nil {
        return fmt.Errorf("CREATE failed: %w", err)
    }

    // If this is an INCLUDE upgrade, DROP the old index AFTER verifying the new one
    dropDDL, _ := finding.Detail["drop_ddl"].(string)
    if dropDDL != "" {
        // Step 1: Verify new index is valid
        // Extract new index name from recommended_sql
        newIndexName := extractIndexName(finding.RecommendedSQL)
        valid, err := e.checkIndisvalid(ctx, newIndexName)
        if err != nil || !valid {
            log.Printf("executor: new index %s is INVALID — aborting DROP of old index", newIndexName)
            // Create a critical finding about the invalid index
            return fmt.Errorf("new index invalid, old index preserved")
        }

        // Step 2: DROP the old index
        err = e.executeDDL(ctx, dropDDL)
        if err != nil {
            log.Printf("executor: DROP old index failed: %v (new index still valid)", err)
            // Non-fatal — new index is valid, old index remains (duplicate but harmless)
        }
    }

    return nil
}
```

### 1.6 Executor: Skip Advisory-Only Findings

Findings with `severity = 'info'` (materialized_view_candidate, parameter_tuning, low-confidence recommendations) must NOT be auto-executed:

```go
// In the finding selection query:
SELECT ... FROM sage.findings
WHERE status = 'open'
  AND severity IN ('critical', 'warning')  -- NOT 'info'
  AND recommended_sql IS NOT NULL
  AND recommended_sql != ''
  AND acted_on_at IS NULL
ORDER BY severity DESC, last_seen DESC
```

If this filter already exists, verify it excludes `info` severity.

---

## Step 2: Run Full Test Suite

After completing Step 1 wiring, run all tests:

```bash
cd sidecar
go test ./... -count=1 -timeout 300s -p 1 2>&1 | tee test_results.txt
```

**Expected:**
- `internal/optimizer/...` — 132 PASS
- `internal/analyzer/...` — 5 PASS
- `internal/collector/...` — 11 PASS
- `internal/config/...` — 9 PASS (+ new tests for optimizer_llm config)
- `internal/executor/...` — 4 PASS
- `internal/llm/...` — 24 PASS (+ new tests for Manager)
- All other packages — unchanged
- **Total: 261+ PASS, 0 FAIL**

Also run:
```bash
go build ./...
go vet ./...
```
Both must be clean.

### New Tests to Add in Step 2

**`internal/llm/manager_test.go`** (NEW):

```go
func TestManager_GeneralOnly(t *testing.T)
    // No optimizer configured
    // ForPurpose("index_optimization") returns General client
    // ForPurpose("briefing") returns General client

func TestManager_DualModel(t *testing.T)
    // Both configured
    // ForPurpose("index_optimization") returns Optimizer client
    // ForPurpose("briefing") returns General client

func TestManager_FallbackOnError(t *testing.T)
    // Optimizer client fails, fallback=true
    // ChatForPurpose falls back to General
    // Assert: General response returned

func TestManager_FallbackDisabled(t *testing.T)
    // Optimizer client fails, fallback=false
    // ChatForPurpose returns error
```

**`internal/config/config_test.go`** (ADD):

```go
func TestConfigDefaults_OptimizerLLM(t *testing.T)
    // Load empty config
    // Assert: optimizer_llm.enabled == false
    // Assert: optimizer_llm.timeout_seconds == 120
    // Assert: index_optimizer.min_query_calls == 100
    // Assert: index_optimizer.max_new_per_table == 3
    // Assert: index_optimizer.min_snapshots == 2

func TestConfigValidation_OptimizerLLM(t *testing.T)
    // timeout_seconds = 0 → validation error
    // min_snapshots = 0 → validation error
    // max_new_per_table = -1 → validation error
```

---

## Step 3: Live Integration Test

Run the sidecar against a real PostgreSQL instance (Cloud SQL or local Docker) with Phase 15 test data loaded. The goal is to verify every DB-dependent function in the optimizer that has 0% unit test coverage.

### 3.1 Prerequisites

- PostgreSQL 16+ (for GENERIC_PLAN support) or 14+ (query-text-only fallback)
- `pg_stat_statements` enabled
- HypoPG extension installed: `CREATE EXTENSION IF NOT EXISTS hypopg;`
- Phase 15 test data loaded (50K customers, 500K orders, 1M line_items, etc.)
- Gemini API key: `export SAGE_GEMINI_API_KEY="<key>"`
- Slow query workload run (20 iterations of 8 patterns from Phase 15)
- `ANALYZE;` run after data load

### 3.2 Config for Integration Test

```yaml
mode: standalone

postgres:
  host: <HOST>
  port: 5432
  user: sage_agent
  password: <PASSWORD>
  database: sage_test
  sslmode: require
  max_connections: 5

collector:
  interval_seconds: 60
  batch_size: 1000

analyzer:
  interval_seconds: 120
  slow_query_threshold_ms: 500
  seq_scan_min_rows: 10000
  unused_index_window_days: 0
  table_bloat_dead_tuple_pct: 10
  table_bloat_min_rows: 1000

trust:
  level: autonomous
  tier3_safe: true
  tier3_moderate: true
  rollback_threshold_pct: 10
  rollback_window_minutes: 15
  rollback_cooldown_days: 0

executor:
  ddl_timeout_seconds: 300
  maintenance_window: "* * * * *"

llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  api_key: ${SAGE_GEMINI_API_KEY}
  model: "gemini-2.5-flash"
  timeout_seconds: 60
  token_budget_daily: 100000
  max_output_tokens: 2048
  index_optimizer:
    enabled: true
    min_query_calls: 10       # lowered for testing
    max_indexes_per_table: 10
    max_new_per_table: 3
    max_include_columns: 5
    write_heavy_ratio_pct: 70
    write_impact_threshold_pct: 15
    min_snapshots: 2
    hypopg_min_improvement_pct: 10
    confidence_threshold: 0.5
    plan_source: auto

# Dual-model optional — test with general model first:
# optimizer_llm:
#   enabled: true
#   endpoint: "https://api.anthropic.com/v1"
#   model: "claude-opus-4-6"
#   ...

mcp:
  enabled: true
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

### 3.3 Start Sidecar + Backdate Trust Ramp

```bash
./pg_sage_sidecar --mode=standalone --config=config.yaml 2>&1 | tee integration.log &
SIDECAR_PID=$!
sleep 15

# Backdate trust ramp:
psql -h <HOST> -U sage_agent -d sage_test -c "
INSERT INTO sage.config (key, value)
VALUES ('trust_ramp_start', (now() - interval '32 days')::text)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;"
```

### 3.4 Verification Checklist (Wait 2 Full Cycles = ~5 min)

```bash
sleep 300
```

**Collection verified:**
```sql
SELECT category, count(*) FROM sage.snapshots
GROUP BY category ORDER BY category;
-- "queries" MUST be present
```
- [ ] "queries" category present with count ≥ 2
- [ ] "tables" category present
- [ ] "indexes" category present

**Tier 1 findings present:**
```sql
SELECT category, count(*) FROM sage.findings WHERE status = 'open'
GROUP BY category ORDER BY category;
-- Expected: missing_fk_index, duplicate_index, slow_query, table_bloat, sequence_exhaustion
```
- [ ] At least 10 Tier 1 findings

**Optimizer findings present:**
```sql
SELECT category, object_identifier, severity,
       detail->>'llm_rationale' IS NOT NULL AS has_rationale,
       detail->>'confidence_score' AS confidence,
       detail->>'action_level' AS action_level,
       detail->>'hypopg_validated' AS hypopg_validated,
       left(recommended_sql, 100) AS sql_preview
FROM sage.findings
WHERE category = 'index_optimization'
   OR detail->>'llm_rationale' IS NOT NULL
ORDER BY last_seen DESC;
```
- [ ] At least 1 finding with `category = 'index_optimization'`
- [ ] `detail->>'llm_rationale'` is NOT NULL (LLM was called)
- [ ] `recommended_sql` uses CONCURRENTLY
- [ ] `detail->>'confidence_score'` is a number between 0 and 1
- [ ] `detail->>'action_level'` is one of: autonomous, advisory, informational
- [ ] `detail->>'hypopg_validated'` is true (if HypoPG installed)

**Optimizer-specific DB functions verified (these have 0% unit coverage):**

| Function | How to Verify | Checklist |
|----------|--------------|-----------|
| `BuildTableContexts` | Optimizer findings reference real tables | [ ] |
| `CapturePlans` (GENERIC_PLAN on PG16+) | `detail->>'plan_data_available'` = true | [ ] |
| `CheckColdStart` | First cycle blocked, second cycle allowed | [ ] Check logs for "waiting for snapshots" |
| LLM call succeeds | `pg_sage_llm_calls_total > 0` in Prometheus | [ ] |
| `parseRecommendations` with real Gemini output | Findings have valid DDL | [ ] |
| All 8 validators on real data | No hallucinated columns, no duplicates | [ ] |
| HypoPG `IsAvailable` | Logs show "HypoPG available" or "not installed" | [ ] |
| HypoPG `Validate` (cost comparison) | `detail->>'hypopg_improvement_pct'` has a value | [ ] |
| Confidence scoring with real signals | Score > 0 in findings | [ ] |
| `fetchCollation` | No error in logs for collation fetch | [ ] |
| `fetchColStats` (pg_stats) | Prompt includes MCV data if available | [ ] |

**LLM metrics (Prometheus):**
```bash
curl -s http://localhost:9187/metrics | grep pg_sage_llm
```
- [ ] `pg_sage_llm_calls_total` > 0
- [ ] `pg_sage_llm_tokens_used_today` > 0
- [ ] `pg_sage_llm_circuit_open` = 0
- [ ] `pg_sage_llm_latency_seconds` has observations

**Executor acted on optimizer findings:**
```sql
SELECT id, finding_id, recommended_sql, outcome, error_message
FROM sage.action_log
WHERE finding_id IN (
    SELECT id FROM sage.findings WHERE category = 'index_optimization'
)
ORDER BY executed_at DESC;
```
- [ ] At least 1 action with outcome = 'success'
- [ ] DDL contains CONCURRENTLY
- [ ] No infinite retry (same finding_id appears at most once)

**Negative checks:**
```sql
-- No PK/unique indexes in optimizer recommendations:
SELECT * FROM sage.findings
WHERE category = 'index_optimization'
  AND (object_identifier LIKE '%_pkey' OR recommended_sql LIKE '%UNIQUE%');
-- Must return 0 rows

-- No sage schema in findings:
SELECT * FROM sage.findings
WHERE object_identifier LIKE 'sage.%';
-- Must return 0 rows
```
- [ ] No PK indexes recommended
- [ ] No sage schema objects

### 3.5 LLM Failure + Recovery Test

```bash
# Break endpoint:
kill $SIDECAR_PID
sed -i 's|generativelanguage.googleapis.com|invalid.example.com|' config.yaml
./pg_sage_sidecar --mode=standalone --config=config.yaml 2>&1 | tee broken.log &
SIDECAR_PID=$!
sleep 180

# Circuit breaker should open:
curl -s http://localhost:9187/metrics | grep pg_sage_llm_circuit_open
# Expected: 1

# Tier 1 findings still generated:
psql -h <HOST> -U sage_agent -d sage_test -c "
SELECT count(*) FROM sage.findings WHERE status='open';"
# Must be > 0

# Restore:
kill $SIDECAR_PID
sed -i 's|invalid.example.com|generativelanguage.googleapis.com|' config.yaml
./pg_sage_sidecar --mode=standalone --config=config.yaml 2>&1 | tee integration.log &
SIDECAR_PID=$!
sleep 330

curl -s http://localhost:9187/metrics | grep pg_sage_llm_circuit_open
# Expected: 0
```
- [ ] Circuit breaker opens on bad endpoint
- [ ] Tier 1 findings still work during outage
- [ ] Circuit breaker closes after cooldown + restore

---

## Step 4: Fix Deferred Items

### 4.1 EstimateSize Wiring

**Problem:** `hypopg.EstimateSize()` exists but isn't called because it needs the hypothetical index OID from `Validate()`, which calls `hypopg_reset()` before size can be estimated.

**Fix:** Change the validation flow to estimate size BEFORE resetting:

```go
func (h *HypoPG) Validate(ctx context.Context, conn *pgxpool.Conn, rec Recommendation, queries []QueryInfo) (*HypoPGResult, error) {
    // 1. Create hypothetical index
    oid, err := h.createHypo(ctx, conn, rec.DDL)
    if err != nil { return nil, err }

    // 2. Measure costs (before/after for each query)
    improvement, err := h.measureCosts(ctx, conn, queries)
    if err != nil {
        h.reset(ctx, conn)
        return nil, err
    }

    // 3. Estimate size BEFORE reset (needs the OID alive)
    estimatedSize, err := h.estimateSize(ctx, conn, oid)
    if err != nil {
        // Non-fatal — use heuristic estimate instead
        estimatedSize = h.heuristicSize(rec)
    }

    // 4. Reset (cleanup)
    h.reset(ctx, conn)

    return &HypoPGResult{
        ImprovementPct: improvement,
        EstimatedSize:  estimatedSize,
        Accepted:       improvement >= h.config.MinImprovementPct,
    }, nil
}
```

**Key change:** The `Validate` return type now includes `EstimatedSize`. Update `types.go` `HypoPGResult` struct if needed. The cost estimation in `cost.go` should prefer `HypoPGResult.EstimatedSize` over its heuristic when available.

### 4.2 Enhanced Regression Detection

**Problem:** The executor's rollback monitor only checks `mean_exec_time` delta. Write amplification from new indexes shows up as INSERT latency, WAL bytes, and checkpoint frequency — not query read latency.

**File:** `internal/executor/executor.go` (or rollback monitor file)

Add these signals to the regression check:

```go
type RegressionSignals struct {
    // Existing:
    QueryLatencyPctChange float64

    // New — from pg_stat_statements for INSERT/UPDATE on affected table:
    InsertLatencyPctChange float64

    // New — from pg_stat_statements aggregate:
    WALBytesPctChange float64

    // New — from pg_stat_bgwriter / pg_stat_checkpointer:
    CheckpointFreqPctChange float64
}

func (e *Executor) checkRegression(ctx context.Context, action ActionLogEntry) (bool, RegressionSignals) {
    signals := RegressionSignals{}

    // Existing: read latency
    signals.QueryLatencyPctChange = e.measureQueryLatencyDelta(ctx, action)

    // NEW: write latency (INSERT/UPDATE on the table that got the new index)
    signals.InsertLatencyPctChange = e.measureInsertLatencyDelta(ctx, action)

    // NEW: WAL bytes (from pg_stat_statements wal_bytes column, if available)
    signals.WALBytesPctChange = e.measureWALDelta(ctx, action)

    // NEW: checkpoint frequency
    signals.CheckpointFreqPctChange = e.measureCheckpointDelta(ctx, action)

    shouldRollback :=
        signals.QueryLatencyPctChange > e.config.RollbackThresholdPct ||
        signals.InsertLatencyPctChange > 20.0 ||  // 20% INSERT latency increase
        signals.WALBytesPctChange > 50.0           // 50% WAL spike

    return shouldRollback, signals
}
```

**Data sources:**
- INSERT latency: `SELECT mean_exec_time FROM pg_stat_statements WHERE query LIKE 'INSERT INTO <table>%'`
- WAL bytes: `SELECT wal_bytes FROM pg_stat_statements` (PG14+, needs `track_wal_io_timing`)
- Checkpoint frequency: `SELECT checkpoints_timed + checkpoints_req FROM pg_stat_bgwriter` (PG14-16) or `pg_stat_checkpointer` (PG17+)

**Tests to add:**

```go
func TestRegression_InsertLatencySpike(t *testing.T)
    // queryLatencyPctChange = 0% (reads fine)
    // insertLatencyPctChange = 30%
    // Assert: shouldRollback = true

func TestRegression_WALSpike(t *testing.T)
    // all latency signals fine
    // walBytesPctChange = 60%
    // Assert: shouldRollback = true

func TestRegression_AllStable(t *testing.T)
    // all signals < threshold
    // Assert: shouldRollback = false
```

---

## Step 5: Test on AlloyDB

**Prerequisite:** AlloyDB PG17 instance accessible (see `pg_sage_alloydb_test_CLAUDE.md` for provisioning).

**Expected: zero code changes.** AlloyDB is standard PostgreSQL with disaggregated storage. The optimizer should work identically.

### 5.1 Run Unit + Integration Tests Against AlloyDB

```bash
export SAGE_DATABASE_URL="postgres://sage_agent:pw@<ALLOYDB_IP>:5432/sage_test?sslmode=require"
export SAGE_GEMINI_API_KEY="<key>"

cd sidecar
go test ./... -count=1 -timeout 300s -p 1
# Expected: 261+ PASS / 0 FAIL
```

### 5.2 Run Full Pipeline Against AlloyDB

Same config as Step 3, change only `postgres.host`. Start sidecar, backdate trust ramp, wait 2 cycles.

**AlloyDB-specific checks:**
- [ ] `google_ml.*` internal tables do NOT appear in optimizer findings
- [ ] HypoPG works on AlloyDB (or graceful fallback if not installed)
- [ ] CONCURRENTLY DDL completes successfully on disaggregated storage
- [ ] ML-assisted vacuum may have already cleaned dead tuples — bloat findings may differ (this is correct behavior)
- [ ] Checkpoint pressure findings may differ (disaggregated WAL)

### 5.3 Parity Check

```sql
-- Compare finding counts:
SELECT category, count(*) FROM sage.findings WHERE status = 'open'
GROUP BY category ORDER BY category;
```

| Category | Cloud SQL (expected) | AlloyDB (actual) | Match? |
|----------|---------------------|-----------------|--------|
| index_optimization | ≥1 | ? | ±25% |
| duplicate_index | 2 | ? | exact |
| missing_fk_index | 2 | ? | exact |
| slow_query | ≥4 | ? | ±25% |
| table_bloat | 2 | ? | may differ (ML-vacuum) |
| sequence_exhaustion | 1 | ? | exact |

- [ ] Critical findings match across platforms
- [ ] Any AlloyDB-specific differences documented

---

## Definition of Done

### Step 1: Integration Wiring
- [ ] `optimizer.Analyze()` called from analyzer cycle
- [ ] `optimizer_llm` config section parsed and validated
- [ ] `llm.Manager` created with dual-model routing
- [ ] Prometheus metrics have per-model labels
- [ ] Executor handles `detail->>'drop_ddl'` for INCLUDE upgrades
- [ ] Executor skips `severity = 'info'` findings
- [ ] `go build ./...` clean
- [ ] `go vet ./...` clean

### Step 2: Tests
- [ ] 129 existing sidecar tests PASS
- [ ] 132 optimizer tests PASS
- [ ] New Manager tests PASS (4+)
- [ ] New config tests PASS (2+)
- [ ] Total: 261+ PASS, 0 FAIL

### Step 3: Live Integration
- [ ] Optimizer findings appear in sage.findings with real data
- [ ] LLM called successfully (Prometheus metrics > 0)
- [ ] HypoPG validation working (or graceful fallback)
- [ ] Confidence scores populated
- [ ] Executor creates at least 1 index from optimizer recommendation
- [ ] CONCURRENTLY DDL succeeds
- [ ] Circuit breaker opens/closes correctly
- [ ] Tier 1 findings unaffected by optimizer integration
- [ ] No PK/unique/sage objects in optimizer findings

### Step 4: Deferred Fixes
- [ ] EstimateSize called before hypopg_reset()
- [ ] cost.go uses HypoPG size when available
- [ ] Enhanced regression: INSERT latency signal added
- [ ] Enhanced regression: WAL bytes signal added
- [ ] 3+ new regression detection tests pass

### Step 5: AlloyDB
- [ ] 261+ tests pass against AlloyDB
- [ ] Full pipeline produces optimizer findings on AlloyDB
- [ ] No AlloyDB-specific bugs (or documented)
- [ ] Parity with Cloud SQL ±25%
