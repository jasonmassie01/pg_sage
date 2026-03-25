# Architecture

pg_sage is a Go sidecar that connects to any PostgreSQL 14-17 over the network. The C extension is frozen at v0.6.0-rc3 and is not the product.

## Why a Sidecar

1. **No installation friction**: No `shared_preload_libraries`, no PostgreSQL restart, no matching PG major-version binaries. Works on managed services (RDS, Cloud SQL, AlloyDB, Aurora) where custom extensions are restricted or prohibited.

2. **Single implementation**: All collector, analyzer, optimizer, and executor logic lives in Go. No dual C/Go maintenance burden.

3. **Feature velocity**: The optimizer, MCP server, briefing worker, and Prometheus exporter ship as a single binary with no database-side dependency.

---

## Pipeline

```
pg_sage sidecar (single Go binary)
  ├── Collector        [every 60s]
  │     pg_stat_statements, pg_stat_user_tables, pg_stat_user_indexes,
  │     pg_sequences, pg_locks, pg_stat_replication, pg_stat_bgwriter,
  │     pg_stat_checkpointer, pg_stat_activity, pg_database_size()
  │
  ├── Analyzer         [every 120s]
  │   ├── Tier 1: Rules engine (18+ deterministic checks)
  │   └── Tier 2: Index Optimizer (LLM + HypoPG validation)
  │
  ├── Executor         [trust-gated]
  │   ├── CONCURRENTLY DDL on raw pgx connection
  │   ├── Rollback monitor (read + write latency regression)
  │   └── Emergency stop via sage.config
  │
  ├── MCP Server       [:8080]  Claude Desktop / AI agent interface
  └── Prometheus       [:9187]  Metrics endpoint
```

---

## Component Details

### Collector

Gathers snapshots every 60s (configurable) from 10+ catalog views. Stores raw data in `sage.snapshots` as JSONB. Uses a circuit breaker to back off during database crises.

### Analyzer (Tier 1 -- Rules Engine)

16+ deterministic rules across 6 categories. No LLM required:

| Category | Rules |
|----------|-------|
| Index health | unused_index, invalid_index, duplicate_index, missing_fk_index |
| Query performance | slow_query, high_plan_time, query_regression, seq_scan_heavy |
| Sequences | sequence_exhaustion |
| Maintenance | table_bloat, xid_wraparound |
| System | connection_leak, cache_hit_ratio, checkpoint_pressure |
| Replication | replication_lag, inactive_slot |

Each rule produces findings with severity (critical/warning/info), recommended SQL, and rollback SQL.

### Optimizer (Tier 2 -- LLM Index Optimizer)

Lives in `internal/optimizer/` (18 files, 4,640 lines, 144 tests). Key capabilities:

- **Plan-aware**: Captures EXPLAIN plans via `GENERIC_PLAN` (PG 16+) or on-demand execution to inform recommendations.
- **HypoPG validation**: Creates hypothetical indexes and measures actual planner cost reduction before recommending.
- **Dual-model LLM**: Separate LLM client for optimizer (reasoning-tier) with independent circuit breaker and token budget.
- **Confidence scoring**: 0.0-1.0 score based on 6 weighted signals (query volume, plan clarity, write rate, HypoPG result, selectivity, table traffic). Maps to action levels: autonomous (>=0.7) / advisory (>=0.4) / informational (<0.4).
- **8 validators**: CONCURRENTLY check, column existence, duplicate detection, write impact analysis, max indexes per table, extension requirements, BRIN correlation, expression volatility.
- **Cold start protection**: Waits for N snapshots before running.
- **Post-check**: Verifies `indisvalid` after CREATE INDEX CONCURRENTLY.

### Executor (Tier 3 -- Trust-Gated)

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Day 0-7 | No actions -- findings only |
| **advisory** | Day 8-30 | SAFE: drop unused/duplicate indexes, VACUUM |
| **autonomous** | Day 31+ | MODERATE: create indexes, reindex |

HIGH-risk actions always require manual confirmation. Every action is logged to `sage.action_log` with before/after state and rollback SQL. Regression triggers automatic rollback.

The executor checks: trust level, trust ramp, per-tier toggles, maintenance window, emergency stop flag, and replica status before acting.

### MCP Server

HTTP + SSE transport on `:8080` (configurable). Implements the Model Context Protocol for Claude Desktop and other AI agents. Exposes resources (health, findings, schema, stats, slow queries, explain plans) and tools (diagnose, briefing, suggest_index, review_migration).

### Prometheus Exporter

Metrics endpoint on `:9187`. Exports findings count by severity, circuit breaker state, connection stats, cache hit ratio, database size, and LLM usage.

---

## Data Flow

1. **Collector** gathers `pg_stat_statements`, `pg_stat_user_tables`, `pg_stat_user_indexes` every 60s into `sage.snapshots`.
2. **Analyzer** runs rules every 120s, then calls `optimizer.Analyze()` if LLM is enabled.
3. **Optimizer** enriches table contexts with `information_schema.columns`, `pg_stats`, plan data, and workload classification.
4. LLM generates index recommendations as JSON.
5. **Validator** runs 8 checks; **HypoPG** validates if available.
6. **Confidence scorer** assigns action level.
7. Findings are persisted to `sage.findings`.
8. **Executor** acts on findings based on trust level, confidence, and maintenance window.

---

## Schema Bootstrap

On startup, pg_sage acquires advisory lock `710190109` (`hashtext('pg_sage')`), then creates the `sage` schema and tables if they do not exist. See [SQL Reference](sql-reference.md) for the full schema.

---

## C Extension (Frozen)

The C extension at `extension/` is frozen at v0.6.0-rc3. When co-deployed on self-managed PostgreSQL, it adds `sage.explain_cache` via executor hooks and in-process SQL functions (`sage.explain()`, `sage.diagnose()`, `sage.briefing()`). The sidecar detects the extension at startup and uses it opportunistically. All core functionality works without it.
