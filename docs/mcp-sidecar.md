# MCP Server

pg_sage includes an MCP (Model Context Protocol) server that exposes database health, findings, and diagnostic tools to AI assistants like Claude Desktop, Cursor, and Copilot.

The MCP server is built into the pg_sage binary and starts automatically. It uses HTTP + SSE (Server-Sent Events) transport.

---

## What is MCP?

The [Model Context Protocol](https://modelcontextprotocol.io/) is an open standard for connecting AI assistants to external data sources and tools. It uses JSON-RPC 2.0 over Server-Sent Events for real-time communication.

---

## Connecting Claude Desktop

Add to your Claude Desktop config:

**Linux/macOS**: `~/.config/claude/claude_desktop_config.json`
**Windows**: `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "pg_sage": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

Restart Claude Desktop. You can now ask questions like:

- "What are my slowest queries?"
- "Show me duplicate indexes"
- "Why is my application slow?"
- "What maintenance does my database need?"

---

## Configuration

The MCP server listens on `:8080` by default. Configure via:

```yaml
# config.yaml
mcp:
  enabled: true
  listen_addr: "0.0.0.0:8080"
```

Or via environment variable:

```bash
export SAGE_MCP_PORT=8080
```

Or via CLI flag:

```bash
./pg_sage --mcp-addr 0.0.0.0:8080
```

---

## Resources

MCP resources provide read-only access to database state. Clients read resources via `resources/read` requests.

### `sage://health`

System health overview including connections, cache hit ratio, disk usage, and circuit breaker status.

```json
{
  "version": "0.7.0",
  "mode": "standalone",
  "circuit_state": "closed",
  "llm_circuit_state": "closed",
  "emergency_stopped": false,
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
sage://schema/public.orders
```

### `sage://stats/{table}`

Table size, row counts, dead tuples, vacuum status, and index usage statistics.

```
sage://stats/public.orders
```

### `sage://slow-queries`

Top 20 slow queries from `pg_stat_statements`, ordered by total execution time.

### `sage://explain/{queryid}`

Cached EXPLAIN plan for a specific query ID.

---

## Tools

MCP tools allow AI assistants to invoke actions via `tools/call` requests.

### `diagnose`

Interactive diagnostic analysis. The LLM reasons through problems step by step.

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

Get index recommendations for a specific table based on query patterns.

**Input:**

```json
{ "table": "public.orders" }
```

**Output:** Suggested CREATE INDEX statements with rationale.

### `review_migration`

Review DDL or migration SQL for production safety issues.

**Input:**

```json
{ "ddl": "ALTER TABLE orders ADD COLUMN status text DEFAULT 'pending';" }
```

**Output:** Risk assessment with specific warnings and safer alternatives.

### `sage_status`

Returns current pg_sage status including trust level, circuit breaker state, and collection stats.

### `sage_emergency_stop`

Immediately halt all autonomous actions.

### `sage_resume`

Resume after an emergency stop.

### `sage_briefing`

Generate a standalone health briefing (works without the `diagnose` tool's LLM dependency).

---

## Prompts

MCP prompts provide pre-built conversation templates.

| Prompt | Arguments | Description |
|---|---|---|
| `investigate_slow_query` | `queryid` (required) | Investigate why a specific query is slow |
| `review_schema` | `table` (required) | Review the schema design of a table |
| `capacity_plan` | none | Analyze current database capacity and growth trends |

---

## Authentication

Set `SAGE_API_KEY` to require a Bearer token on all requests:

```bash
export SAGE_API_KEY="your-secret-key"
```

When set, all MCP requests must include:

```
Authorization: Bearer your-secret-key
```

Always set an API key in production.

---

## Rate Limiting

- Default: 60 requests per minute per IP
- Configurable via `SAGE_RATE_LIMIT`
- Respects `X-Forwarded-For` for proxied clients
- Rate-limited requests receive HTTP 429

---

## Prometheus Metrics

The Prometheus endpoint at `:9187/metrics` exports:

| Metric | Type | Description |
|---|---|---|
| `pg_sage_findings_total{severity}` | gauge | Open findings by severity |
| `pg_sage_connection_up` | gauge | Database connectivity (1=up, 0=down) |
| `pg_sage_database_size_bytes` | gauge | Database size |
| `pg_sage_cache_hit_ratio` | gauge | Buffer cache hit ratio |
| `pg_sage_collector_last_run_timestamp` | gauge | Last collection timestamp |
| `pg_sage_llm_calls_total{model,purpose}` | counter | LLM API calls |
| `pg_sage_llm_circuit_open{model}` | gauge | LLM circuit breaker state |
| `pg_sage_optimizer_recommendations_total{action_level}` | counter | Optimizer recommendations |
| `pg_sage_executor_actions_total{outcome}` | counter | Executor actions by outcome |
