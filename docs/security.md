# Security

pg_sage is designed to observe and optimize PostgreSQL without accessing user data. This page documents the security model, privacy controls, and operational safeguards.

---

## Why Superuser?

PostgreSQL requires superuser privileges to register background workers via `shared_preload_libraries`. This is a hard requirement of the `postmaster/bgworker.h` API -- there is no way around it.

What sage does with superuser access:

- Registers three background workers (collector, analyzer, briefing)
- Allocates shared memory for circuit breakers, scheduling state, and the EXPLAIN capture ring buffer
- Reads catalog views and statistics collector views
- Executes `CREATE INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY` during maintenance windows (autonomous trust level only)

What sage does **not** do with superuser access:

- Never reads, writes, or modifies row data in user tables
- Never accesses `pg_authid` passwords or connection credentials
- Never creates roles or grants privileges
- Never drops user objects

!!! tip "Least-privilege sidecar mode"
    The MCP sidecar can run without superuser. It connects to PostgreSQL with a regular role that has `SELECT` on `sage.*` tables and `EXECUTE` on `sage.*` functions. See [Deployment](deployment.md) for the recommended role setup.

---

## What sage Accesses

All data sources are read-only catalog views and statistics:

| Source | Purpose |
|---|---|
| `pg_stat_statements` | Query text, execution counts, timing |
| `pg_stat_activity` | Active sessions, idle-in-transaction detection |
| `pg_stat_user_tables` | Table bloat, dead tuples, vacuum status |
| `pg_stat_user_indexes` | Index usage, duplicate detection |
| `pg_indexes` | Index definitions for context assembly |
| `pg_locks` | Lock contention detection |
| `pg_stat_replication` | Replication lag monitoring |
| `pg_stat_bgwriter` / `pg_stat_checkpointer` | Checkpoint health |
| `pg_settings` | Configuration audit |
| `information_schema.columns` | Schema DDL for LLM context |
| `pg_sequences` | Sequence exhaustion detection |
| `pg_database_size()` | Database size tracking |

!!! warning "pg_stat_statements required"
    `pg_stat_statements` must be loaded alongside pg_sage. Without it, query-level analysis (slow queries, regressions, missing indexes) is unavailable.

---

## What sage Never Does

- **Never reads table row data.** All analysis uses aggregate statistics and catalog metadata. Sage queries `pg_stat_user_tables.n_live_tup`, never `SELECT * FROM your_table`.
- **Never accesses credentials or secrets.** Sage does not read `pg_authid.rolpassword` or any password hashes.
- **Never modifies user data.** Autonomous actions are limited to DDL (`CREATE INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY`). Sage never runs `INSERT`, `UPDATE`, or `DELETE` on user tables.
- **Never exposes passwords.** The `sage.llm_api_key` and `sage.slack_webhook_url` GUCs are marked `GUC_SUPERUSER_ONLY` and are not readable by non-superuser roles.
- **Never phones home.** There are zero hardcoded external endpoints. All outbound connections are to user-configured addresses only.

---

## Network Behavior

### LLM disabled (default)

When `sage.llm_enabled = off` (the default), pg_sage makes **zero outbound network connections**. All analysis is performed locally using the rules engine.

### LLM enabled

When `sage.llm_enabled = on`, sage makes HTTP POST requests to the configured `sage.llm_endpoint`. These requests contain **metadata only** -- never row data.

Exactly what is sent to the LLM:

| Data | Example |
|---|---|
| Schema DDL | `CREATE TABLE public.orders (id bigint NOT NULL, ...)` |
| EXPLAIN plans | `Seq Scan on orders (cost=0.00..1234.00 rows=50000 ...)` |
| Parameterized query text | `SELECT * FROM orders WHERE customer_id = $1 AND status = $2` |
| Aggregate metrics | `mean_exec_time=450ms, calls=12000, n_dead_tup=50000` |
| Finding summaries | `Unused index: idx_orders_legacy (0 scans in 30d)` |
| System health numbers | `cache_hit_ratio=99.2, active_backends=12` |

