# MCP Sidecar

pg_sage includes a thin Go sidecar that exposes the extension's capabilities via the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) over HTTP+SSE. This lets AI coding assistants (Claude, Cursor, Copilot) interact with your database through pg_sage. The sidecar operates in two modes: extension mode (with pg_sage C extension installed) and standalone mode (direct catalog queries for managed databases like Cloud SQL, AlloyDB, RDS, Aurora).

---

## What is MCP?

The Model Context Protocol is an open standard for connecting AI assistants to external data sources and tools. It uses JSON-RPC 2.0 over Server-Sent Events (SSE) for real-time communication.

The pg_sage sidecar implements the MCP specification, exposing database health, findings, schema information, and diagnostic tools as MCP resources and tools.

---

## Running the Sidecar

### With YAML Config (Recommended)

```bash
cd sidecar && go build -o sage-sidecar ./cmd/pg_sage_sidecar
./sage-sidecar --config config.yaml
```

### With Docker Compose

```bash
docker compose up
```

This starts both the PostgreSQL + pg_sage container and the sidecar.

### With Environment Variables

```bash
export SAGE_DATABASE_URL="postgres://user:pass@host:5432/dbname"
export SAGE_MCP_PORT=5433
export SAGE_PROMETHEUS_PORT=9187
cd sidecar && go build -o sage-sidecar ./cmd/pg_sage_sidecar && ./sage-sidecar
```

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SAGE_DATABASE_URL` | `postgres://postgres@localhost:5432/postgres?sslmode=disable` | PostgreSQL connection string |
| `SAGE_MCP_PORT` | `5433` | Port for the MCP server |
| `SAGE_PROMETHEUS_PORT` | `9187` | Port for Prometheus metrics |
| `SAGE_RATE_LIMIT` | `60` | Maximum requests per minute per IP |
| `SAGE_TOKEN_BUDGET` | `10000` | Token budget for LLM calls |
| `SAGE_API_KEY` | `""` | API key for authentication (empty = no auth) |
| `SAGE_TLS_CERT` | `""` | Path to TLS certificate file |
| `SAGE_TLS_KEY` | `""` | Path to TLS private key file |
| `SAGE_PG_MAX_CONNS` | `5` | Maximum PostgreSQL connections in pool |
| `SAGE_PG_MIN_CONNS` | `1` | Minimum PostgreSQL connections in pool |

---

## Resources

MCP resources provide read-only access to database state. Clients read resources via `resources/read` requests.

### `sage://health`

System health overview including connections, cache hit ratio, disk usage, and worker status.

```json
{
  "version": "0.5.0",
  "enabled": true,
  "circuit_state": "closed",
  "llm_circuit_state": "closed",
  "emergency_stopped": false,
  "workers": { "collector": true, "analyzer": true, "briefing": true },
  "connections": { "total": 12, "active": 3, "max": 100 },
  "cache_hit_ratio_pct": 99.87,
  "disk": { "database_size": "256 MB", "database_size_bytes": 268435456 }
}
```

### `sage://findings`

All open findings sorted by severity.

```json
[
  {
    "id": 1,
    "category": "duplicate_index",
    "severity": "critical",
    "title": "Duplicate index public.idx_orders_dup2 matches idx_orders_dup1",
    "recommendation": "Drop the duplicate index",
    "recommended_sql": "DROP INDEX CONCURRENTLY public.idx_orders_dup2;",
    "status": "open"
  }
]
```

### `sage://schema/{table}`

Columns, indexes, constraints, and foreign keys for a specific table.

```
GET sage://schema/public.orders
```

```json
{
  "table_name": "public.orders",
  "columns": [
    { "name": "id", "type": "integer", "nullable": "NO", "default": "nextval('orders_id_seq')" }
  ],
  "indexes": [
    { "name": "orders_pkey", "definition": "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)" }
  ],
  "constraints": [
    { "name": "orders_pkey", "type": "p", "definition": "PRIMARY KEY (id)" }
  ],
  "foreign_keys": []
}
```

### `sage://stats/{table}`

Table size, row counts, dead tuples, vacuum status, and index usage statistics.

```
GET sage://stats/public.orders
```

```json
{
  "table_name": "public.orders",
  "total_size": "128 MB",
  "table_size": "96 MB",
  "indexes_size": "32 MB",
  "live_tuples": 1500000,
  "dead_tuples": 1200,
  "dead_tuple_ratio_pct": 0.08,
  "seq_scan": 15,
  "idx_scan": 482000,
  "last_autovacuum": "2025-03-18 14:30:00+00"
}
```

### `sage://slow-queries`

Top 20 slow queries from `pg_stat_statements`, ordered by total execution time.

```json
[
  {
    "queryid": 1234567890,
    "query": "SELECT * FROM orders WHERE created_at > $1",
    "calls": 15000,
    "total_exec_time_ms": 45000.50,
    "mean_exec_time_ms": 3.00,
    "max_exec_time_ms": 250.00
  }
]
```

### `sage://explain/{queryid}`

Cached EXPLAIN plan for a specific query ID.

```
GET sage://explain/1234567890
```

```json
{
  "captured_at": "2025-03-18 14:00:00+00",
  "queryid": 1234567890,
  "query_text": "SELECT * FROM orders WHERE created_at > $1",
  "plan": [ { "Plan": { "Node Type": "Seq Scan", "Total Cost": 25000.00 } } ],
  "source": "on-demand",
  "total_cost": 25000.00
}
```

