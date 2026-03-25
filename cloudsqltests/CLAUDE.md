# CLAUDE.md — pg_sage Standalone Sidecar (Phase 0.2 + LLM Integration)

## Mission

Add a `--mode=standalone` operating mode to the pg_sage Go sidecar that runs the full collector → analyzer → executor → briefing pipeline over a standard libpq connection — without the C extension installed. This unlocks Cloud SQL, RDS, Aurora, Supabase, Neon, and every other managed Postgres service where custom extensions are blocked.

When complete, a user should be able to run:

```bash
./pg_sage_sidecar --mode=standalone \
  --pg-host=<CLOUD_SQL_IP> \
  --pg-port=5432 \
  --pg-user=sage_agent \
  --pg-password=<PASSWORD> \
  --pg-database=mydb \
  --pg-sslmode=require
```

And get the same findings, recommendations, MCP server, Prometheus metrics, briefings, **and autonomous actions** as the extension mode — with LLM-powered index optimization via Gemini — minus the background worker efficiency and ALTER SYSTEM config tuning.

---

## PRE-BUILD: Git Workflow & Commit Discipline

**Every logical change gets its own commit.** When something breaks in testing, we need to know which commit introduced it. No smushing 15 changes into one commit.

```bash
cd pg_sage
git checkout master && git pull origin master
git checkout -b feat/standalone-v0.7

# Commit sequence (minimum):
# 1. "fix: v1 test report bugs (NULL queryid, PG17 checkpointer, NULL catalog columns, config validation, grants check, timestamp parsing, findings columns, snapshot persist, deadlocks NULL)"
# 2. "fix: schema exclusion in FK/index/table collectors + analyzer wrapper"
# 3. "fix: bloat min_rows threshold, plan_time collection, reset detection"
# 4. "feat: sage schema self-indexing in bootstrap DDL"
# 5. "feat: LLM client hardening (Gemini integration, timeout, circuit breaker, token budget)"
# 6. "feat: LLM index optimizer (cross-query consolidation, INCLUDE columns, budget guards)"
# 7. "feat: pg_stat_io collection PG16+, partition aggregation"
# 8. "test: LLM client + index optimizer unit and integration tests"
# 9. "chore: config.example.yaml with all v0.7 settings"
# 10. "release: v0.7.0-rc1"

# After all work:
git tag v0.7.0-rc1
git push origin feat/standalone-v0.7
git push origin v0.7.0-rc1
```

**Test environments MUST pull by tag `v0.7.0-rc1`, not `master`, not `latest`.**

Docker build:
```bash
cd sidecar
docker build -t us-central1-docker.pkg.dev/${GCP_PROJECT}/pg-sage/sidecar:v0.7.0-rc1 .
docker push us-central1-docker.pkg.dev/${GCP_PROJECT}/pg-sage/sidecar:v0.7.0-rc1
```

---

## PRE-BUILD: LLM Configuration (Gemini)

The LLM backend is **Google Gemini** via its OpenAI-compatible endpoint. Zero adapter code needed — Gemini speaks standard OpenAI chat completions format natively.

```yaml
llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  api_key: ${SAGE_GEMINI_API_KEY}   # Gemini API key from Google AI Studio
  model: "gemini-2.5-flash"  # fast + cheap for index analysis
  timeout_seconds: 60
  token_budget_daily: 100000
  context_budget_tokens: 4096
  cooldown_seconds: 300
  index_optimizer:
    enabled: true
    min_query_calls: 100
    max_indexes_per_table: 3
    max_include_columns: 5
    over_indexed_ratio: 1.0
    write_heavy_ratio: 0.5
```

**Environment variable for test deployment:**
```bash
export SAGE_GEMINI_API_KEY="<your-gemini-key>"
```

**Verify Gemini endpoint works before writing any code:**
```bash
curl "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${SAGE_GEMINI_API_KEY}" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Say hello"}],
    "max_tokens": 50
  }'
```

Must return a valid JSON response with `choices[0].message.content`. If this fails, the API key is wrong or the model name changed — fix before proceeding.

**Key Gemini details:**
- Base URL: `https://generativelanguage.googleapis.com/v1beta/openai/`
- Auth header: `Authorization: Bearer $GEMINI_API_KEY` (NOT `x-api-key`)
- Request format: standard OpenAI `{"model": "...", "messages": [...], "max_tokens": N}`
- Response format: standard OpenAI `{"choices": [{"message": {"content": "..."}}], "usage": {"total_tokens": N}}`
- Token tracking: `usage.prompt_tokens`, `usage.completion_tokens`, `usage.total_tokens`
- Rate limits: 1500 req/min for flash models (generous)

---

## PRE-BUILD: Audit Existing LLM Client

Before adding features, verify the existing `internal/llm/` code actually works.

### Read the code first:
```bash
cat internal/llm/client.go
cat internal/llm/circuit_breaker.go
cat internal/llm/context.go
```

### Verify these are implemented (fix if not):

**HTTP client basics:**
```go
// client.go MUST have:
client := &http.Client{
    Timeout: time.Duration(config.LLM.TimeoutSeconds) * time.Second,
}
// Context propagation:
req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
// Auth header:
req.Header.Set("Authorization", "Bearer "+config.LLM.APIKey)
req.Header.Set("Content-Type", "application/json")
// Response body close:
defer resp.Body.Close()
// Response size limit:
body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB max
// Non-200 handling:
if resp.StatusCode != 200 {
    return fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(body))
}
// Empty choices handling:
if len(response.Choices) == 0 {
    return "", fmt.Errorf("LLM returned empty choices array")
}
```

**Circuit breaker:**
```go
// circuit_breaker.go MUST implement:
// - Retry with exponential backoff: 1s, 4s, 16s (3 attempts)
// - After 3 failures: open circuit for cooldown_seconds
// - During open circuit: all LLM calls return immediately with fallback
// - After cooldown: half-open state, allow one probe call
// - If probe succeeds: close circuit
// - If probe fails: reopen for another cooldown period
```

**Token budget:**
```go
// client.go MUST track:
// - Parse response.Usage.TotalTokens (or .PromptTokens + .CompletionTokens)
// - Accumulate daily total in an atomic int64
// - Reject calls when daily total exceeds config.LLM.TokenBudgetDaily
// - Reset mechanism: check on each call:
//     if time.Now().UTC().YearDay() != lastResetDay || time.Now().UTC().Year() != lastResetYear {
//         atomic.StoreInt64(&dailyTokens, 0)
//         lastResetDay = time.Now().UTC().YearDay()
//         lastResetYear = time.Now().UTC().Year()
//     }
// - Expose via Prometheus gauge: pg_sage_llm_tokens_budget_remaining
//   (computed as config.TokenBudgetDaily - dailyTokens)
```

### Connect to Gemini and verify end-to-end:

Start the sidecar with LLM enabled, pointed at Gemini, connected to a PG instance with data.

```bash
./pg_sage_sidecar --mode=standalone \
  --config=config.yaml  # with llm.enabled: true and Gemini config
```

**Checklist (all must pass before proceeding):**
- [ ] Sidecar starts without error when `llm.enabled: true`
- [ ] First LLM call appears in logs within one analyzer cycle (~10 min)
- [ ] Request body shows proper OpenAI chat format (visible at debug log level)
- [ ] Gemini response is parsed — content appears in briefing or finding detail
- [ ] Token count tracked from response `usage.total_tokens`
- [ ] Prometheus `pg_sage_llm_calls_total` > 0
- [ ] Prometheus `pg_sage_llm_tokens_total` > 0

**Then test failure modes:**
- [ ] Set `llm.endpoint` to garbage URL → sidecar logs error, falls back to rules-only
- [ ] Set `llm.api_key` to "invalid" → Gemini returns 401, circuit breaker opens after retries
- [ ] Set `llm.timeout_seconds: 1` → timeout fires on real Gemini calls, circuit breaker opens
- [ ] Set `llm.token_budget_daily: 100` → budget exhausted after 1-2 calls, remaining calls blocked

If ANY of these fail, fix the client before proceeding to the index optimizer.

---

## PRE-BUILD: V1 Test Report Fixes

These 9 bugs were found in v1 testing. Apply each as its own commit.

### Already fixed in v0.6.2 (verify they're in the codebase):
1. **Bug #1:** NULL queryid → `COALESCE(queryid, 0)` + `AND queryid IS NOT NULL`
2. **Bug #2:** PG17 pg_stat_checkpointer → version-conditional SQL
3. **Bug #3:** NULL catalog columns → COALESCE wrappers
4. **Bug #4:** Config validation → `validate()` method
5. **Bug #5:** Wrong user in grants check → `SELECT current_user`
6. **Bug #6:** Trust ramp timestamp parsing → multi-format parser
7. **Bug #7:** findings INSERT nonexistent columns → removed
8. **Bug #8:** Snapshot persist schema mismatch → rewrote persist()
9. **Bug #9:** NULL deadlocks → COALESCE

### New fixes from v2 analysis (implement now):
10. **FIX-1: Schema exclusion** — FK collector, index collector, table collector all must exclude `sage`, `pg_catalog`, `information_schema`. Also add schema exclusion at analyzer wrapper level.
11. **FIX-2: Bloat min_rows** — Add `table_bloat_min_rows: 1000` config. Skip bloat findings for tiny tables.
12. **FIX-3: total_plan_time** — Add to query stats collector. Add `high_plan_time` analyzer rule.
13. **FIX-4: Reset detection** — Compare total calls between snapshots. Skip regression analysis on reset.
14. **FIX-5: pg_stat_io** — Conditional collection on PG16+.
15. **FIX-6: Partition aggregation** — Collect `pg_inherits`. Aggregate child stats under parent.
16. **FIX-7: Unique index protection** — Verify unused index rule excludes unique indexes. Add test.
17. **Sage self-indexing** — Add `idx_action_log_finding`, `idx_mcp_log_client`, `idx_findings_category_status` to bootstrap DDL.

---

## PRE-BUILD: Prometheus Metrics to Add

```go
// LLM metrics
pg_sage_llm_calls_total              // counter
pg_sage_llm_errors_total             // counter (by type: timeout, http_error, parse_error)
pg_sage_llm_tokens_total             // counter
pg_sage_llm_tokens_budget_remaining  // gauge
pg_sage_llm_latency_seconds          // histogram
pg_sage_llm_circuit_open             // gauge: 0=closed, 1=open
pg_sage_llm_parse_failures_total     // counter

// Index optimizer metrics
pg_sage_index_optimizer_tables_analyzed_total   // counter
pg_sage_index_optimizer_recommendations_total   // counter (by type: create, drop, include_upgrade)
pg_sage_index_optimizer_rejections_total        // counter (by reason: over_indexed, write_heavy, max_per_cycle)
```

---

## PRE-BUILD: Pre-Test Verification

After all code changes, before tagging `v0.7.0-rc1`:

```bash
# 1. All tests pass
cd sidecar && go test ./... -count=1

# 2. Integration tests with real Gemini (requires API key)
SAGE_GEMINI_API_KEY=$KEY go test ./... -tags=integration -count=1 -timeout=300s

# 3. Clean build
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar/
./pg_sage_sidecar --version  # must print v0.7.0-rc1

# 4. Verify Gemini integration end-to-end
#    Start sidecar against a local PG with test data
#    Wait 15 min for analyzer + LLM cycle
#    Check sage.findings for index_optimization category findings
#    Check sage.briefings for llm_used = true

# 5. Tag and push
git tag v0.7.0-rc1 && git push origin feat/standalone-v0.7 && git push origin v0.7.0-rc1

# 6. Docker build + push
docker build -t us-central1-docker.pkg.dev/${GCP_PROJECT}/pg-sage/sidecar:v0.7.0-rc1 .
docker push us-central1-docker.pkg.dev/${GCP_PROJECT}/pg-sage/sidecar:v0.7.0-rc1

# 7. Verify test environment
docker run --rm <image>:v0.7.0-rc1 ./pg_sage_sidecar --version
# Must print: v0.7.0-rc1
```

