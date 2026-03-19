# pg_sage MCP Sidecar

A Go sidecar that exposes pg_sage's PostgreSQL extension functionality via the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) and a Prometheus metrics endpoint.

## Quick Start

```bash
# Build
go build -o sage-sidecar .

# Run (uses defaults: PG on localhost:5432, MCP on :5433, Prometheus on :9187)
./sage-sidecar

# Or with Docker
docker build -t sage-sidecar .
docker run -e SAGE_DATABASE_URL=postgres://postgres@host.docker.internal:5432/postgres?sslmode=disable sage-sidecar
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `SAGE_DATABASE_URL` | `postgres://postgres@localhost:5432/postgres?sslmode=disable` | PostgreSQL connection string |
| `SAGE_MCP_PORT` | `5433` | MCP server port |
| `SAGE_PROMETHEUS_PORT` | `9187` | Prometheus metrics port |
| `SAGE_RATE_LIMIT` | `60` | Max requests per minute per client IP |
| `SAGE_TOKEN_BUDGET` | `10000` | Max tokens per MCP request |

## MCP Transport

The sidecar implements MCP over HTTP + SSE:

1. Client opens `GET /sse` to receive an SSE stream
2. Server sends an `endpoint` event with a session URL
3. Client sends JSON-RPC 2.0 requests to `POST /messages?sessionId=xxx`
4. Server pushes responses back on the SSE stream

## Resources

| URI | Description |
|---|---|
| `sage://health` | Database health snapshot |
| `sage://findings` | Open findings from pg_sage |
| `sage://slow-queries` | Recently observed slow queries |
| `sage://schema/{table}` | Column and index info for a table |
| `sage://stats/{table}` | pg_stat_user_tables stats |
| `sage://explain/{queryid}` | Cached EXPLAIN plan |

## Tools

| Tool | Description |
|---|---|
| `diagnose` | Interactive diagnostic question |
| `briefing` | Health briefing |
| `suggest_index` | Index suggestions for a table |
| `review_migration` | Review DDL for risks |

## Prompts

| Prompt | Arguments |
|---|---|
| `investigate_slow_query` | `queryid` |
| `review_schema` | `table` |
| `capacity_plan` | (none) |

## Prometheus Metrics

Available at `GET :9187/metrics`:

- `pg_sage_findings_total{severity}` — open findings by severity
- `pg_sage_info{version}` — version info
- `pg_sage_circuit_breaker_state{breaker}` — circuit breaker states
- `pg_sage_status_*` — additional metrics from `sage.status()`
