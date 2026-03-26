# pg_sage

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8.svg)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14--17-336791.svg)](https://www.postgresql.org)
[![Tests](https://img.shields.io/badge/tests-530%20passing-brightgreen.svg)](#testing)
[![CI](https://github.com/jasonmassie01/pg_sage/actions/workflows/test.yml/badge.svg)](https://github.com/jasonmassie01/pg_sage/actions/workflows/test.yml)

**Autonomous PostgreSQL DBA Agent** — monitors, analyzes, and optimizes any PostgreSQL 14–17 database. Works on managed services (Cloud SQL, AlloyDB, Aurora, RDS) and self-managed instances.

pg_sage is a Go sidecar that connects to your database over the network, collects performance data from `pg_stat_statements` and catalog views, runs 18+ diagnostic rules, sends enriched context to an LLM for index optimization and 6 configuration advisors, and executes fixes autonomously with trust-ramped safety controls. 530 tests across 14 packages.

```bash
# Linux amd64
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_0.7.0_linux_amd64.tar.gz | tar xz
./pg_sage --database-url "postgres://user:pass@host:5432/db"
```

---

## What It Does

**Collects** snapshots every 60s from pg_stat_statements, pg_stat_user_tables, pg_stat_user_indexes, sequences, locks, replication state, and 5+ additional catalog views.

**Analyzes** with 18+ deterministic rules that detect slow queries, missing indexes, duplicate indexes, unused indexes, table bloat, sequence exhaustion, checkpoint pressure, and more.

**Optimizes** with an LLM-powered index optimizer that consolidates recommendations across your workload, validates them with [HypoPG](https://hypopg.readthedocs.io/) before execution, and scores confidence using 6 weighted signals.

**Executes** autonomously with graduated trust: observation → advisory → autonomous. All DDL uses `CONCURRENTLY`. Every action has rollback metadata. Regression triggers automatic rollback.

**Reports** via [MCP](https://modelcontextprotocol.io/) (Claude Desktop), Prometheus metrics, and structured briefings.

---

## Quick Start

### Managed PostgreSQL (Cloud SQL, AlloyDB, Aurora, RDS)

```bash
# Download (pick your platform)
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_0.7.0_linux_amd64.tar.gz | tar xz
# Also available: linux_arm64, darwin_amd64, darwin_arm64

# Run (minimum config — observation mode, no LLM)
./pg_sage \
  --database-url "postgres://sage_agent:pw@your-instance:5432/postgres" \
  --mode standalone
```

### Docker

```bash
docker run -e SAGE_DATABASE_URL="postgres://sage_agent:pw@host:5432/db" \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

### With LLM (Gemini, Claude, or any OpenAI-compatible API)

```yaml
# config.yaml
mode: standalone

postgres:
  host: your-instance-ip
  port: 5432
  user: sage_agent
  password: your-password
  database: postgres
  sslmode: require
  max_connections: 5

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 120

trust:
  level: observation     # start here, promote after confidence builds

llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_GEMINI_API_KEY}
  optimizer:
    enabled: true

mcp:
  enabled: true
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

```bash
./pg_sage --config config.yaml
```

### Database User Setup

```sql
CREATE USER sage_agent WITH PASSWORD 'your-password';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;    -- for index creation
GRANT pg_signal_backend TO sage_agent;           -- for query termination
CREATE SCHEMA sage;
GRANT ALL ON SCHEMA sage TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent;
```

---

## Architecture

```
pg_sage sidecar
  ├── Collector        [every 60s]  pg_stat_statements, tables, indexes, locks, ...
  ├── Analyzer         [every 120s]
  │   ├── Tier 1: Rules engine (18+ deterministic checks)
  │   └── Tier 2: Index Optimizer (LLM + HypoPG validation)
  ├── Executor         [trust-gated]
  │   ├── CONCURRENTLY DDL on raw pgx connection
  │   ├── Rollback monitor (read + write latency regression)
  │   └── Emergency stop via sage.config
  ├── MCP Server       [:8080]  Claude Desktop / AI agent interface
  └── Prometheus       [:9187]  Metrics endpoint
```

### Tier 1 — Rules Engine

Deterministic checks that run every analyzer cycle. No LLM required.

| Category | What It Detects |
|----------|----------------|
| Index health | Duplicate, unused, missing FK, invalid, bloated indexes |
| Query performance | Slow queries, regressions, sequential scans on large tables |
| Sequences | Approaching exhaustion (configurable threshold) |
| Maintenance | Dead tuple accumulation, VACUUM needs, table bloat |
| Checkpoint pressure | High checkpoint frequency |
| Replication | Lag monitoring |

### Tier 2 — LLM Features

**Index Optimizer v2** — Sends enriched table context (columns, pg_stats selectivity, execution plans, write rates, workload classification) to an LLM and gets back consolidated index recommendations. Each recommendation passes through:

1. **8 validators** — CONCURRENTLY keyword, column existence, duplicate detection, write impact, max indexes, extension requirements, BRIN correlation, expression volatility
2. **HypoPG validation** — creates hypothetical indexes and measures actual planner cost reduction (when HypoPG is installed)
3. **Confidence scoring** — 6 weighted signals (query volume, plan clarity, write rate, HypoPG result, selectivity, table traffic) produce a 0.0–1.0 score
4. **Action level** — ≥0.7 autonomous, ≥0.4 advisory, <0.4 informational

**Configuration Advisor Suite** — 6 LLM-powered advisors for PostgreSQL tuning:

| Advisor | What It Analyzes |
|---------|-----------------|
| Vacuum Tuning | Per-table autovacuum settings, dead tuple ratios, reloptions |
| WAL/Checkpoint | max_wal_size, wal_compression, checkpoint frequency |
| Connection Pool | max_connections sizing, idle timeout, pooler detection |
| Memory Tuning | shared_buffers, work_mem, cache hit ratio, spill detection |
| Query Rewrite | N+1 patterns, correlated subqueries, OFFSET pagination |
| Bloat Remediation | VACUUM FULL vs pg_repack vs do nothing, with duration estimates |

All advisor findings are informational (severity=info) — never auto-executed. Includes a GUC validator that blocks dangerous values and respects managed-service restrictions (Cloud SQL, AlloyDB, Aurora, RDS).

Supports dual-model routing: a fast model (Gemini Flash, Haiku) for general tasks and a reasoning model (Opus, Pro) for index optimization.

### Tier 3 — Action Executor

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Day 0–7 | No actions — findings only |
| **advisory** | Day 8–30 | SAFE: drop unused/duplicate indexes, VACUUM |
| **autonomous** | Day 31+ | MODERATE: create indexes, reindex |

HIGH-risk actions (sequence changes, RLS) always require manual confirmation. Every action is logged to `sage.action_log` with before/after state and rollback SQL.

---

## Verified Platforms

| Platform | PG Versions | Status | Code Changes |
|----------|-------------|--------|-------------|
| Google Cloud SQL | 14, 15, 16, 17 | ✅ 530 tests, 3 LLM findings verified e2e | Zero |
| Google AlloyDB | 17 | ✅ Full parity | Zero |
| Self-managed | 14, 15, 16, 17 | ✅ Verified | Zero |
| Amazon Aurora | 14–17 | Test plan ready | — |
| Amazon RDS | 14–17 | Test plan ready | — |

---

## Testing

530 tests across 14 packages, 0 failures.

```bash
cd sidecar
go test ./... -count=1 -timeout 300s
```

| Package | Tests | What It Covers |
|---------|-------|---------------|
| `internal/optimizer` | 144 | Validation, confidence, fingerprinting, cost, HypoPG, plans |
| `internal/advisor` | 102 | 6 LLM advisors, GUC validator, prompt parsing, e2e with Gemini |
| `internal/llm` | 28 | Chat, budget, circuit breaker, timeout, dual-model Manager |
| `internal/schema` | 15 | Bootstrap, migrations, advisory lock, idempotent restart |
| `sidecar` (root) | 11 | MCP, SSE, Prometheus |
| `internal/collector` | 11 | SQL variants, circuit breaker, snapshot categories |
| `internal/briefing` | 11 | Generate with live findings |
| `internal/ha` | 10 | Advisory lock, recovery detection |
| `internal/config` | 9 | Defaults, precedence, hot-reload, validation |
| `internal/retention` | 6 | Purge old snapshots, clean stale findings |
| `internal/analyzer` | 5 | Bloat, plan time, regression, slow queries |
| `internal/startup` | 5 | Prereq checks, version detection, plan_time columns |
| `internal/executor` | 4 | Trust gates (12 subtests), CONCURRENTLY, categorize |

---

## Configuration

### YAML (sidecar)

See [`config.example.yaml`](config.example.yaml) for a complete starter configuration.

Key settings:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `trust.level` | `observation` | Trust tier: `observation`, `advisory`, `autonomous` |
| `collector.interval_seconds` | `60` | Seconds between snapshot collections |
| `analyzer.interval_seconds` | `120` | Seconds between analysis cycles |
| `llm.enabled` | `false` | Enable LLM-powered features |
| `llm.optimizer.enabled` | `false` | Enable index optimizer |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new indexes recommended per table per cycle |
| `postgres.max_connections` | `5` | Connection pool size |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `SAGE_DATABASE_URL` | PostgreSQL connection string (overrides YAML) |
| `SAGE_GEMINI_API_KEY` | Gemini API key |
| `SAGE_OPTIMIZER_LLM_API_KEY` | Separate API key for optimizer model (optional) |

---

## MCP Integration

Connect Claude Desktop (or any MCP client) to `http://localhost:8080/sse`:

```json
{
  "mcpServers": {
    "pg_sage": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

Available tools: `sage_status`, `sage_analyze`, `sage_explain`, `sage_briefing`, `sage_diagnose`.

Ask questions like:
- "What are my slowest queries?"
- "Show me duplicate indexes"
- "Why is my application slow?"
- "What maintenance does my database need?"

---

## Prometheus Metrics

Scrape `http://localhost:9187/metrics`:

```
pg_sage_findings_total{severity="critical|warning|info"}
pg_sage_collector_last_run_timestamp
pg_sage_connection_up
pg_sage_llm_calls_total{model,purpose}
pg_sage_llm_circuit_open{model}
pg_sage_optimizer_recommendations_total{action_level}
pg_sage_executor_actions_total{outcome}
pg_sage_database_size_bytes
```

---

## Project Structure

```
pg_sage/
├── sidecar/                        ← THE PRODUCT
│   ├── cmd/pg_sage_sidecar/        # Entry point
│   ├── internal/
│   │   ├── analyzer/               # Tier 1 rules engine
│   │   ├── briefing/               # LLM briefing generation
│   │   ├── collector/              # Snapshot collection
│   │   ├── config/                 # YAML config + hot-reload
│   │   ├── executor/               # Tier 3 trust-gated DDL
│   │   ├── ha/                     # Advisory lock, HA awareness
│   │   ├── llm/                    # LLM client + dual-model Manager
│   │   ├── advisor/                # 6 LLM configuration advisors + GUC validator
│   │   ├── optimizer/              # Index Optimizer v2
│   │   ├── retention/              # Data retention + cleanup
│   │   ├── schema/                 # Bootstrap + migrations
│   │   └── startup/                # Prereq checks
│   ├── resources.go                # MCP resources
│   ├── tools.go                    # MCP tools
│   └── go.mod
├── extension/                      ← FROZEN (security fixes only)
│   ├── src/                        # C extension (v0.6.0-rc3)
│   ├── include/
│   ├── sql/
│   └── Makefile
├── cloudsqltests/                  # GCP integration test infrastructure
├── docs/                           # Architecture, walkthroughs
└── grafana/                        # Dashboard templates
```

### C Extension (Frozen)

The C extension at `extension/` is frozen at v0.6.0-rc3. It works on self-managed PostgreSQL with auto-explain hooks and in-process SQL functions (`sage.explain()`, `sage.diagnose()`, `sage.briefing()`). No new features — security fixes only. The sidecar is the product.

---

## How It Works

1. **Sidecar starts**, connects to PostgreSQL, acquires advisory lock `710190109` (`hashtext('pg_sage')`), bootstraps the `sage` schema
2. **Collector** runs every 60s, captures snapshots from 10+ catalog views into `sage.snapshots`
3. **Analyzer** runs every 120s, applies Tier 1 rules → generates findings → calls optimizer (if LLM enabled) → upserts all findings to `sage.findings`
4. **Optimizer** (when enabled) groups queries by table, assembles enriched context (columns, pg_stats, plans, write rates), sends to LLM, validates every recommendation through 8 checks + HypoPG, scores confidence
5. **Executor** reads actionable findings, checks trust gates (level × ramp × per-tier toggles × maintenance window × emergency stop × replica check), executes DDL on raw `pgx.Conn` with `CONCURRENTLY`, monitors for regression
6. **MCP server** exposes findings and tools to Claude Desktop or any AI agent
7. **Prometheus** exports metrics for monitoring and alerting

---

## License

[GNU Affero General Public License v3.0](https://www.gnu.org/licenses/agpl-3.0.html)