!!! danger "Never sent to LLM"
    Row data, column values, passwords, connection strings, API keys, and personally identifiable information are never included in LLM requests. Query text from `pg_stat_statements` contains parameterized placeholders (`$1`, `$2`), not literal values.

---

## Privacy Controls

Two GUCs provide additional privacy when using external LLM endpoints:

### `sage.redact_queries`

```sql
ALTER SYSTEM SET sage.redact_queries = on;
SELECT pg_reload_conf();
```

When enabled, query text is replaced with the numeric `queryid` in all LLM context. The LLM sees `queryid: 7234891023` instead of the full SQL text. This prevents any query structure from leaving your network.

### `sage.anonymize_schema`

```sql
ALTER SYSTEM SET sage.anonymize_schema = on;
SELECT pg_reload_conf();
```

When enabled, table and column names are replaced with consistent hashes before being sent to the LLM. The LLM sees `table_a3f8.col_1b2c` instead of `orders.customer_id`. Structural relationships (foreign keys, index definitions) are preserved so the LLM can still reason about schema design.

!!! tip "Combine both for maximum privacy"
    Enable both `sage.redact_queries` and `sage.anonymize_schema` to ensure that no identifiable schema or query information leaves your network. The LLM can still analyze execution plans, aggregate metrics, and structural patterns.

---

## Sidecar Security

The MCP sidecar (`sidecar/`) exposes an HTTP/SSE endpoint for AI assistant integration. It has multiple layers of protection:

### API Key Authentication

Set `SAGE_API_KEY` to require a Bearer token on all requests:

```bash
export SAGE_API_KEY="your-secret-key-here"
```

All requests must include the header `Authorization: Bearer <key>`. Requests with missing or invalid keys receive `401 Unauthorized`. Authentication failures are logged with the client IP.

!!! danger "Always set SAGE_API_KEY in production"
    Without `SAGE_API_KEY`, the sidecar accepts all requests without authentication. The sidecar logs a warning at startup when the key is not configured.

### TLS

Enable TLS by setting certificate and key paths:

```bash
export SAGE_TLS_CERT="/path/to/cert.pem"
export SAGE_TLS_KEY="/path/to/key.pem"
```

When configured, the sidecar enforces TLS 1.2 as the minimum protocol version. Without TLS, the sidecar listens on plain HTTP.

### Input Validation

- **Table names** are validated against a strict regex: `^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?$`. No SQL injection is possible through resource URIs or tool arguments.
- **Query IDs** are validated as integers only.
- **Resource URIs** are matched against a known allowlist (`sage://health`, `sage://findings`, etc.).

### Request Limits

- **Body size**: Maximum 1 MB per request (`maxRequestBodySize = 1 << 20`).
- **Rate limiting**: Configurable via `SAGE_RATE_LIMIT` (default: 60 requests per interval).
- **Request timeout**: 30 seconds per MCP request.
- **Pool exhaustion protection**: When the PostgreSQL connection pool is exhausted, database-backed methods return `503` instead of queuing.

### Security Headers