---

## Context

### What exists today

The pg_sage repo has two components:

1. **C extension** (`src/`): Three background workers (collector, analyzer, briefing), rules engine, trust model, circuit breaker, `sage.*` schema, all SQL functions. This is the brain. ~18K lines of C. Requires `shared_preload_libraries` and `CREATE EXTENSION`.

2. **Go sidecar** (`sidecar/`): MCP server (JSON-RPC over SSE), Prometheus exporter (`/metrics`), health endpoint. This is the mouth. It reads from the `sage.*` schema that the C extension populates and exposes it externally. It does NOT independently collect or analyze anything.

### What needs to change

The Go sidecar needs a second operating mode where it IS the brain — it replaces the C background workers with SQL-based polling loops that query standard Postgres catalog views over a normal connection.

### Architecture constraints

In standalone mode:
- **No C extension installed.** No `sage.*` GUC parameters. No background workers.
- **No superuser.** The sidecar connects as `sage_agent` with `pg_monitor` role.
- **No ALTER SYSTEM.** Managed services block this (or severely limit it).
- **No filesystem access.** Cannot read `csvlog` or `auto_explain` log files.
- **Network latency.** Every query is a round trip. Minimize query count per cycle.
- The sidecar CREATES the `sage` schema and tables itself on first connection.
- Configuration comes from a YAML config file and/or CLI flags and/or environment variables — not GUCs.

### Key spec references

The spec (`docs/pg_sage_spec_v2.2.md`) already defines:
- **Deployment Matrix** — sidecar mode marked "Required" for RDS, Cloud SQL, Neon
- **Privilege Model** — `sage_agent` role with `pg_monitor`, writes to `sage` schema only
- **Schema Reference** — exact DDL for `sage.snapshots`, `sage.findings`, `sage.action_log`, `sage.explain_cache`, `sage.briefings`, `sage.config`, `sage.mcp_log`
- **Safety Systems** — circuit breaker thresholds, advisory lock, HA awareness
- **Rules Engine** — every Tier 1 rule with its source catalog views and thresholds

---

## Build Plan

### Phase 1: Core Infrastructure

#### 1.1 Mode flag and config

Add `--mode` flag to the sidecar CLI:
- `--mode=extension` (default, current behavior) — reads from `sage.*` schema populated by C extension
- `--mode=standalone` — runs its own collector/analyzer/executor loops, creates schema, populates everything

All CLI flags (`--pg-host`, `--pg-port`, etc.) override their YAML config equivalents. Environment variables (`SAGE_PG_HOST`, etc.) override YAML but are overridden by CLI flags. Precedence: CLI > env > YAML > defaults.

Create a `config.yaml` schema. In standalone mode, config comes from this file:

```yaml
mode: standalone

postgres:
  host: 10.0.0.5
  port: 5432
  user: sage_agent
  password: ${SAGE_PG_PASSWORD}  # env var expansion
  database: mydb
  sslmode: require
  max_connections: 5             # pool size. 2 is too low for Cloud SQL/RDS latency — connections held longer during network round trips. Confirmed in v1 testing: 3 caused pool exhaustion under test load.

collector:
  interval_seconds: 60
  batch_size: 1000

analyzer:
  interval_seconds: 600
  slow_query_threshold_ms: 1000
  seq_scan_min_rows: 100000
  unused_index_window_days: 30
  index_bloat_threshold_pct: 30
  table_bloat_dead_tuple_pct: 20
  table_bloat_min_rows: 1000       # don't flag bloat on tables smaller than this (FIX-2)
  idle_in_transaction_timeout_minutes: 30
  cache_hit_ratio_warning: 0.95
  xid_wraparound_warning: 500000000
  xid_wraparound_critical: 1000000000
  regression_threshold_pct: 50
  regression_lookback_days: 7
  checkpoint_frequency_warning_per_hour: 12
  plan_time_ratio_warning: 2.0     # flag when mean_plan_time > mean_exec_time × this
  plan_time_min_ms: 100            # don't flag plan time below this absolute floor

safety:
  cpu_ceiling_pct: 90              # based on active_backends / max_connections
  query_timeout_ms: 500            # collector/analyzer queries
  ddl_timeout_seconds: 300         # Tier 3 DDL (CREATE INDEX etc)
  disk_pressure_threshold_pct: 5
  backoff_consecutive_skips: 3
  dormant_interval_seconds: 600

trust:
  level: observation               # observation | advisory | autonomous
  ramp_start: ""                   # persisted to sage.config on first startup; read from DB on restart
  maintenance_window: ""           # cron with TZ: "0 2 * * 6 America/Chicago"
  tier3_safe: true                 # drop unused/invalid indexes, kill idle sessions, vacuum, analyze
  tier3_moderate: false            # create indexes, reindex, autovacuum tune — needs maintenance window
  tier3_high_risk: false           # always false in standalone (VACUUM FULL, schema changes)
  rollback_threshold_pct: 10
  rollback_window_minutes: 15
  rollback_cooldown_days: 7

llm:
  enabled: false                   # DEFAULT OFF. Enable in config.yaml or via SAGE_LLM_ENABLED=true env var.
                                   # Unit tests run with enabled=false. Integration tests set enabled=true + SAGE_GEMINI_API_KEY.
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  api_key: ${SAGE_GEMINI_API_KEY}  # Gemini API key from Google AI Studio
  model: "gemini-2.5-flash"
  timeout_seconds: 60
  token_budget_daily: 100000
  context_budget_tokens: 4096
  cooldown_seconds: 300
  index_optimizer:
    enabled: true                  # requires llm.enabled=true as prerequisite
    min_query_calls: 100           # ignore queries called fewer times
    max_indexes_per_table: 3       # max new indexes per table per executor cycle
    max_include_columns: 5         # cap on INCLUDE column count
    over_indexed_ratio: 1.0        # index_count / column_count ceiling
    write_heavy_ratio: 0.5         # below this R/W ratio, require LLM justification

briefing:
  schedule: "0 6 * * *"
  channels: ["stdout"]
  slack_webhook_url: ""

retention:
  snapshots_days: 90
  findings_days: 180
  actions_days: 365
  explains_days: 90

mcp:
  enabled: true
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

**Config hot-reload:** Watch `config.yaml` via fsnotify. Apply hot-reloadable values (intervals, thresholds, trust level, LLM settings) without restart. Non-hot-reloadable (postgres connection, listen addresses) require restart. Log changed values on reload.

**SAGE_DATABASE_URL support:** If the environment variable `SAGE_DATABASE_URL` is set (e.g. `postgres://sage_agent:pw@10.0.0.5:5432/mydb?sslmode=require`), it overrides ALL individual `postgres.*` config fields. This provides backwards compatibility with v1 test infrastructure. Precedence: `SAGE_DATABASE_URL` > individual CLI flags > individual env vars > YAML > defaults.

#### 1.2 Startup prerequisite checks

On startup in standalone mode, before doing anything else:

```sql
-- 1. Can we connect?
SELECT 1;

-- 2. PostgreSQL version >= 14?
SELECT current_setting('server_version_num')::int;
-- < 140000 → FATAL "PostgreSQL 14+ required. Detected: X". Exit 1.
-- Store version int for feature gating:
--   PG14: baseline
--   PG16: +pg_stat_io, +waitstart in pg_locks
--   PG17: +MAINTAIN privilege

-- 3. Is pg_stat_statements installed?
SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements';
-- Missing → FATAL "pg_stat_statements not installed. Run: CREATE EXTENSION pg_stat_statements;
-- Ensure shared_preload_libraries includes pg_stat_statements (requires restart)." Exit 1.

-- 4. Can we read it?
SELECT 1 FROM pg_stat_statements LIMIT 1;
-- Permission denied → FATAL "Cannot read pg_stat_statements. Run: GRANT pg_monitor TO sage_agent;" Exit 1.

-- 5. Is query text visible?
SELECT query FROM pg_stat_statements WHERE query IS NOT NULL LIMIT 1;
-- All NULL → WARN "Query text NULL. Run: GRANT pg_read_all_stats TO sage_agent;" Continue degraded.

-- 6. Check pg_stat_statements column availability
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'pg_catalog' AND table_name = 'pg_stat_statements' AND column_name = 'wal_records';
-- Store boolean: has_wal_columns. Used by collector to conditionally include wal_records/wal_fpi/wal_bytes.

-- 7. Check total_plan_time availability
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'pg_catalog' AND table_name = 'pg_stat_statements' AND column_name = 'total_plan_time';
-- Store boolean: has_plan_time_columns. Available in pg_stat_statements >= 1.8 (PG14+).
-- If false, omit total_plan_time/mean_plan_time from query collector and skip rule 3.16.
```

#### 1.3 Schema bootstrap

AFTER prerequisite checks:

1. **Take advisory lock FIRST** — before schema creation to prevent two sidecars racing:
   ```sql
   SELECT pg_try_advisory_lock(hashtext('pg_sage'))
   ```
   Returns false → FATAL "Another pg_sage instance holds the advisory lock on this database." Exit 1.

2. Check schema exists: `SELECT 1 FROM information_schema.schemata WHERE schema_name = 'sage'`

3. If missing → create schema + all tables + all indexes in a single transaction. Use the DDL from the spec's Schema Reference, PLUS these indexes for sage's own query patterns:

   ```sql
   -- Spec-defined indexes
   CREATE INDEX idx_snapshots_time ON sage.snapshots (collected_at DESC);
   CREATE INDEX idx_snapshots_category ON sage.snapshots (category, collected_at DESC);
   CREATE INDEX idx_findings_status ON sage.findings (status, severity, last_seen DESC);
   CREATE INDEX idx_findings_object ON sage.findings (object_identifier, category);
   CREATE INDEX idx_action_log_time ON sage.action_log (executed_at DESC);
   CREATE INDEX idx_explain_queryid ON sage.explain_cache (queryid, captured_at DESC);

   -- Additional indexes for sage's own query patterns (NOT in original spec — added from v1 test learnings)
   CREATE INDEX idx_action_log_finding ON sage.action_log (finding_id, outcome, executed_at DESC);
     -- Used by: executor hysteresis check (WHERE finding_id=$1 AND outcome='rolled_back' AND executed_at > $2)
   CREATE INDEX idx_mcp_log_client ON sage.mcp_log (client_id, requested_at DESC);
     -- Used by: MCP rate limiter (WHERE client_id=$1 AND requested_at > now() - interval '1 minute')
   CREATE INDEX idx_findings_category_status ON sage.findings (category, object_identifier) WHERE status = 'open';
     -- Used by: findings dedup (WHERE category=$1 AND object_identifier=$2 AND status='open')
     -- Partial index: only open findings matter for dedup. Resolved findings don't participate.
   ```

   **Design principle:** Sage's own schema must follow the same practices sage recommends. If sage tells users to index FK columns, sage's own FKs must be indexed. If sage recommends partial indexes for status-filtered queries, sage's own dedup query should use one. This is table stakes for credibility — the Hacker News crowd will check.

4. If exists → verify expected tables exist and have expected columns. Missing columns = run migration. Missing tables = create them. Never DROP existing tables.

5. Persist `trust.ramp_start` to `sage.config`. On first-ever startup, `INSERT` with `now()`. On subsequent starts, `SELECT value FROM sage.config WHERE key = 'trust_ramp_start'` — use that value, ignore YAML.

**Required grants (run once as postgres/cloudsqlsuperuser):**
```sql
CREATE USER sage_agent WITH PASSWORD '...';
GRANT CONNECT ON DATABASE mydb TO sage_agent;
GRANT CREATE ON DATABASE mydb TO sage_agent;  -- for schema creation
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;        -- for query text visibility
-- Tier 3 (optional):
GRANT CREATE ON SCHEMA public TO sage_agent;  -- for CREATE INDEX
GRANT pg_signal_backend TO sage_agent;        -- for pg_terminate_backend
```

