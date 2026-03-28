# pg_sage

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8.svg)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14--17-336791.svg)](https://www.postgresql.org)

Autonomous PostgreSQL DBA agent. Runs as an external Go sidecar -- no extension, no `shared_preload_libraries`. Connects over the standard wire protocol. Works on Cloud SQL, AlloyDB, Aurora, RDS, and self-managed instances.

pg_sage collects performance data from `pg_stat_statements` and catalog views, runs 25+ diagnostic rules, optionally sends enriched context to an LLM for index optimization and configuration advising, and executes fixes autonomously with trust-ramped safety controls.

## Key Features

- **Tier 1 rules engine** -- 25+ deterministic rules covering index health, query performance, vacuum/bloat, sequences, security, replication, and system configuration
- **Tier 2 LLM-enhanced analysis** -- daily health briefings, index optimization with HypoPG validation, 6 configuration advisors (vacuum, WAL, connections, memory, query rewrite, bloat remediation)
- **Tier 3 trust-ramped executor** -- observation (day 0-7) to advisory (day 8-30) to autonomous (day 31+), with rollback monitoring and emergency stop
- **Per-query tuner** -- EXPLAIN plan analysis with `pg_hint_plan` directives for disk sorts, hash spills, bad nested loops, missed index scans, and parallel query
- **Workload forecaster** -- linear regression and EWMA over daily aggregates to predict disk growth, connection saturation, cache pressure, sequence exhaustion, query volume spikes, and checkpoint pressure
- **Alerting** -- Slack, PagerDuty, and generic webhook channels with per-severity routing, cooldown, and quiet hours
- **React dashboard** -- embedded in the Go binary via `go:embed`, served on `:8080` alongside the REST API
- **Fleet mode** -- monitor N databases from a single sidecar with per-database trust levels, token budgets, and health scores
- **Prometheus metrics** -- standard `/metrics` endpoint on `:9187`

## Quick Start

### Binary

```bash
# Download (Linux amd64 example)
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz | tar xz

# Minimal run -- observation mode, no LLM
./sage-sidecar --database-url "postgres://sage_agent:pw@your-host:5432/postgres"
```

### Docker

```bash
docker run \
  -e SAGE_DATABASE_URL="postgres://sage_agent:pw@host:5432/postgres" \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

### With Config File

```bash
cp config.example.yaml config.yaml
# Edit postgres.host, postgres.password, trust.level, llm settings
./sage-sidecar --config config.yaml
```

### Database User Setup

```sql
CREATE USER sage_agent WITH PASSWORD 'your-password';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
CREATE SCHEMA sage;
GRANT ALL ON SCHEMA sage TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent;
```

### Ports

| Port | Service |
|------|---------|
| 8080 | REST API + React dashboard (web UI) |
| 9187 | Prometheus metrics |

## Configuration

See [`config.example.yaml`](config.example.yaml) for a complete reference.

**Precedence:** CLI flags > environment variables > YAML file > built-in defaults.

Environment variables can be referenced in YAML with `${VAR_NAME}` syntax.

### Key Settings

| Parameter | Default | Description |
|-----------|---------|-------------|
| `mode` | `standalone` | `standalone` or `fleet` |
| `trust.level` | `observation` | `observation`, `advisory`, or `autonomous` |
| `collector.interval_seconds` | `60` | Snapshot collection interval |
| `analyzer.interval_seconds` | `120` | Analysis cycle interval |
| `llm.enabled` | `false` | Enable Tier 2 LLM features |
| `llm.optimizer.enabled` | `false` | Enable index optimizer |
| `llm.advisor.enabled` | `false` | Enable configuration advisors |
| `postgres.max_connections` | `5` | Sidecar connection pool size |
| `executor.maintenance_window` | `* * * * *` | Cron expression for action execution |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `SAGE_DATABASE_URL` | PostgreSQL connection string (overrides YAML) |
| `SAGE_GEMINI_API_KEY` | API key for Gemini or any OpenAI-compatible LLM |
| `SAGE_OPTIMIZER_LLM_API_KEY` | Separate API key for optimizer model (optional) |

### Hot Reload

Most settings reload without restart: analyzer thresholds, trust level, LLM config, retention, briefing schedule. Connection settings (`postgres.*`, listen addresses) require a restart.

## Architecture

```
pg_sage sidecar (single Go binary)
  |
  +-- Collector        [every 60s]   pg_stat_statements, tables, indexes, locks, sequences, replication
  +-- Analyzer         [every 120s]
  |   +-- Tier 1: Rules engine       25+ deterministic checks
  |   +-- Tier 2: Index Optimizer    LLM + 8 validators + HypoPG
  |   +-- Tier 2: Config Advisors    6 LLM-powered tuning advisors
  |   +-- Forecaster                 Linear regression on daily aggregates
  |   +-- Per-Query Tuner            EXPLAIN plan scanning + pg_hint_plan
  +-- Executor         [trust-gated] CONCURRENTLY DDL, rollback monitoring
  +-- Alerting                       Slack, PagerDuty, webhook
  +-- REST API + Dashboard  [:8080]  17 endpoints + React SPA (web UI)
  +-- Prometheus            [:9187]  Metrics endpoint
```

### Tier 1 -- Rules Engine

No LLM required. Deterministic checks every analysis cycle.

| Category | Rules |
|----------|-------|
| Index health | Duplicate, unused, missing FK, invalid, bloated indexes |
| Query performance | Slow queries, regressions, sequential scans on large tables, high total time |
| Vacuum/bloat | Dead tuple accumulation, table bloat, autovacuum needs |
| Sequences | Approaching exhaustion |
| Replication | Lag monitoring, slot status |
| System | Checkpoint pressure, config audit, connection usage |

### Tier 2 -- LLM Features

Pluggable LLM provider via any OpenAI-compatible endpoint (Gemini, OpenAI, Anthropic, Groq, Ollama). Supports dual-model routing (fast model for general tasks, reasoning model for optimization).

- **Index Optimizer** -- enriched table context sent to LLM, recommendations validated through 8 checks + HypoPG, confidence scored 0.0-1.0
- **Configuration Advisors** -- vacuum tuning, WAL/checkpoint, connection pool, memory, query rewrite, bloat remediation
- **Health Briefings** -- periodic summaries of database state
- **Interactive Diagnose** -- ReAct loop for ad-hoc investigation

### Tier 3 -- Action Executor

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|-----------------|
| observation | Day 0-7 | Findings only, no actions |
| advisory | Day 8-30 | SAFE: drop unused/duplicate indexes, VACUUM |
| autonomous | Day 31+ | MODERATE: create indexes, reindex |

HIGH-risk actions always require manual confirmation. All actions logged to `sage.action_log` with rollback SQL.

### Workload Forecaster

Uses linear regression and EWMA over 7+ days of daily aggregates:

| Forecast | Method | Signal |
|----------|--------|--------|
| Disk growth | Linear regression | Database size trend (GB/day) |
| Connection saturation | Linear regression | Peak active backends vs max_connections |
| Cache pressure | EWMA + regression | Buffer cache hit ratio decline |
| Sequence exhaustion | Linear regression | Per-sequence usage trend to 100% |
| Query volume | Week-over-week growth | Total query calls spike detection |
| Checkpoint pressure | EWMA | Checkpoint rate trending above 12/hr |

### Per-Query Tuner

Scans EXPLAIN plans for slow queries and generates `pg_hint_plan` directives:

- Disk sorts (increase `work_mem`)
- Hash spills
- Bad nested loops (force hash/merge join)
- Sequential scans with available indexes (force index scan)
- Disabled parallel query
- Inefficient sort+limit patterns

## Dashboard

The React dashboard is embedded in the binary and served at `http://localhost:8080`.

| Page | Description |
|------|-------------|
| Dashboard | Fleet overview, health scores, key metrics |
| Findings | All findings with severity/status filters |
| Actions | Executed actions with rollback status |
| Database | Per-database detail view |
| Forecasts | Capacity projections and trend charts |
| Query Hints | Per-query tuning recommendations |
| Alert Log | Alert history across all channels |
| Settings | Runtime configuration editor |

<!-- TODO: Add screenshot -->

## API Endpoints

All under `/api/v1/`. In fleet mode, most accept `?database=` to filter by instance.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/databases` | Fleet overview with health scores |
| GET | `/findings` | List findings (filter by severity, status, database) |
| GET | `/findings/:id` | Finding detail |
| POST | `/findings/:id/suppress` | Suppress a finding |
| POST | `/findings/:id/unsuppress` | Unsuppress a finding |
| GET | `/actions` | List executed actions |
| GET | `/actions/:id` | Action detail |
| GET | `/forecasts` | Workload forecast findings |
| GET | `/query-hints` | Per-query tuning recommendations |
| GET | `/alert-log` | Alert dispatch history |
| GET | `/snapshots/latest` | Latest collector snapshot |
| GET | `/snapshots/history` | Snapshot time series |
| GET | `/config` | Current configuration |
| PUT | `/config` | Update configuration (hot reload) |
| GET | `/metrics` | JSON metrics summary |
| POST | `/emergency-stop` | Halt all autonomous actions |
| POST | `/resume` | Resume after emergency stop |

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

## Building from Source

**Requirements:** Go 1.24+, Node.js 20+

```bash
cd sidecar

# Build the React dashboard
cd web && npm ci && npm run build && cd ..

# Build the Go binary (dashboard is embedded via go:embed)
go build -o sage-sidecar ./cmd/pg_sage_sidecar/

# Run tests
go test ./... -count=1 -timeout 300s

# Run with race detector
go test ./... -race -count=1

# Lint
go vet ./...
golangci-lint run ./...
```

## Docker

### Build

```bash
cd sidecar
docker build -t pg_sage .
```

### Run

```bash
docker run \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -e SAGE_PG_PASSWORD=secret \
  -e SAGE_GEMINI_API_KEY=sk-... \
  -p 8080:8080 \
  -p 9187:9187 \
  pg_sage
```

### Release Build

```bash
# From repo root
goreleaser build --snapshot --clean
# Produces: linux/darwin, amd64/arm64
```

## Project Structure

```
pg_sage/
  sidecar/
    cmd/pg_sage_sidecar/       Entry point
    internal/
      advisor/                 6 LLM configuration advisors + GUC validator
      alerting/                Slack, PagerDuty, webhook channels
      analyzer/                Tier 1 rules engine (25+ rules)
      api/                     REST API + embedded React dashboard
      briefing/                LLM health briefings
      collector/               Snapshot collection from catalog views
      config/                  YAML config, validation, hot-reload
      executor/                Tier 3 trust-gated action execution
      fleet/                   Multi-database manager
      forecaster/              Workload forecasting (6 forecast types)
      ha/                      Advisory lock leader election
      llm/                     OpenAI-compatible LLM client
      optimizer/               LLM index optimizer + HypoPG validation
      retention/               Data retention + cleanup
      schema/                  Schema bootstrap + migrations
      startup/                 Prerequisite validation
      tuner/                   Per-query tuner + pg_hint_plan
    web/                       React 19 + Vite + Tailwind dashboard
    Dockerfile
    go.mod
  config.example.yaml
  .goreleaser.yml
  docker-compose.yml
  LICENSE
```

## License

[GNU Affero General Public License v3.0](https://www.gnu.org/licenses/agpl-3.0.html)