All responses include:

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Cache-Control: no-store`

---

## Circuit Breakers

Circuit breakers prevent sage from becoming the incident during a database crisis.

### Database Circuit Breaker

Tracks consecutive skipped or failed collector/analyzer cycles. When failures exceed the threshold:

| State | Behavior |
|---|---|
| **Closed** | Normal operation. Each successful cycle increments `consecutive_successes`. |
| **Open** | Tripped. Sage stops all analysis and collection. Backs off exponentially. |
| **Dormant** | Extended backoff after prolonged failure. Periodic probe attempts to recover. |

### LLM Circuit Breaker

Independent breaker for the LLM endpoint. Tracks `llm_consecutive_failures` in shared memory. When the LLM is unavailable:

- All LLM-powered features degrade gracefully to Tier 1 (rules engine) behavior
- Briefings fall back to structured templates
- `sage_diagnose()` returns rule-based analysis only
- The breaker auto-recovers after the backoff period

### Daily Token Budget

The `sage.llm_token_budget` GUC (default: 50,000) caps total LLM tokens per day. When exhausted, `sage_llm_available()` returns false and all LLM features are disabled until the next calendar day. Token usage is tracked in shared memory (`llm_tokens_used_today`).

---

## Emergency Stop

Immediately halt all autonomous activity:

```sql
SELECT sage.emergency_stop();
```

This sets `emergency_stopped = true` in shared memory. All background workers check this flag every cycle and skip their work when it is set. The stop is instantaneous -- no current cycle completes.

Resume normal operation:

```sql
SELECT sage.resume();
```

!!! warning "Emergency stop persists across restarts"
    The emergency stop flag lives in shared memory and is reset on server restart. If you need sage to stay stopped across restarts, set `sage.enabled = off` in `postgresql.conf`.

---

## Audit Trail

### Action Log (`sage.action_log`)

Every autonomous action (index creation, reindex, configuration change) is recorded with:

- The SQL that was executed
- The rollback SQL to reverse it
- Execution timestamp and outcome
- The finding that triggered the action

Rollback SQL is available for `sage.rollback_window` minutes (default: 15) after execution. If a performance regression exceeding `sage.rollback_threshold` percent is detected within the window, sage automatically rolls back the change.

### MCP Log (`sage.mcp_log`)

Every MCP sidecar request is logged:

| Column | Content |
|---|---|
| `client_ip` | Source IP of the request |
| `method` | JSON-RPC method (`resources/read`, `tools/call`, etc.) |
| `resource_uri` | Resource URI if applicable |
| `tool_name` | Tool name if applicable |
| `tokens_used` | LLM tokens consumed |
| `duration_ms` | Request processing time |
| `status` | `ok` or `error` |
| `error_message` | Error detail if failed |

Both tables are subject to retention policies (`sage.retention_actions` defaults to 365 days).

---

## Recommendations

Production deployment checklist:

1. **Set `SAGE_API_KEY`** -- never run the sidecar without authentication in production.

2. **Enable TLS** -- set `SAGE_TLS_CERT` and `SAGE_TLS_KEY` for the sidecar. Use a reverse proxy (nginx, Caddy) if you need automatic certificate renewal.

3. **Start in observation mode** -- deploy with `sage.trust_level = 'observation'` and review findings for at least a week before escalating.

    ```sql
    ALTER SYSTEM SET sage.trust_level = 'observation';
    ```

4. **Set a maintenance window** -- restrict autonomous actions to low-traffic periods.

    ```sql
    ALTER SYSTEM SET sage.maintenance_window = '0 2 * * * UTC';
    ```

5. **Enable privacy controls for external LLMs** -- if using a cloud LLM endpoint, enable both redaction settings:

    ```sql
    ALTER SYSTEM SET sage.redact_queries = on;
    ALTER SYSTEM SET sage.anonymize_schema = on;
    ```

6. **Review findings before escalating trust** -- move from `observation` to `advisory` to `autonomous` only after confirming that sage's recommendations are appropriate for your workload.

7. **Set a token budget** -- cap LLM spend with `sage.llm_token_budget` to prevent runaway costs.

8. **Monitor sage itself** -- check `sage.status()` and the circuit breaker state regularly. Sage self-monitors its own schema footprint (`sage.max_schema_size`) and will throttle itself if it grows too large.

9. **Use a dedicated database role for the sidecar** -- grant only `SELECT` on `sage.*` tables and `EXECUTE` on `sage.*` functions. Do not use the PostgreSQL superuser for the sidecar connection.

10. **Keep pg_sage updated** -- security fixes and improvements ship with each release.