#### 1.4 Connection pool

Use `pgxpool` from `jackc/pgx`. Pool size = `config.postgres.max_connections` (default: 5).

**Why 5, not 2:** Cloud SQL, RDS, Aurora, AlloyDB, and Azure all add network latency to every query. Connections are held ~10-50x longer than localhost. v1 testing confirmed pool exhaustion at `MaxConns=3` when an SSE session goroutine + handler queries competed for connections. 5 provides headroom for: 1 collector query, 1 analyzer query, 1 executor DDL, 1 MCP handler, 1 spare. If users set this to 2 on a managed service, they will see "connection pool exhausted" errors under normal operation.

**Statement timeout:** Use `SET statement_timeout` (session-level), NOT `SET LOCAL` (which requires a transaction). Apply via `pgxpool.AfterConnect` callback for the default timeout. Override per-query when needed.

**CRITICAL: `CREATE INDEX CONCURRENTLY` cannot run inside a transaction.** pgx `pool.BeginTx()` wraps in `BEGIN/COMMIT` — CONCURRENTLY fails inside that. For all CONCURRENTLY operations, acquire a raw `pgx.Conn` from the pool via `pool.Acquire()`, then use `conn.Exec()` directly. Same for `DROP INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY`.

Non-CONCURRENTLY DDL (`ALTER TABLE SET (...)`, `VACUUM`, `ANALYZE`) can and should run in transactions for atomicity.

---

### Phase 2: Collector

Goroutine on `time.Ticker` at `config.collector.interval_seconds`.

Each cycle: circuit breaker check → collect all categories → single-transaction insert.

#### 2.1 Query statistics
```sql
SELECT queryid, dbid, userid, query, calls,
       total_plan_time, mean_plan_time,
       total_exec_time, mean_exec_time,
       min_exec_time, max_exec_time, stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
       temp_blks_read, temp_blks_written, blk_read_time, blk_write_time
       -- conditionally append: , wal_records, wal_fpi, wal_bytes  (if has_wal_columns)
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL
ORDER BY total_exec_time DESC
LIMIT 500;
```

**Conditionally include `total_plan_time, mean_plan_time`:** Only if `has_plan_time_columns` (checked at startup). If false, omit those columns from the SELECT. The Go struct should use `*float64` (pointer) for these fields so nil = not available. Rule 3.16 (high planning time) must check for nil before comparing.

**Added from v1 learnings:** `total_plan_time` and `mean_plan_time` (available PG14+ in pg_stat_statements v1.8+). `AND queryid IS NOT NULL` filter (v1 Bug #1: NULL queryid rows crash the scanner). Use `COALESCE(queryid, 0)` as defense-in-depth if the filter somehow misses a NULL.

Store as `category = 'queries'`.

#### 2.2 Table statistics
```sql
SELECT schemaname, relname, seq_scan, seq_tup_read, idx_scan, idx_tup_fetch,
       n_tup_ins, n_tup_upd, n_tup_del, n_tup_hot_upd, n_live_tup, n_dead_tup,
       last_vacuum, last_autovacuum, last_analyze, last_autoanalyze,
       vacuum_count, autovacuum_count, analyze_count, autoanalyze_count,
       pg_total_relation_size(relid) AS total_bytes,
       pg_table_size(relid) AS table_bytes,
       pg_indexes_size(relid) AS index_bytes
FROM pg_stat_user_tables
WHERE schemaname NOT IN ('sage', 'pg_catalog', 'information_schema')
  AND (schemaname, relname) > ($1, $2)
ORDER BY schemaname, relname
LIMIT $3;
```
**Keyset pagination** using composite `(schemaname, relname)` — NOT just `relname`. Multi-schema databases need both columns. `$1` = last schemaname, `$2` = last relname (both empty string on first/reset). `$3` = `batch_size`. When fewer rows returned than batch_size, reset both to empty. Track in-memory only.

Store as `category = 'tables'`.

#### 2.2b Table DDL (for LLM Index Optimizer context)

Collected once per analyzer cycle (not every collector cycle — DDL changes rarely). Run during the ANALYZER phase, only for tables that have index-related findings:

```sql
-- For each table with index findings, fetch its DDL:
SELECT
    c.relname AS table_name,
    n.nspname AS schema_name,
    array_agg(
        a.attname || ' ' || pg_catalog.format_type(a.atttypid, a.atttypmod) ||
        CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
        ORDER BY a.attnum
    ) AS columns
FROM pg_class c
JOIN pg_namespace n ON c.relnamespace = n.oid
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE n.nspname = $1 AND c.relname = $2
GROUP BY c.relname, n.nspname;
```

This produces a column list that can be formatted into `CREATE TABLE` DDL. Do NOT query this for every table every cycle — only for tables the index optimizer needs (those with index-related findings).

**Alternative (simpler, slightly less precise):**
```sql
SELECT column_name, data_type, is_nullable, column_default
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position;
```

Store per-table, in-memory during the optimizer phase. Not persisted to snapshots.

#### 2.3 Index statistics
```sql
SELECT s.schemaname, s.relname, s.indexrelname, s.idx_scan, s.idx_tup_read, s.idx_tup_fetch,
       pg_relation_size(s.indexrelid) AS index_bytes,
       i.indisunique, i.indisprimary, i.indisvalid,
       pg_get_indexdef(s.indexrelid) AS indexdef,
       am.amname AS index_type
FROM pg_stat_user_indexes s
JOIN pg_index i ON s.indexrelid = i.indexrelid
JOIN pg_class c ON s.indexrelid = c.oid
JOIN pg_am am ON c.relam = am.oid
WHERE s.schemaname != 'sage'
ORDER BY pg_relation_size(s.indexrelid) DESC;
```
**Added:** `indisvalid` (detects failed CONCURRENTLY builds), `amname` (needed for duplicate detection — only compare btree).

Store as `category = 'indexes'`.

#### 2.4 Foreign key data
```sql
SELECT conrelid::regclass AS table_name, confrelid::regclass AS referenced_table,
       a.attname AS fk_column, conname AS constraint_name
FROM pg_constraint c
JOIN pg_attribute a ON a.attnum = ANY(c.conkey) AND a.attrelid = c.conrelid
JOIN pg_namespace n ON c.connamespace = n.oid
WHERE c.contype = 'f'
  AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema');
```
**Schema exclusion (FIX-1 from v1 test):** Filters out sage's own FK constraints. Without this, sage generates findings about its own `sage.findings(action_log_id)` FK — which appeared on all 4 PG versions in v1 testing.

Required for missing FK index rule. Store as `category = 'foreign_keys'`.

#### 2.4b Partition inheritance mapping
```sql
SELECT inhrelid::regclass AS child_table, inhparent::regclass AS parent_table
FROM pg_inherits
JOIN pg_class c ON c.oid = inhparent
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema');
```
Store as `category = 'partitions'`. Used by analyzer to aggregate child table stats under parent.

**Partition aggregation method:** For partitioned tables, `pg_stat_user_tables` shows the parent with all-zero stats and each child partition with real numbers. The analyzer must:
1. Build a `parent → [child1, child2, ...]` map from the partitions snapshot
2. For each parent, SUM these columns across all children: `seq_scan`, `seq_tup_read`, `idx_scan`, `idx_tup_fetch`, `n_tup_ins`, `n_tup_upd`, `n_tup_del`, `n_live_tup`, `n_dead_tup`, `total_bytes`, `table_bytes`, `index_bytes`
3. For timestamp columns (`last_vacuum`, `last_autovacuum`, etc.), use the MAX (most recent) across children
4. Present aggregated stats under the parent table name for all rules
5. Suppress individual child findings UNLESS a child is a significant outlier (e.g., one partition has 90% of the dead tuples)
6. If a table has no entries in `pg_inherits`, it's not partitioned — use its stats as-is

#### 2.4c IO statistics (PG16+ only)
```sql
-- Only run if pgVersionNum >= 160000
SELECT backend_type, object, context,
       reads, read_time, writes, write_time,
       extends, extend_time, fsyncs, fsync_time
FROM pg_stat_io
WHERE backend_type IN ('client backend', 'autovacuum worker', 'background writer', 'checkpointer');
```
Store as `category = 'io'`. On PG14/15, skip this query. No error.

#### 2.5 System statistics

**Two versions — select based on pgVersionNum:**

PG14-16:
```sql
SELECT
  (SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND pid != pg_backend_pid()) AS active_backends,
  (SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction') AS idle_in_transaction,
  (SELECT count(*) FROM pg_stat_activity) AS total_backends,
  (SELECT setting::int FROM pg_settings WHERE name = 'max_connections') AS max_connections,
  (SELECT blks_hit::float / nullif(blks_hit + blks_read, 0) FROM pg_stat_database WHERE datname = current_database()) AS cache_hit_ratio,
  (SELECT COALESCE(deadlocks, 0) FROM pg_stat_database WHERE datname = current_database()) AS deadlocks,
  (SELECT checkpoints_timed + checkpoints_req FROM pg_stat_bgwriter) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica,
  (SELECT pg_database_size(current_database())) AS db_size_bytes;
```

PG17+ (`pg_stat_bgwriter` → `pg_stat_checkpointer`, columns renamed):
```sql
SELECT
  (SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND pid != pg_backend_pid()) AS active_backends,
  (SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction') AS idle_in_transaction,
  (SELECT count(*) FROM pg_stat_activity) AS total_backends,
  (SELECT setting::int FROM pg_settings WHERE name = 'max_connections') AS max_connections,
  (SELECT blks_hit::float / nullif(blks_hit + blks_read, 0) FROM pg_stat_database WHERE datname = current_database()) AS cache_hit_ratio,
  (SELECT COALESCE(deadlocks, 0) FROM pg_stat_database WHERE datname = current_database()) AS deadlocks,
  (SELECT num_timed + num_requested FROM pg_stat_checkpointer) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica,
  (SELECT pg_database_size(current_database())) AS db_size_bytes;
```

**CRITICAL (v1 Bug #2):** Use `pgVersionNum >= 170000` to select the correct query. PG17 crashes on `pg_stat_bgwriter.checkpoints_timed`.

Store as `category = 'system'`.

#### 2.6 Lock statistics
```sql
SELECT l.locktype, l.mode, l.granted,
       c.relname, a.query, a.state, a.wait_event_type, a.wait_event,
       a.pid, a.backend_start, a.query_start
FROM pg_locks l
LEFT JOIN pg_class c ON l.relation = c.oid
LEFT JOIN pg_stat_activity a ON l.pid = a.pid
WHERE NOT l.granted
ORDER BY a.query_start NULLS LAST
LIMIT 100;
```
**No `l.waitstart`** — added in PG16, absent on PG14/15. Sort by `a.query_start` instead.

Store as `category = 'locks'`.

#### 2.7 Sequence statistics
```sql
SELECT schemaname, sequencename, data_type, last_value, max_value, increment_by,
       CASE
         WHEN increment_by > 0 AND max_value > 0 THEN round((last_value::numeric / max_value::numeric) * 100, 2)
         WHEN increment_by < 0 THEN round(((max_value::numeric - last_value::numeric) / max_value::numeric) * 100, 2)
         ELSE 0
       END AS pct_used
FROM pg_sequences WHERE schemaname NOT IN ('pg_catalog', 'information_schema', 'sage');
```
Handles descending sequences. Store as `category = 'sequences'`.

#### 2.8 Replication statistics
Only collect if `is_replica = false` (from system snapshot):
```sql
SELECT client_addr, state, sent_lsn, write_lsn, flush_lsn, replay_lsn,
       write_lag, flush_lag, replay_lag, sync_state
FROM pg_stat_replication;
```
Replication slots — **two versions:**
```sql
-- Primary: includes retained_bytes
SELECT slot_name, slot_type, active, xmin, catalog_xmin,
       pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS retained_bytes
FROM pg_replication_slots;

-- Replica: pg_current_wal_lsn() fails in recovery
SELECT slot_name, slot_type, active, xmin, catalog_xmin
FROM pg_replication_slots;
```
Store as `category = 'replication'`.

#### 2.9 Circuit breaker
```sql
SELECT count(*) FILTER (WHERE state = 'active' AND pid != pg_backend_pid())::float /
       nullif((SELECT setting::int FROM pg_settings WHERE name = 'max_connections'), 0) AS load_ratio
FROM pg_stat_activity;
```
If `load_ratio > cpu_ceiling_pct / 100.0`, skip. After `backoff_consecutive_skips` skips → dormant mode. Resume after 3 consecutive successful cycles. Track state in memory.

#### 2.10 Snapshot insertion
Single transaction, explicit timestamp shared across all rows:
```sql
INSERT INTO sage.snapshots (collected_at, category, data)
VALUES ($1, 'queries', $2::jsonb), ($1, 'tables', $3::jsonb), ($1, 'indexes', $4::jsonb),
       ($1, 'foreign_keys', $5::jsonb), ($1, 'partitions', $6::jsonb),
       ($1, 'system', $7::jsonb), ($1, 'locks', $8::jsonb),
       ($1, 'sequences', $9::jsonb), ($1, 'replication', $10::jsonb)
       -- conditionally: , ($1, 'io', $11::jsonb)  (PG16+ only)
       ;
```
**Query count per cycle: ~13** (1 breaker + 10 collectors [+1 if PG16 io] + 1 insert). Target <15.

---

### Phase 3: Rules Engine (Analyzer)

Goroutine on `time.Ticker` at `config.analyzer.interval_seconds`.

Each cycle:
1. Load latest snapshot: `SELECT category, data FROM sage.snapshots WHERE collected_at = (SELECT max(collected_at) FROM sage.snapshots)`
2. Load previous snapshot (for delta-based rules): `SELECT category, data FROM sage.snapshots WHERE collected_at = (SELECT max(collected_at) FROM sage.snapshots WHERE collected_at < (SELECT max(collected_at) FROM sage.snapshots))`
   - **First cycle:** Previous snapshot is nil. Delta-based rules (3.8 regression, 3.11-3.15 checkpoint pressure, 3.17 reset detection) must check for nil previous and skip gracefully. Log "first cycle, skipping delta rules."
   - **Keep previous in memory** between cycles to avoid the nested subquery. After each cycle, current becomes previous.
3. Deserialize into Go structs in memory
4. Run all rules
5. Write/update findings

The analyzer does NOT query catalog views directly (except XID wraparound and connection leak detection). It works on snapshot data. Query budget: 3-5 queries per cycle.

Rule signature:
```go
type Rule func(current *Snapshot, previous *Snapshot, history []Snapshot, config *Config) []Finding
// previous may be nil on first cycle — every rule MUST handle this
```

#### 3.1 Unused index detection
`idx_scan = 0` AND NOT primary AND NOT unique AND `indisvalid = true`.

**Index age:** Maintain `first_seen:<schema>.<indexname>` entries in `sage.config`. Create on first observation. Only flag if `now() - first_seen > unused_index_window_days`. Clean up entries for dropped indexes in Phase 7 retention.

Finding: `recommended_sql = 'DROP INDEX CONCURRENTLY IF EXISTS <schema>.<name>;'`, `rollback_sql = '<full indexdef>;'`, `action_risk = 'safe'`.

#### 3.2 Invalid index detection
`indisvalid = false`. Leftover from failed `CREATE INDEX CONCURRENTLY`.
Finding: `recommended_sql = 'DROP INDEX CONCURRENTLY IF EXISTS <schema>.<name>;'`, `action_risk = 'safe'`, `severity = 'warning'`.

#### 3.3 Duplicate index detection
Only btree (`index_type = 'btree'`). Parse `indexdef` to extract ordered column list, WHERE clause, INCLUDE columns. Use regex:
```
CREATE INDEX .+ ON .+ USING btree \((.+)\)(?:\s+INCLUDE\s+\((.+)\))?(?:\s+WHERE\s+(.+))?$
```
Exact duplicate: same columns + WHERE + INCLUDE on same table. Subset: A's columns are leading prefix of B's with same WHERE.
Do NOT compare: different sort order (ASC/DESC), different nulls positioning, different opclass.

#### 3.4 Missing index suggestions
a) **Unindexed foreign keys:** FK columns from `foreign_keys` snapshot with no matching index. `action_risk = 'moderate'`.
b) **High seq_scan tables:** `seq_scan > 100 AND n_live_tup > seq_scan_min_rows AND (idx_scan = 0 OR seq_scan > idx_scan * 10)`. Advisory only (no DDL — can't determine columns).
c) Deduplicate with rule 3.9 (seq scan watchdog).