---

## Tools

MCP tools allow AI assistants to invoke actions. Clients call tools via `tools/call` requests.

### `diagnose`

Interactive diagnostic analysis via ReAct reasoning. The LLM reasons through problems step by step, executing follow-up SQL queries autonomously.

**Input:**

```json
{ "question": "Why are my queries slow today?" }
```

**Output:** Natural-language analysis with SQL evidence and recommendations.

### `briefing`

Generate an on-demand health briefing of the current database state.

**Input:** None required.

**Output:** Structured health report covering findings, performance metrics, and recommendations.

### `suggest_index`

Get index recommendations for a specific table based on query patterns and current index coverage.

**Input:**

```json
{ "table": "public.orders" }
```

**Output:** Suggested CREATE INDEX statements with rationale.

### `review_migration`

Review DDL or migration SQL for production safety issues (locking, long-running operations, data loss risks).

**Input:**

```json
{ "ddl": "ALTER TABLE orders ADD COLUMN status text DEFAULT 'pending';" }
```

**Output:** Risk assessment with specific warnings and safer alternatives.

---

## Prompts

MCP prompts provide pre-built conversation templates that AI assistants can use.

| Prompt | Arguments | Description |
|---|---|---|
| `investigate_slow_query` | `queryid` (required) | Investigate why a specific query is slow |
| `review_schema` | `table` (required) | Review the schema design of a table |
| `capacity_plan` | none | Analyze current database capacity and growth trends |

### Example: `investigate_slow_query`

```json
{
  "name": "investigate_slow_query",
  "arguments": { "queryid": "1234567890" }
}
```

Returns a prompt message instructing the AI to examine the execution plan, table statistics, and index usage for the specified query.

---

## Authentication

Set the `SAGE_API_KEY` environment variable to require authentication:

```bash
export SAGE_API_KEY="your-secret-key"
```

When set, all MCP requests must include the key in the `Authorization` header:

```
Authorization: Bearer your-secret-key
```

!!! warning
    Without `SAGE_API_KEY` set, the sidecar accepts unauthenticated requests. Always set an API key in production.

---

## Rate Limiting

The sidecar implements a sliding-window rate limiter per client IP:

- Default: 60 requests per minute per IP
- Configurable via `SAGE_RATE_LIMIT` environment variable
- Respects `X-Forwarded-For` header for proxied clients
- Rate-limited requests receive HTTP 429 with a JSON error body
- Rate limit events are logged to `sage.mcp_log` (or `public.sage_mcp_log` in sidecar-only mode)

```json
{"error": "rate limit exceeded"}
```

---

## Prometheus Metrics

The sidecar exposes Prometheus metrics at `:9187/metrics`.

### With pg_sage Extension

| Metric | Type | Description |
|---|---|---|
| `pg_sage_info{version}` | gauge | Extension version |
| `pg_sage_findings_total{severity}` | gauge | Open findings by severity (critical, warning, info) |
| `pg_sage_circuit_breaker_state{breaker}` | gauge | Circuit breaker state (0=closed, 1=open) |
| `pg_sage_status_*` | gauge | Numeric fields from `sage.status()` |

### Sidecar-Only Mode

| Metric | Type | Description |
|---|---|---|
| `pg_sage_info{version,pg_version}` | gauge | Sidecar and PostgreSQL version |
| `pg_sage_connections_total{state}` | gauge | Connections by state |
| `pg_sage_max_connections` | gauge | Configured `max_connections` |
| `pg_sage_database_size_bytes` | gauge | Database size in bytes |
| `pg_sage_xact_commit_total` | counter | Transactions committed |
| `pg_sage_xact_rollback_total` | counter | Transactions rolled back |
| `pg_sage_blks_hit_total` | counter | Shared buffer hits |
| `pg_sage_blks_read_total` | counter | Disk blocks read |
| `pg_sage_deadlocks_total` | counter | Deadlocks detected |
| `pg_sage_cache_hit_ratio` | gauge | Buffer cache hit ratio |
| `pg_sage_uptime_seconds` | gauge | PostgreSQL uptime |

---

## Standalone Mode for Managed Databases

When connected to a managed database (Cloud SQL, AlloyDB, RDS, Aurora) without the pg_sage C extension installed, the sidecar automatically detects the absence and switches to standalone mode. Cloud environment auto-detection identifies the provider (Cloud SQL, AlloyDB, RDS, Aurora) and adjusts behavior accordingly.

In standalone mode, the sidecar runs its own collector, analyzer, executor, and LLM client — providing the full pg_sage experience without requiring the C extension:

- MCP resources work using direct SQL against `pg_stat_*` views and catalog queries
- Prometheus metrics are generated from catalog queries
- Tools including `diagnose` and `briefing` are fully available
- The sidecar creates its own schema tables (`sage.findings`, `sage.action_log`, etc.) in the target database
- Background collection and analysis run on configurable intervals

```bash
docker compose run --rm \
  -e SAGE_DATABASE_URL="postgres://user:pass@your-rds-host:5432/dbname?sslmode=require" \
  -p 5433:5433 -p 9187:9187 \
  sidecar
```

!!! note
    Standalone mode provides autonomous analysis, diagnostics, and remediation for managed databases where installing C extensions is not possible. For lowest overhead on self-managed instances, install the C extension directly.
