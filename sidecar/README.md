# pg_sage Sidecar — v0.7.0

A standalone Go application that monitors PostgreSQL health, generates findings, executes autonomous fixes, and exposes everything via [MCP](https://modelcontextprotocol.io/) and Prometheus. Works **with or without** the pg_sage C extension.

## Quick Start

```bash
# Build from source
cd sidecar
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar

# Copy and edit the config
cp config.example.yaml config.yaml
# Set at minimum: postgres.host, postgres.password

# Run
./pg_sage_sidecar --config config.yaml

# Or with Docker
docker build -t pg_sage_sidecar .
docker run -v $(pwd)/config.yaml:/app/config.yaml pg_sage_sidecar
```

## Configuration

All configuration lives in a YAML file. See [`config.example.yaml`](config.example.yaml) for the full reference with defaults and comments.

**Precedence:** CLI flags > environment variables > config file > built-in defaults.

Environment variables can be referenced inside the YAML using `${VAR_NAME}` syntax (useful for secrets like `postgres.password` and `llm.api_key`).

### Key Sections

| Section | Purpose |
|---|---|
| `mode` | `standalone` (full pipeline) or `extension` (reads from C extension) |
| `postgres` | Connection settings, max_connections, SSL mode |
| `collector` | Snapshot interval, batch size for catalog scans |
| `analyzer` | Rule thresholds: slow queries, bloat, cache ratio, XID wraparound, etc. |
| `safety` | CPU ceiling, query timeouts, DDL timeout, backoff/dormant behavior |
| `trust` | Trust level (`observation` / `advisory` / `autonomous`), maintenance windows, rollback policy |
| `llm` | OpenAI-compatible endpoint for LLM-powered analysis and index optimization |
| `briefing` | Scheduled health briefings (stdout or Slack) |
| `retention` | Cleanup policy for snapshots, findings, actions, explains |
| `mcp` | MCP server listen address |
| `prometheus` | Metrics endpoint listen address |

### Hot Reload

Most settings reload without restart (analyzer thresholds, trust level, LLM config, retention, briefing schedule). Connection settings (`postgres.*`, `mcp.listen_addr`, `prometheus.listen_addr`) require a restart.

## Architecture

Entry point: `cmd/pg_sage_sidecar/main.go`

Internal packages under `internal/`:

| Package | Responsibility |
|---|---|
| `config` | YAML loading, validation, hot-reload via fsnotify |
| `collector` | Periodic snapshots of `pg_stat_statements`, `pg_stat_user_tables`, locks, connections |
| `analyzer` | Rule-based finding generation (slow queries, bloat, missing indexes, XID, cache ratio) |
| `executor` | Autonomous action execution with trust-ramped tiers and rollback |
| `llm` | OpenAI-compatible client with token budgeting and circuit breaker |
| `briefing` | Scheduled health summaries to stdout or Slack |
| `retention` | Cleanup of aged-out snapshots, findings, actions, and explain plans |
| `schema` | Schema introspection (columns, indexes, constraints, stats) |
| `ha` | Leader election for multi-replica deployments |
| `startup` | Bootstrap: schema migration, extension detection, initial collection |

## Standalone Mode

When `mode: standalone`, the sidecar runs the full pipeline internally:

1. **Collector** snapshots catalog views on an interval
2. **Analyzer** evaluates rules against collected data and generates findings
3. **Executor** acts on findings based on `trust.level`:
   - `observation` — log only
   - `advisory` — log + surface via MCP
   - `autonomous` — execute safe actions (trust-ramped with rollback)
4. **Retention** cleans up old data on schedule

No C extension required. Compatible with managed databases: **Cloud SQL, AlloyDB, RDS, Aurora**.

When `mode: extension`, the sidecar reads findings from the C extension's `sage.*` schema and only provides the MCP/metrics layer.

## MCP Resources and Tools

Transport: HTTP + SSE. Client opens `GET /sse`, receives a session endpoint, sends JSON-RPC 2.0 to `POST /messages?sessionId=xxx`.

### Resources

| URI | Description |
|---|---|
| `sage://health` | Database health snapshot |
| `sage://findings` | Open findings |
| `sage://slow-queries` | Recently observed slow queries |
| `sage://schema/{table}` | Column and index info for a table |
| `sage://stats/{table}` | pg_stat_user_tables stats |
| `sage://explain/{queryid}` | Cached EXPLAIN plan |

### Tools

| Tool | Description |
|---|---|
| `diagnose` | Interactive diagnostic question |
| `briefing` | On-demand health briefing |
| `suggest_index` | Index suggestions for a table |
| `review_migration` | Review DDL for risks |

### Prompts

| Prompt | Arguments |
|---|---|
| `investigate_slow_query` | `queryid` |
| `review_schema` | `table` |
| `capacity_plan` | (none) |

**Authentication:** API key via `X-API-Key` header. Rate limited per client IP (configurable).

## Prometheus Metrics

Available at `GET :9187/metrics`:

- `pg_sage_findings_total{severity}` — open findings by severity
- `pg_sage_info{version}` — version info
- `pg_sage_circuit_breaker_state{breaker}` — circuit breaker states
- `pg_sage_collector_duration_seconds` — collection cycle timing
- `pg_sage_analyzer_duration_seconds` — analysis cycle timing
- `pg_sage_actions_total{tier,result}` — autonomous actions executed

## Docker

```bash
docker build -t pg_sage_sidecar .

docker run \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -e SAGE_PG_PASSWORD=secret \
  -e SAGE_LLM_API_KEY=sk-... \
  -p 5433:5433 \
  -p 9187:9187 \
  pg_sage_sidecar
```

Mount your `config.yaml` and pass secrets via environment variables referenced in the YAML.