#### 3.5 Table bloat
`dead_ratio = n_dead_tup / max(n_live_tup + n_dead_tup, 1)`. Flag if > `table_bloat_dead_tuple_pct / 100`. `recommended_sql = 'VACUUM <table>;'`, `rollback_sql = ''`, `action_risk = 'safe'`.

#### 3.6 XID wraparound
Additional analyzer query: `SELECT age(datfrozenxid) FROM pg_database WHERE datname = current_database();`
Warning at `xid_wraparound_warning`, critical at `xid_wraparound_critical`. Advisory only.

#### 3.7 Slow query detection
`mean_exec_time > slow_query_threshold_ms`. Severity: warning if 2-5x, critical if >5x.

#### 3.8 Query regression detection
Sample ~100 historical snapshots:
```sql
SELECT data FROM sage.snapshots WHERE category = 'queries'
  AND collected_at > now() - make_interval(days => $1)
ORDER BY collected_at DESC;
```
Then downsample in Go to ~100 points. Build `queryid → avg(mean_exec_time)` map. Flag if current > historical × (1 + regression_threshold_pct / 100).

**First cycle:** No history → skip rule. Log this.

#### 3.9 Sequential scan watchdog
Same criteria as 3.4b. Skip if 3.4 already flagged the same table.

#### 3.10 Connection leak detection
Run targeted query in analyzer:
```sql
SELECT pid, usename, application_name, state, now() - state_change AS idle_duration
FROM pg_stat_activity
WHERE state = 'idle in transaction'
  AND now() - state_change > make_interval(mins => $1)
  AND pid != pg_backend_pid();
```
**`idle in transaction` only** — not plain `idle` (which is normal).
`recommended_sql = 'SELECT pg_terminate_backend(<pid>);'`, `rollback_sql = ''`, `action_risk = 'safe'`.

#### 3.11–3.15: Sequence exhaustion, cache ratio, checkpoint pressure, replication lag, inactive slots
As described in original. **Checkpoint pressure:** flag if delta > `checkpoint_frequency_warning_per_hour`. First cycle with no previous snapshot → skip.

#### 3.16 High planning time detection (FIX-3 from v1 test)

Flag queries where `mean_plan_time > mean_exec_time * 2` AND `mean_plan_time > 100ms`. This catches planning-dominated queries (complex multi-way joins, queries hitting many partitions).

Category: `high_plan_time`. Severity: warning if plan_time > 2x exec_time, critical if > 10x.
Advisory only — no DDL recommendation. The finding should include the plan-to-exec ratio and suggest EXPLAIN ANALYZE for investigation.

#### 3.17 pg_stat_statements reset detection (FIX-4 from v1 test)

Compare total `calls` across all queries between current and previous snapshot. If total calls drops by >90% in a single cycle AND more than 10 queries were in the previous snapshot, a reset likely happened.

When detected:
- Log WARNING: "pg_stat_statements appears to have been reset. Skipping regression analysis this cycle."
- Skip rule 3.8 (regression detection) for this cycle
- Do NOT resolve existing slow query findings
- Generate an info-level finding: "pg_stat_statements was reset at approximately [time]. Historical baselines will rebuild over the next [regression_lookback_days] days."

#### 3.18 Table bloat minimum row threshold (FIX-2 from v1 test)

In rule 3.5 (table bloat), add a minimum row check: do NOT generate bloat findings for tables where `n_live_tup + n_dead_tup < config.analyzer.table_bloat_min_rows` (default: 1000).

The `nation` table (25 rows, 80% dead tuples) triggered a false positive in v1 testing on all 4 PG versions. Autovacuum's default threshold (50 rows) is higher than the table's row count, so the dead tuples are expected and harmless.

#### Schema exclusion (applies to ALL rules)

ALL analyzer rules must skip objects in the `sage` schema. Check this in the rule registry's execution wrapper, not in each individual rule:

```go
func (a *Analyzer) runRules(snapshot *Snapshot, ...) []Finding {
    // Filter snapshot data to exclude sage schema BEFORE passing to rules
    snapshot.Tables = filterSchema(snapshot.Tables, "sage")
    snapshot.Indexes = filterSchema(snapshot.Indexes, "sage")
    snapshot.ForeignKeys = filterSchema(snapshot.ForeignKeys, "sage")
    // Then run all rules against the filtered snapshot
}
```

This ensures no rule, present or future, generates findings about sage's own schema.

#### Findings dedup and resolution
Dedup: `SELECT id FROM sage.findings WHERE category = $1 AND object_identifier = $2 AND status = 'open'`. If exists → update `last_seen`, increment `occurrence_count`. If not → insert.

Resolution: after all rules, check open findings whose conditions have cleared. Set `status = 'resolved', resolved_at = now()`.

---

### Phase 4: HA / Replica Awareness

From system snapshot: `is_replica` boolean.

If replica: suppress Tier 3, tag findings `"context": "replica"`, skip vacuum recs, skip `pg_current_wal_lsn()` queries.

**Role flip detection:** If `is_replica` changes between cycles → WARN log, reset executor state, re-verify advisory lock.

---

### Phase 4b: LLM Index Optimization (Tier 2 — Requires LLM)

This phase runs AFTER the rules engine and BEFORE the executor. It takes the raw index-related findings from Tier 1 (missing FK indexes, seq scan advisories, unused indexes) and feeds them to the LLM along with the full index landscape and top query patterns for each affected table. The LLM returns consolidated, workload-aware index recommendations that replace the mechanical Tier 1 suggestions.

**When LLM is disabled:** This phase is skipped entirely. The executor acts on raw Tier 1 findings. This is fine — Tier 1 recommendations (FK indexes, unused index drops) are mechanically sound. The LLM adds optimization intelligence, not correctness.

**When LLM is enabled:** Tier 1 index findings are held from the executor. The LLM optimizer produces refined findings that supersede them. Only the refined findings go to the executor.

#### 4b.1 Per-Table Index Review

For each table that has one or more index-related findings (missing FK, seq scan, unused), assemble this context and send to the LLM:

**Query-to-table mapping (how to identify which queries hit which tables):**

Extract table names from parameterized `pg_stat_statements` query text using regex:
```go
// Match schema-qualified and unqualified table names after FROM, JOIN, UPDATE, DELETE FROM, INTO
tablePattern := regexp.MustCompile(`(?i)(?:FROM|JOIN|UPDATE|DELETE\s+FROM|INTO)\s+(?:ONLY\s+)?(?:(\w+)\.)?(\w+)`)
// For each query in the snapshot, extract referenced tables
// Match against known table names from the tables snapshot (prevents false positives on subquery aliases)
```

**IMPORTANT:** This is heuristic, not exact. CTEs, subqueries, and dynamic SQL can fool the regex. That's acceptable — the LLM receives "best-effort" query context, not a perfect dependency graph. False positive table matches are filtered by cross-referencing against the tables snapshot (if a "table name" doesn't exist in pg_stat_user_tables, skip it). False negatives (missed tables) mean some queries won't be included in the optimizer context — the optimizer still works, just with less information.

**Write rate calculation (from snapshot deltas):**
```go
type WriteRate struct {
    InsertsPerDay float64
    UpdatesPerDay float64
    DeletesPerDay float64
}
// Requires current + previous snapshot (at minimum)
// delta = current.n_tup_ins - previous.n_tup_ins
// rate = delta / (current.collected_at - previous.collected_at) * 86400
```
**First cycle (no previous snapshot):** Write rate is unknown. Set all rates to -1 (sentinel). Include in LLM prompt as "Write rate: unknown (first observation cycle — insufficient history)." The LLM should respond conservatively and not create indexes without write rate data unless the read benefit is overwhelming.

**Assemble context and send to Gemini:**

```
[TABLE]
CREATE TABLE public.orders (
  o_orderkey integer NOT NULL,
  o_custkey integer NOT NULL,
  ...
);
-- DDL reconstructed from information_schema.columns (section 2.2b)

[EXISTING INDEXES]
CREATE INDEX idx_orders_pkey ON orders USING btree (o_orderkey);
CREATE INDEX idx_orders_custkey ON orders USING btree (o_custkey);
-- (all indexes on this table, from the indexes snapshot)

[TOP QUERIES]
-- Top 10 queries by total_exec_time that reference this table (via regex extraction)
-- Include: query text (parameterized), calls, mean_exec_time, mean_plan_time,
--          shared_blks_hit, shared_blks_read, rows, temp_blks_written
queryid=12345: SELECT ... FROM orders WHERE o_custkey = $1 AND o_orderdate > $2 ORDER BY o_orderdate
  calls=45000 mean_exec=12.3ms mean_plan=0.2ms blks_read=1450 rows=23

[TABLE STATS]
n_live_tup=150000 n_dead_tup=3200 seq_scan=45000 idx_scan=120000
n_tup_ins=5000/day n_tup_upd=12000/day n_tup_del=800/day
total_size=45MB table_size=32MB index_size=13MB

[WRITE RATE CONTEXT]
Current index count: 2
Insert rate: 5000/day
Update rate: 12000/day
Estimated write amplification per additional index: ~17000 additional index writes/day

[CURRENT FINDINGS]
- missing_fk_index: o_custkey (FK to customer.c_custkey)
- seq_scan_advisory: 45000 seq scans, 150K rows

[TASK]
Analyze the table's query workload, existing indexes, and write rate.
Recommend the MINIMAL set of index changes that maximizes read performance
while respecting write overhead.

For each recommendation, provide:
1. Exact CREATE INDEX CONCURRENTLY DDL (with schema-qualified table name)
2. Which queries benefit and estimated improvement
3. Write overhead cost (additional writes per INSERT/UPDATE/DELETE)
4. Whether INCLUDE columns would convert any Index Scans to Index Only Scans
5. Whether a composite index can satisfy multiple query patterns
6. Whether any existing indexes become redundant after the new indexes

DO NOT recommend indexes for:
- Queries called < 100 times (ad-hoc, not worth indexing)
- Tables with more indexes than columns (over-indexed)
- Columns with <10 distinct values on tables >100K rows (low selectivity — seq scan may be faster)

IMPORTANT: INCLUDE column upgrades to existing indexes require DROP + CREATE (PostgreSQL does not support ALTER INDEX ADD INCLUDE). Generate both the DROP and CREATE DDL.

Respond in JSON:
{
  "table": "public.orders",
  "create": [
    {
      "ddl": "CREATE INDEX CONCURRENTLY idx_orders_custkey_date ON public.orders (o_custkey, o_orderdate DESC)",
      "replaces": ["idx_orders_custkey"],
      "benefits_queries": [12345, 67890],
      "estimated_read_improvement": "12x for queryid 12345, 3x for 67890",
      "write_cost": "~17000 additional index writes/day",
      "rationale": "Composite covers FK join and range+sort. Existing idx_orders_custkey is strict subset."
    }
  ],
  "drop": [
    {
      "ddl": "DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_custkey",
      "reason": "Strict subset of new idx_orders_custkey_date"
    }
  ],
  "include_upgrades": [
    {
      "drop_ddl": "DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status",
      "create_ddl": "CREATE INDEX CONCURRENTLY idx_orders_status_covering ON public.orders (o_orderstatus) INCLUDE (o_orderkey, o_totalprice)",
      "benefits_queries": [67890],
      "rationale": "Converts Index Scan to Index Only Scan. Requires DROP+CREATE because ALTER INDEX cannot add INCLUDE columns."
    }
  ],
  "no_action": [
    {
      "query": "queryid 99999: SELECT * FROM orders WHERE o_comment LIKE '%test%'",
      "reason": "Called 3 times total. Ad-hoc query, not worth indexing."
    }
  ],
  "index_budget_note": "Table currently has 2 indexes. After changes: 2. Write amplification delta: +15%."
}
```

**Gemini model name (CONFIRMED WORKING):** `gemini-2.5-flash`. The preview model name `gemini-2.5-flash-preview` returns HTTP 404 as of March 2026. Also delisted: `gemini-2.0-flash` ("no longer available to new users"). If the model name changes in the future, update `config.yaml` — no code change needed.

**LLM call sequencing:** Process tables SEQUENTIALLY, one at a time. Do NOT call Gemini in parallel for multiple tables. Add a 1-second `time.Sleep` between calls to stay well within rate limits (Gemini flash allows 1500 req/min but we're being conservative). If the optimizer needs to process 10 tables, it takes ~20 seconds total — well within an analyzer cycle.

**Action ordering in executor:** When the optimizer recommends CREATE + DROP for the same table:
1. CREATE new indexes FIRST (so data is covered during the transition)
2. VERIFY the new indexes are valid (`indisvalid = true`)
3. THEN DROP old indexes
Never drop first — if create fails, the old indexes still protect queries.

#### 4b.2 Index Budget Awareness

Before sending any table to the LLM, compute the index budget context:

```go
type IndexBudget struct {
    CurrentIndexCount  int
    ColumnCount        int
    InsertRate         float64  // per day, from snapshot deltas
    UpdateRate         float64
    DeleteRate         float64
    ReadWriteRatio     float64  // (seq_scan + idx_scan) / (n_tup_ins + n_tup_upd + n_tup_del)
    EstimatedWriteAmpPerIndex float64  // (insert + update + delete) per day
}
```

Include this in the LLM prompt. The LLM must justify write overhead for every recommended index.

**Hard limits (enforced in code, not just LLM guidance):**
- If `CurrentIndexCount >= ColumnCount`: do NOT create new indexes. Flag as "over-indexed table" finding. The LLM should recommend consolidation or drops, not additions.
- If `ReadWriteRatio < 0.5` (write-heavy table): only allow index creation with explicit LLM justification that the read benefit outweighs write cost. Log the justification.
- Maximum 3 new indexes per table per executor cycle. Prevents runaway index proliferation from a single analysis.

#### 4b.3 Cross-Query Index Consolidation

The LLM's core task is finding a minimal index set. Examples of what it should consolidate:

| Separate Tier 1 Findings | LLM Consolidated Recommendation |
|--------------------------|-------------------------------|
| Missing FK: `orders(o_custkey)` + Slow query needs `orders(o_custkey, o_orderdate)` | One composite: `(o_custkey, o_orderdate DESC)` — covers both the FK join and the range+sort pattern |
| Missing FK: `lineitem(l_orderkey)` + Missing FK: `lineitem(l_suppkey)` + Slow 3-way join on both | Two separate indexes (can't combine different FK columns in one useful composite) — but LLM explains WHY they can't be consolidated |
| 5 different queries filtering `orders(o_orderstatus)` with different SELECT lists | One covering index: `(o_orderstatus) INCLUDE (o_orderkey, o_totalprice, o_orderdate)` — covers all 5 patterns |
| Unused index `idx_A(a, b)` + Missing index recommendation for `(a, b, c)` | Drop `idx_A`, create `(a, b, c)` — the new one is a superset |

#### 4b.4 INCLUDE Column Recommendations

The LLM should detect when a query's SELECT list is narrow enough to be covered by adding INCLUDE columns to an existing or recommended index. The criteria:

1. Query does an Index Scan (not Seq Scan) — so an index exists on the WHERE columns
2. Query returns < 6 non-key columns — narrow enough to be worth covering
3. The table has high `shared_blks_read` for this query — heap fetches are expensive
4. Adding the columns as INCLUDE doesn't make the index unreasonably wide

The finding should explain: "This query does N heap fetches per call because columns X, Y are not in the index. Adding INCLUDE (X, Y) converts to an Index Only Scan, eliminating the heap fetches."

**IMPORTANT:** INCLUDE columns do NOT affect index ordering or WHERE clause matching. They only avoid heap fetches. The LLM must understand this distinction — INCLUDE columns go AFTER the key columns, not in the key.

#### 4b.5 Configuration

```yaml
llm:
  index_optimizer:
    enabled: true               # requires llm.enabled=true as prerequisite
    min_query_calls: 100        # don't optimize for queries called fewer times
    max_indexes_per_table: 3    # max new indexes per table per cycle
    max_include_columns: 5      # don't create absurdly wide covering indexes
    over_indexed_ratio: 1.0     # index_count/column_count threshold
    write_heavy_ratio: 0.5      # below this read/write ratio, require justification
```

#### 4b.5b Response Parsing (Go Structs)

```go
type IndexOptimizationResponse struct {
    Table           string                    `json:"table"`
    Create          []IndexCreateRec          `json:"create"`
    Drop            []IndexDropRec            `json:"drop"`
    IncludeUpgrades []IndexIncludeUpgradeRec  `json:"include_upgrades"`
    NoAction        []NoActionItem            `json:"no_action"`
    BudgetNote      string                    `json:"index_budget_note"`
}

type IndexCreateRec struct {
    DDL                    string   `json:"ddl"`            // must start with CREATE INDEX CONCURRENTLY
    Replaces               []string `json:"replaces"`       // index names this supersedes
    BenefitsQueries        []int64  `json:"benefits_queries"`
    EstimatedReadImprovement string `json:"estimated_read_improvement"`
    WriteCost              string   `json:"write_cost"`
    Rationale              string   `json:"rationale"`
}

type IndexDropRec struct {
    DDL    string `json:"ddl"`     // must start with DROP INDEX CONCURRENTLY
    Reason string `json:"reason"`
}

type IndexIncludeUpgradeRec struct {
    DropDDL         string  `json:"drop_ddl"`         // DROP the old index
    CreateDDL       string  `json:"create_ddl"`       // CREATE the new index with INCLUDE
    BenefitsQueries []int64 `json:"benefits_queries"`
    Rationale       string  `json:"rationale"`
}

type NoActionItem struct {
    Query  string `json:"query"`
    Reason string `json:"reason"`
}
```

**Parsing the LLM response:**
1. Extract `choices[0].message.content` from the Gemini response
2. The content is a JSON string. Strip markdown fences if present (LLMs often wrap in ` ```json ... ``` `)
3. `json.Unmarshal` into `IndexOptimizationResponse`
4. Validate all DDL strings (see 4b.7)

**Fallback on parse failure:**
- If JSON parsing fails: log raw response at WARN, increment `pg_sage_llm_parse_failures_total`, skip this table's optimization, let Tier 1 findings pass through unchanged to executor
- If response has zero recommendations (all arrays empty): treat as "LLM reviewed and found nothing to improve" — suppress Tier 1 findings for this table anyway (the LLM decided the existing indexes are fine)
- If circuit breaker is open: skip all tables, Tier 1 findings pass through

#### 4b.6 Finding Format

LLM-optimized index findings use category `index_optimization` (distinct from Tier 1's `missing_fk_index` or `unused_index`). They supersede Tier 1 findings on the same table:

- When an `index_optimization` finding exists for a table, Tier 1 `missing_fk_index` findings for that table are auto-suppressed (status: `superseded`)
- The `detail` JSONB includes the full LLM rationale, affected queryids, write cost estimate, and the consolidation explanation
- `recommended_sql` contains ALL DDL statements separated by `;\n` — CREATEs first, then DROPs. The executor splits on `;\n` and executes in order. Example: `"CREATE INDEX CONCURRENTLY idx_new ON t (a,b);\nDROP INDEX CONCURRENTLY IF EXISTS idx_old"`
- `rollback_sql` contains the reverse: re-CREATE dropped indexes, then DROP newly created ones. Same `;\n` separator.
- The `detail` JSONB also stores the structured `IndexOptimizationResponse` so MCP tools can expose the full rationale

#### 4b.7 Anti-Proliferation Guard (Code-Level, Not LLM)

Even if the LLM recommends something, the executor enforces hard limits:

```go
func (e *Executor) validateIndexAction(finding Finding, table TableStats) error {
    if table.IndexCount + netNewIndexes >= table.ColumnCount {
        return fmt.Errorf("refusing to create index: table %s already has %d indexes on %d columns",
            table.Name, table.IndexCount, table.ColumnCount)
    }
    if netNewIndexes > config.LLM.IndexOptimizer.MaxIndexesPerTable {
        return fmt.Errorf("refusing to create %d indexes in one cycle (max: %d)",
            netNewIndexes, config.LLM.IndexOptimizer.MaxIndexesPerTable)
    }
    return nil
}
```

This is the safety net. The LLM is the brain, but the code is the guardrail.

---

### Phase 5: Action Executor (Tier 3)

Runs after analyzer (and optimizer, if LLM enabled) each cycle. Processes findings with non-empty `recommended_sql`.

**Action ordering for index optimization findings:** When a finding has both CREATE and DROP operations (e.g., "create composite, drop old subset"):
1. Execute all CREATEs first
2. Verify each new index is valid: `SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`
3. If ANY create failed or produced an INVALID index: STOP. Do not execute DROPs. Log error. The old indexes remain to protect queries.
4. Only after all CREATEs are valid: execute DROPs
5. For `include_upgrades`: CREATE the new covering index FIRST, verify valid, THEN drop the old one

#### 5.1 Trust gate
```go
func shouldExecute(finding Finding, config Config, rampStart time.Time, isReplica bool, emergencyStop bool) bool {
    if emergencyStop || isReplica || config.Trust.Level != "autonomous" { return false }
    switch finding.ActionRisk {
    case "safe":
        return config.Trust.Tier3Safe && time.Since(rampStart) >= 8*24*time.Hour
    case "moderate":
        if !config.Trust.Tier3Moderate { return false }
        if time.Since(rampStart) < 31*24*time.Hour { return false }
        if config.Trust.MaintenanceWindow != "" && !inMaintenanceWindow(config.Trust.MaintenanceWindow) { return false }
        return true
    }
    return false
}
```
If `tier3_moderate = true` but no maintenance window → WARN at startup. Moderate actions will not execute.

#### 5.2 Action execution
For each eligible finding:
1. Check hysteresis: `SELECT 1 FROM sage.action_log WHERE finding_id = $1 AND outcome = 'rolled_back' AND executed_at > now() - make_interval(days => $2)`. If exists → skip.
2. Snapshot before-state metrics.
3. Execute DDL on raw `pgx.Conn` (CONCURRENTLY) or in transaction (non-CONCURRENTLY). Override timeout to `ddl_timeout_seconds`.
4. Log to `sage.action_log` with `outcome = 'pending'`.
5. Goroutine: wait `rollback_window_minutes`, re-check metrics. Regression > `rollback_threshold_pct` → execute `rollback_sql`, update log `outcome = 'rolled_back'`. No regression → update `outcome = 'success'`, populate `after_state`.

**Interrupted DDL on reconnect:** After reconnection, check for INVALID indexes not present in the last known snapshot. Log warning. Do NOT auto-retry.

**Non-reversible actions:** VACUUM, ANALYZE, pg_terminate_backend have `rollback_sql = ''`. Skip rollback monitoring for these.

#### 5.3 Emergency stop
Check `sage.config WHERE key = 'emergency_stop'` before every action. Expose via MCP tools `sage_emergency_stop` and `sage_resume`.

#### 5.4 Grant verification at startup
```sql
SELECT has_schema_privilege($1, 'public', 'CREATE');
SELECT pg_has_role($1, 'pg_signal_backend', 'MEMBER');
```
If trust is autonomous and grants missing → WARN with exact fix SQL.

---

### Phase 6: Briefing Worker

Cron-scheduled via `robfig/cron`. Query recent findings + actions. Generate structured text (or LLM-enhanced). Write to `sage.briefings`. Dispatch to stdout / Slack.

---

### Phase 7: Data Retention

After each analyzer cycle. Batched deletes (LIMIT 1000 per statement). Also clean `first_seen:*` entries in `sage.config` for indexes absent from latest snapshot.

---

### Phase 8: MCP and Prometheus

**MCP:** Go-native implementations of `sage_analyze`, `sage_get_recommendations`, `sage_status`, `sage_emergency_stop`, `sage_resume`. `tools/list` reflects mode + trust level. Tier 3 tools listed only when autonomous + enabled. ALTER SYSTEM tools hidden.

**Prometheus:** Add `pg_sage_collector_cycle_duration_seconds`, `pg_sage_collector_skipped_cycles_total`, `pg_sage_collector_dormant`, `pg_sage_analyzer_findings_open` (by severity), `pg_sage_executor_actions_total` (by outcome), `pg_sage_connection_up`, `pg_sage_connection_latency_seconds`, `pg_sage_mode`.

---

### Phase 9: Graceful Reconnection

Exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s cap. Log each attempt. Health endpoint: `"status": "reconnecting"`. On reconnect: re-verify advisory lock (if taken by another instance, exit gracefully), check for interrupted DDL (INVALID indexes), resume.

---

### Phase 10: Graceful Shutdown

SIGTERM/SIGINT: set shutdown flag → all goroutines check each tick → wait for in-flight DDL (up to `ddl_timeout_seconds`) → `SELECT pg_advisory_unlock(hashtext('pg_sage'))` → close pool → exit 0. Double SIGTERM → force exit.

---

## File Structure

```
sidecar/
├── cmd/pg_sage_sidecar/
│   └── main.go                     # CLI, config loading, signal handling
├── internal/
│   ├── config/
│   │   ├── config.go               # YAML + env + CLI merge, precedence
│   │   ├── defaults.go             # Defaults matching spec
│   │   └── watcher.go              # fsnotify hot-reload
│   ├── startup/
│   │   └── checks.go               # Prerequisites (pg_stat_statements, version, grants)
│   ├── collector/
│   │   ├── collector.go            # Loop goroutine
│   │   ├── queries.go              # SQL constants
│   │   ├── queries_compat.go       # PG version-specific variants
│   │   ├── snapshot.go             # Types and DB insertion
│   │   └── circuit_breaker.go      # Load check, skip/dormant
│   ├── analyzer/
│   │   ├── analyzer.go             # Loop goroutine
│   │   ├── rules.go                # Registry, sequential execution
│   │   ├── rules_index.go          # Unused, invalid, duplicate, missing FK
│   │   ├── rules_vacuum.go         # Bloat, dead tuples, XID wraparound
│   │   ├── rules_query.go          # Slow, regression, seq scan
│   │   ├── rules_system.go         # Connections, cache, checkpoints, leaks
│   │   ├── rules_sequence.go       # Exhaustion (ascending + descending)
│   │   ├── rules_replication.go    # Lag, inactive slots
│   │   ├── finding.go              # Types, dedup, resolution, severity
│   │   └── index_parser.go         # pg_get_indexdef() regex parser
│   ├── executor/
│   │   ├── executor.go             # Action loop
│   │   ├── trust.go                # Trust ramp, maintenance window, emergency stop
│   │   ├── rollback.go             # Monitor goroutines, hysteresis
│   │   ├── grants.go               # Startup verification
│   │   └── ddl.go                  # CONCURRENTLY-aware execution helpers
│   ├── schema/
│   │   ├── bootstrap.go            # Advisory lock → schema creation → migration
│   │   └── migrations/001_initial.sql
│   ├── briefing/
│   │   ├── briefing.go             # Cron-scheduled loop
│   │   ├── formatter.go            # Structured text (no-LLM)
│   │   └── channels.go             # Stdout, Slack, table
│   ├── llm/
│   │   ├── client.go               # OpenAI-compatible HTTP
│   │   ├── circuit_breaker.go      # Retries, cooldown
│   │   ├── context.go              # Context assembly (spec 2.4)
│   │   └── index_optimizer.go      # Tier 2: cross-query consolidation, INCLUDE, budget
│   ├── ha/
│   │   └── ha.go                   # Recovery check, role flip detection
│   ├── retention/
│   │   └── cleanup.go              # Batched deletes, first_seen cleanup
│   ├── mcp/                        # EXISTING — add mode + trust awareness
│   └── prometheus/                 # EXISTING — add standalone metrics
├── config.example.yaml
├── go.mod
├── go.sum
└── Dockerfile
```

---

## Testing Strategy

### V1 Test Results (27/27 PASS — but 9 of 12 packages untested)

v1 testing (2026-03-22) ran 27 tests against a live Cloud SQL instance with Gemini connected. All passed. But coverage is concentrated in 3 packages:

| Package | Tests | Status | Risk |
|---------|-------|--------|------|
| `sidecar` (MCP/SSE/Prometheus) | 11 | ✅ TESTED against Cloud SQL | Low |
| `internal/analyzer` | 5 | ✅ TESTED (unit, fixtures) | Medium |
| `internal/llm` (client + optimizer) | 11 | ✅ TESTED (mock HTTP server) | Medium |
| `internal/collector` | 0 | ❌ **NONE** | **CRITICAL** |
| `internal/executor` | 0 | ❌ **NONE** | **CRITICAL** |
| `internal/schema` | 0 | ❌ **NONE** | **HIGH** |
| `internal/config` | 0 | ❌ **NONE** | Medium |
| `internal/startup` | 0 | ❌ **NONE** | Medium |
| `internal/briefing` | 0 | ❌ **NONE** | Low |
| `internal/ha` | 0 | ❌ **NONE** | Medium |
| `internal/retention` | 0 | ❌ **NONE** | Low |
| Integration (end-to-end) | 0 | ❌ **NOT IMPLEMENTED** | **CRITICAL** |

**v1 fixes applied during testing:**
- `poolCfg.MaxConns` 3 → 5 (Cloud SQL latency caused pool exhaustion)
- Gemini model: `gemini-2.5-flash-preview` → `gemini-2.5-flash` (preview returns 404)

### PRIORITY 0: Critical untested packages (write BEFORE v2 adversarial tests)

#### `internal/collector` tests — `collector_test.go`

The collector feeds everything. If it's broken, analyzer/executor/optimizer all produce garbage.

```go
// Unit tests with fixture data (no DB required):
func TestQueryStatsSQL_NullQueryidFiltered(t *testing.T)
    // Fixture: snapshot with queryid=NULL rows
    // Assert: NULL rows excluded from parsed output
    // Validates: v1 Bug #1 fix (AND queryid IS NOT NULL)

func TestQueryStatsSQL_PlanTimeConditional(t *testing.T)
    // Fixture: has_plan_time_columns=true → includes total_plan_time, mean_plan_time
    // Fixture: has_plan_time_columns=false → fields are nil (*float64 pointer)

func TestSystemStatsSQL_PG17Checkpointer(t *testing.T)
    // Fixture: pgVersionNum=170000
    // Assert: query uses pg_stat_checkpointer (not pg_stat_bgwriter)
    // Validates: v1 Bug #2 fix

func TestSystemStatsSQL_PG14Baseline(t *testing.T)
    // Fixture: pgVersionNum=140000
    // Assert: query uses pg_stat_bgwriter, no waitstart column

func TestTableStatsSQL_SchemaExclusion(t *testing.T)
    // Fixture: rows with schemaname='sage', 'public', 'pg_catalog'
    // Assert: only 'public' rows in output
    // Validates: FIX-1 schema exclusion

func TestFKCollector_SchemaExclusion(t *testing.T)
    // Fixture: FK constraints in sage schema and public schema
    // Assert: only public schema FKs returned

func TestIndexCollector_SchemaExclusion(t *testing.T)
    // Fixture: indexes in sage schema and public schema
    // Assert: only public schema indexes returned

func TestSnapshotInsertion_AllCategories(t *testing.T)
    // Fixture: all category data assembled
    // Assert: INSERT statement includes queries, tables, indexes, foreign_keys,
    //         partitions, system, locks, sequences, replication
    // Assert: io category included only when pgVersionNum >= 160000

func TestCoalesceNullableColumns(t *testing.T)
    // Fixture: pg_stat_user_tables rows with NULL last_vacuum, NULL deadlocks
    // Assert: COALESCE converts to zero/epoch, no scan error
    // Validates: v1 Bug #3, #9

func TestKeysetPagination_MultiSchema(t *testing.T)
    // Fixture: tables in schemas 'a_schema' and 'b_schema'
    // Assert: pagination uses (schemaname, relname) composite, not just relname
    // Assert: no tables skipped across schema boundary

func TestCircuitBreaker_SkipAndDormant(t *testing.T)
    // Fixture: load_ratio above cpu_ceiling_pct
    // Assert: collector skips cycle
    // After backoff_consecutive_skips: enters dormant mode
    // After load drops: recovers after 3 consecutive successful cycles
```

#### `internal/executor` tests — `executor_test.go`

The executor does the dangerous stuff. Every code path must be tested.

```go
func TestTrustGate_AllCombinations(t *testing.T)
    // Matrix test: level × ramp timing × per-tier toggles × emergency stop × HA × window
    // At minimum:
    //   observation + any → false
    //   advisory + any → false
    //   autonomous + safe + ramp<8d → false
    //   autonomous + safe + ramp>8d → true
    //   autonomous + moderate + ramp<31d → false
    //   autonomous + moderate + ramp>31d + no window → false (+ WARN)
    //   autonomous + moderate + ramp>31d + in window → true
    //   autonomous + any + emergency_stop → false
    //   autonomous + any + is_replica → false

func TestConcurrentlyOnRawConn(t *testing.T)
    // Verify CREATE INDEX CONCURRENTLY uses pool.Acquire() + conn.Exec()
    // NOT pool.BeginTx() which wraps in BEGIN/COMMIT and fails
    // This can be a code inspection test or a testcontainer test

func TestNonReversibleActionsSkipRollback(t *testing.T)
    // Fixture: finding with rollback_sql = '' (VACUUM, ANALYZE, pg_terminate_backend)
    // Assert: no rollback monitoring goroutine spawned

func TestHysteresis_SkipRecentlyRolledBack(t *testing.T)
    // Fixture: action_log entry with outcome='rolled_back', executed_at=1 day ago
    //          config rollback_cooldown_days=7
    // Assert: same finding_id skipped by executor

func TestActionOrdering_CreateBeforeDrop(t *testing.T)
    // Fixture: index_optimization finding with create + drop DDL
    // Assert: CREATE executes first, then DROP
    // Assert: if CREATE fails (returns INVALID), DROP is NOT executed

func TestEmergencyStop(t *testing.T)
    // Fixture: sage.config has emergency_stop=true
    // Assert: executor returns immediately, no actions processed
    // Then: set emergency_stop=false, verify executor resumes

func TestGrantVerification_MissingGrants(t *testing.T)
    // Fixture: has_schema_privilege returns false
    // Assert: WARN logged with exact fix SQL
    // Assert: executor does not crash, continues in degraded mode

func TestDDLTimeout(t *testing.T)
    // Fixture: ddl_timeout_seconds=5
    // Assert: statement_timeout is set to 5000 before DDL execution
    // Assert: reverted to default after DDL completes
```

#### `internal/schema` tests — `schema_test.go`

```go
func TestAdvisoryLock_FirstInstance(t *testing.T)
    // testcontainer: start PG, connect, call pg_try_advisory_lock
    // Assert: returns true, lock held

func TestAdvisoryLock_SecondInstance(t *testing.T)
    // testcontainer: start PG, first conn takes lock
    // Second conn calls pg_try_advisory_lock
    // Assert: returns false
    // Assert: sidecar exits with FATAL message

func TestBootstrap_FreshDatabase(t *testing.T)
    // testcontainer: start PG with no sage schema
    // Run bootstrap
    // Assert: sage schema exists
    // Assert: all tables exist (snapshots, findings, action_log, explain_cache, briefings, config, mcp_log)
    // Assert: all indexes exist (spec-defined + self-indexes from v1 learnings)
    // Assert: trust_ramp_start persisted in sage.config

func TestBootstrap_IdempotentRestart(t *testing.T)
    // testcontainer: run bootstrap twice
    // Assert: no errors on second run
    // Assert: no duplicate tables or indexes
    // Assert: trust_ramp_start NOT overwritten on second run

func TestBootstrap_MissingColumnsAddedByMigration(t *testing.T)
    // testcontainer: create sage schema with v1 DDL (missing columns)
    // Run bootstrap
    // Assert: missing columns added via migration
    // Assert: existing data preserved
```

#### `internal/config` tests — `config_test.go`

```go
func TestConfigPrecedence_CLIOverEnvOverYAML(t *testing.T)
    // Set YAML host=yaml-host, env SAGE_PG_HOST=env-host, CLI --pg-host=cli-host
    // Assert: final config.Postgres.Host == "cli-host"

func TestConfigPrecedence_DatabaseURL(t *testing.T)
    // Set SAGE_DATABASE_URL=postgres://u:p@dburl:5432/db?sslmode=require
    // Also set YAML host=yaml-host
    // Assert: DATABASE_URL overrides ALL individual postgres fields

func TestConfigHotReload(t *testing.T)
    // Write config.yaml with analyzer.interval_seconds=600
    // Start config watcher
    // Rewrite config.yaml with analyzer.interval_seconds=300
    // Assert: config updated within 2 seconds
    // Assert: non-hot-reloadable values (postgres.host) NOT changed

func TestConfigDefaults(t *testing.T)
    // Load empty config (no YAML, no env, no CLI)
    // Assert: all defaults match spec
    // Assert: llm.enabled == false (unit tests must work without Gemini key)
    // Assert: postgres.max_connections == 5

func TestConfigValidation(t *testing.T)
    // Load config with collector_interval_seconds=0
    // Assert: validation error
    // Load config with trust.level="invalid"
    // Assert: validation error
```

#### `internal/startup` tests — `startup_test.go`

```go
func TestPrereqChecks_NoPgStatStatements(t *testing.T)
    // testcontainer: PG without pg_stat_statements in shared_preload_libraries
    // Assert: FATAL error with clear message including fix SQL

func TestPrereqChecks_PG13Rejected(t *testing.T)
    // testcontainer: PG13
    // Assert: FATAL "PostgreSQL 14+ required"

func TestPrereqChecks_NullQueryText(t *testing.T)
    // testcontainer: user without pg_read_all_stats
    // Assert: WARN (not FATAL), continues in degraded mode

func TestPrereqChecks_PlanTimeColumnDetection(t *testing.T)
    // testcontainer: PG16 with pg_stat_statements
    // Assert: has_plan_time_columns == true
```

### Existing unit tests (CONFIRMED PASSING in v1)

Already implemented and passing:
- `rules_index_test.go`: unused with first_seen (new vs old), invalid, duplicate (exact + subset + non-btree skip), missing FK, **unique index NOT flagged**
- `index_parser_test.go`: partial indexes, INCLUDE, expressions, multi-column, operator classes
- `index_optimizer_test.go`: consolidation, INCLUDE recommendation, over-indexed rejection, write-heavy justification, ad-hoc query skip, malformed response, DDL validation, max per cycle
- `trust_test.go`: all gate combinations (level × ramp × toggles × window × emergency × replica)
- `circuit_breaker_test.go`: skip counting, dormant entry/exit
- `rollback_test.go`: hysteresis, regression detection, non-reversible action skip
- `rules_query_test.go`: slow query threshold, regression with sampled history, **high_plan_time detection**, **reset detection (calls drop >90%)**
- `rules_vacuum_test.go`: dead tuple ratio, XID wraparound, **bloat min_rows threshold (skip tiny tables)**

### LLM client unit tests (CONFIRMED PASSING in v1 — mock HTTP server)

Already implemented and passing:
```go
func TestChat_Success(t *testing.T)            // happy path: content + token parsing ✅
func TestChat_BudgetExhausted(t *testing.T)    // daily token budget enforcement ✅
func TestChat_CircuitBreaker(t *testing.T)     // 3 failures → open ✅
func TestChat_EmptyChoices(t *testing.T)       // {"choices":[]} handled ✅
func TestChat_ServerError(t *testing.T)        // exponential backoff 1s+4s+16s=21s ✅
```

### LLM index optimizer unit tests (CONFIRMED PASSING in v1 — mock HTTP server)

Already implemented and passing:
```go
func TestOptimizer_AnalyzeSuccess(t *testing.T)              // 1 recommendation + token count ✅
func TestOptimizer_SkipWriteHeavy(t *testing.T)              // write ratio >70% → skip ✅
func TestOptimizer_SkipOverIndexed(t *testing.T)             // ≥10 existing indexes → skip ✅
func TestParseRecommendations_WithFences(t *testing.T)       // strips markdown fences ✅
func TestParseRecommendations_EmptyArray(t *testing.T)       // [] parses clean ✅
func TestValidateRecommendation_NoConcurrently(t *testing.T) // rejects missing CONCURRENTLY ✅
```

### STILL NEEDED: Additional LLM tests not yet implemented
```go
func TestClientTimeout(t *testing.T)            // mock hangs 5s, client timeout 2s
func TestClientGarbageResponse(t *testing.T)    // mock returns non-JSON, verify no crash
func TestClientLargeResponse(t *testing.T)      // mock returns 1MB+, verify LimitReader
func TestClientNon200(t *testing.T)             // mock returns 401/429, verify error handling
func TestOptimizerConsolidation(t *testing.T)   // two FK → one composite
func TestOptimizerIncludeColumns(t *testing.T)  // Index Scan → Index Only Scan upgrade
func TestOptimizerMaxPerCycle(t *testing.T)     // 5 recommended → only 3 pass
```

### Integration tests (testcontainers-go, PG16 + REAL Gemini API)

**NOT YET IMPLEMENTED.** Build tag: `//go:build integration`. Requires: `SAGE_GEMINI_API_KEY` env var set.

```go
func TestFullLLMCycleGemini(t *testing.T) {
    // 1. Start PG16 testcontainer with pg_stat_statements
    // 2. Start sidecar standalone mode, llm.enabled=true, Gemini endpoint
    // 3. Create TPC-H schema, load data, run bad queries
    // 4. Wait for collector + analyzer + index optimizer cycle
    // 5. Assert: sage.findings contains index_optimization findings
    // 6. Assert: findings detail JSONB has LLM-generated rationale
    // 7. Assert: recommended_sql uses CONCURRENTLY
    // 8. Assert: Prometheus pg_sage_llm_calls_total > 0
    // 9. Assert: Prometheus pg_sage_llm_tokens_total > 0
    // 10. Assert: sage.briefings has llm_used = true

    // Failure mode tests:
    // 11. Set endpoint to garbage → circuit breaker opens
    // 12. Assert: findings fall back to Tier 1
    // 13. Restore endpoint → circuit breaker closes after cooldown
    // 14. Assert: LLM calls resume
}

func TestFullCycleNoLLM(t *testing.T) {
    // Same as above but llm.enabled=false
    // Verify Tier 1 findings work, no LLM errors, no LLM metrics
}
```

### STILL NEEDED: Reconnection and graceful shutdown tests
```go
func TestReconnection_CloudSQLRestart(t *testing.T)
    // testcontainer: start PG, sidecar connects
    // Stop PG container (simulates Cloud SQL maintenance restart)
    // Wait for exponential backoff (1s, 2s, 4s...)
    // Restart PG container
    // Assert: sidecar reconnects, re-verifies advisory lock
    // Assert: health endpoint shows status=reconnecting during retry, then status=ok
    // Assert: Prometheus pg_sage_connection_up=0 during retry, =1 after

func TestGracefulShutdown_DuringDDL(t *testing.T)
    // testcontainer: start PG, sidecar connects
    // Trigger a slow CREATE INDEX CONCURRENTLY (large table)
    // Send SIGTERM during index build
    // Assert: sidecar waits for DDL to complete (up to ddl_timeout_seconds)
    // Assert: advisory lock released AFTER DDL completes
    // Assert: exit code 0
    // Assert: index is valid (not left in INVALID state)

func TestGracefulShutdown_Idle(t *testing.T)
    // Send SIGTERM when no DDL in flight
    // Assert: advisory lock released immediately
    // Assert: exit code 0 within 2 seconds
```

### PG version matrix
PG14, PG15, PG16, PG17. Verify no SQL errors from version-gated features.

---

## Critical Design Decisions

1. **Advisory lock BEFORE schema creation.** Prevents race between two sidecars starting simultaneously.
2. **Tier 3 autonomous actions supported.** The differentiator. CONCURRENTLY DDL over standard libpq. ALTER SYSTEM is the only thing blocked.
3. **`CREATE INDEX CONCURRENTLY` on raw pgx.Conn, never inside BeginTx.** This is a real footgun — verify in code review.
4. **trust.ramp_start persisted in sage.config table.** Survives sidecar restarts. YAML seed used only on first-ever startup.
5. **Index age tracked via first_seen in sage.config.** Prevents false positives on new indexes.
6. **SET statement_timeout (session-level), not SET LOCAL.** SET LOCAL requires transaction context which pgxpool autocommit doesn't provide.
7. **Snapshot storage in-database.** Survives restarts. MCP works identically in both modes.
8. **No `l.waitstart` in lock queries.** PG16+ only. Use `a.query_start` for PG14 compat.
9. **Descending sequence handling.** pct_used formula accounts for negative increment_by.
10. **Non-reversible actions get `rollback_sql = ''`.** VACUUM, ANALYZE, pg_terminate_backend. Skip rollback monitoring for these.
11. **Regression detection uses sampled history.** Not all snapshots — would be 10K+ rows at default interval.
12. **Sage's own schema is excluded from ALL analysis.** Filter at the snapshot level before rules run. Sage's own tables are properly indexed in the bootstrap DDL — it doesn't need to find its own problems at runtime.
13. **Tier 1 rules propose. Tier 2 LLM consolidates. Tier 3 executor acts.** Raw Tier 1 findings (missing FK, seq scan) are mechanically correct but naive — they don't consider the full workload. The LLM optimizer takes those raw findings, the full index landscape, and the top queries per table, and produces a minimal index set. The executor acts on the LLM output, not the raw Tier 1 output. When LLM is disabled, executor acts on Tier 1 directly (still correct, just not optimized).
14. **Index anti-proliferation is enforced in code, not just LLM prompting.** Hard limits: max 3 new indexes per table per cycle, never more indexes than columns, write-heavy tables require justification. The LLM is the brain, code is the guardrail. A hallucinating LLM cannot create 50 indexes.

---

## Definition of Done

### Core infrastructure
- [ ] `--mode=standalone` flag accepted; config loaded with CLI > env > YAML > defaults precedence
- [ ] Config hot-reload via fsnotify for hot-reloadable values
- [ ] Startup prerequisite checks: FATAL on missing pg_stat_statements or PG < 14
- [ ] WARN on NULL query text (missing pg_read_all_stats)
- [ ] Advisory lock taken BEFORE schema creation; second instance exits cleanly
- [ ] Schema bootstrap: creates schema + all tables; idempotent on restart
- [ ] trust.ramp_start persisted to and read from sage.config

### Collector
- [ ] Populates sage.snapshots with all categories (queries, tables, indexes, foreign_keys, partitions, system, locks, sequences, replication, io [PG16+])
- [ ] Query stats include total_plan_time and mean_plan_time
- [ ] Query stats filter: AND queryid IS NOT NULL (v1 Bug #1)
- [ ] FK collector excludes sage, pg_catalog, information_schema schemas (FIX-1)
- [ ] Index collector excludes sage schema
- [ ] Table collector excludes sage schema
- [ ] Keyset pagination for >1000 tables (not OFFSET)
- [ ] PG14 compat: no waitstart column, conditional wal columns
- [ ] PG16+: collects pg_stat_io
- [ ] PG17: uses pg_stat_checkpointer (not pg_stat_bgwriter) (v1 Bug #2)
- [ ] COALESCE on all nullable catalog columns (v1 Bug #3, #9)
- [ ] Partition inheritance mapping collected
- [ ] Skips pg_current_wal_lsn() on replicas
- [ ] Circuit breaker: skips on high load, dormant after consecutive skips, recovers

### Analyzer
- [ ] ALL rules operate on sage-schema-filtered snapshots (exclusion at wrapper level)
- [ ] Findings for: unused indexes (with first_seen), invalid indexes, duplicate indexes (btree only), missing FK indexes, slow queries, regressions (sampled history), bloat (with min_rows threshold), seq scans, sequence exhaustion (ascending + descending), cache ratio, checkpoint pressure, connection leaks (idle in transaction only), XID wraparound, replication lag, inactive slots
- [ ] High planning time detection: mean_plan_time > mean_exec_time × plan_time_ratio_warning
- [ ] pg_stat_statements reset detection: skip regression analysis, don't mass-resolve findings
- [ ] Table bloat skips tables below table_bloat_min_rows (FIX-2)
- [ ] Unique indexes never flagged as unused (FIX-7)
- [ ] Primary key indexes never flagged as unused
- [ ] First-cycle skip for delta-based rules
- [ ] Dedup: updates last_seen + occurrence_count on existing open findings
- [ ] Resolution: clears findings whose conditions no longer hold
- [ ] Partition stats aggregated under parent table (FIX-6)

### LLM Index Optimizer (Phase 4b)
- [ ] Skipped entirely when LLM disabled (Tier 1 findings pass through to executor unchanged)
- [ ] Per-table context assembly: DDL + existing indexes + top queries + stats + write rate
- [ ] Cross-query index consolidation: composite indexes covering multiple patterns
- [ ] INCLUDE column recommendations: detecting Index Scan → Index Only Scan opportunities
- [ ] Supersedes Tier 1 index findings on same table (status: 'superseded')
- [ ] Index budget: refuses to create indexes when count >= column count
- [ ] Write-heavy gate: requires LLM justification when R/W ratio < write_heavy_ratio
- [ ] Max 3 new indexes per table per cycle (code-enforced, not just LLM-guided)
- [ ] Ad-hoc query skip: ignores queries with calls < min_query_calls
- [ ] JSON response parsing with fallback on malformed LLM output
- [ ] LLM output validated before executor receives it (DDL syntax check, identifier quoting)

### Executor
- [ ] Trust gate: level × ramp timing × per-tier toggles × emergency stop × HA × maintenance window
- [ ] WARNS if tier3_moderate enabled without maintenance window
- [ ] CONCURRENTLY DDL on raw pgx.Conn (NOT inside transaction)
- [ ] Non-CONCURRENTLY DDL in transactions
- [ ] CREATE/DROP/REINDEX INDEX CONCURRENTLY, VACUUM, ANALYZE, pg_terminate_backend all work
- [ ] Non-reversible actions have `rollback_sql = ''`; rollback monitoring skipped
- [ ] Actions logged to sage.action_log with before/after state
- [ ] Auto-rollback on regression; logs reason
- [ ] Hysteresis: skips rolled-back actions within cooldown period
- [ ] Emergency stop via sage.config; resume works
- [ ] Grant verification at startup with exact fix SQL in warnings
- [ ] Detects INVALID indexes on reconnect (interrupted DDL)

### Supporting systems
- [ ] HA: replica detection, role flip handling
- [ ] Reconnection: exponential backoff, advisory lock re-verification
- [ ] Graceful shutdown: SIGTERM → finish DDL → release lock → exit 0
- [ ] Briefing: structured text + LLM-enhanced (via Gemini) + Slack dispatch
- [ ] LLM client: connects to Gemini endpoint, parses OpenAI-format response, tracks tokens
- [ ] LLM client: HTTP timeout set from config, context cancellation propagated
- [ ] LLM client: response body capped at 1MB (LimitReader)
- [ ] LLM client: non-200 responses handled with clear error messages
- [ ] LLM client: empty choices array handled without crash
- [ ] LLM circuit breaker: 3 retries → open → cooldown → close (tested with real endpoint failures)
- [ ] LLM token budget: tracked daily, enforced, exposed via Prometheus gauge
- [ ] LLM Prometheus: calls_total, errors_total, tokens_total, budget_remaining, latency, circuit_open, parse_failures
- [ ] MCP: tools reflect mode + trust level; sage_emergency_stop/resume/status tools
- [ ] Prometheus: all standalone + LLM metrics emitting real values
- [ ] Health endpoint: mode, trust level, connection state, last cycle times, circuit breaker, LLM status
- [ ] Data retention: batched deletes + first_seen cleanup
- [ ] config.example.yaml complete with all params including Gemini LLM config
- [ ] Gemini model name is `gemini-2.5-flash` (NOT `gemini-2.5-flash-preview` which returns 404)
- [ ] postgres.max_connections default is 5 (NOT 2 — causes pool exhaustion on managed services)

### Test coverage (P0 — must exist before v2 adversarial tests)
- [ ] `internal/collector`: tests for NULL queryid filtering, PG17 checkpointer, schema exclusion, COALESCE on nullable columns, keyset pagination, circuit breaker, snapshot insertion categories, plan_time conditional inclusion
- [ ] `internal/executor`: tests for trust gate matrix, CONCURRENTLY on raw conn, non-reversible action skip, hysteresis, action ordering (create before drop), emergency stop, grant verification, DDL timeout
- [ ] `internal/schema`: tests for advisory lock (first + second instance), fresh bootstrap, idempotent restart, migration
- [ ] `internal/config`: tests for precedence (CLI > env > YAML > defaults), DATABASE_URL override, hot-reload, defaults match spec, validation rejects invalid values
- [ ] `internal/startup`: tests for missing pg_stat_statements (FATAL), PG13 rejected, null query text (WARN), plan_time column detection
- [ ] Integration test `TestFullLLMCycleGemini` implemented and passing
- [ ] Integration test `TestFullCycleNoLLM` implemented and passing
- [ ] Reconnection test: sidecar reconnects after PG restart with exponential backoff
- [ ] Graceful shutdown test: SIGTERM waits for in-flight DDL, releases advisory lock

### Final
- [ ] Integration tests pass PG16 WITH Gemini connected (full cycle: collect → analyze → LLM optimize → execute → findings → MCP → Prometheus)
- [ ] Integration tests pass PG16 WITHOUT LLM (Tier 1 only, no LLM errors)
- [ ] PG14 compat tests pass (no version-specific SQL errors)
- [ ] README updated with standalone setup, required grants, and Gemini LLM configuration
- [ ] All changes committed atomically, tagged v0.7.0-rc1, Docker image pushed
