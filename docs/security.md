# Security

pg_sage is designed to observe and optimize PostgreSQL without accessing user data. This page documents the security model, privacy controls, and operational safeguards.

---

## Required Database Grants

pg_sage connects as a regular database user -- no superuser required.

```sql
CREATE USER sage_agent WITH PASSWORD 'YOUR_PASSWORD';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;    -- for index creation
GRANT pg_signal_backend TO sage_agent;           -- for query termination
```

The sidecar bootstraps the `sage` schema and tables on first connect. If you prefer to pre-create:

```sql
CREATE SCHEMA sage;
GRANT ALL ON SCHEMA sage TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent;
```

---

## What pg_sage Accesses

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
| `information_schema.columns` | Schema DDL for LLM context |
| `pg_sequences` | Sequence exhaustion detection |
| `pg_database_size()` | Database size tracking |

`pg_stat_statements` must be loaded on the target database. Without it, query-level analysis (slow queries, regressions, missing indexes) is unavailable.

---

## What pg_sage Never Does

- **Never reads table row data.** All analysis uses aggregate statistics and catalog metadata.
- **Never accesses credentials or secrets.** Does not read `pg_authid.rolpassword` or password hashes.
- **Never modifies user data.** Autonomous actions are limited to DDL (`CREATE INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`). Never runs `INSERT`, `UPDATE`, or `DELETE` on user tables.
- **Never uses ALTER SYSTEM.** Configuration changes are made through the YAML config file, not database-side settings.
- **Never phones home.** Zero hardcoded external endpoints. All outbound connections are to user-configured addresses only.

---

## Trust Model

pg_sage uses graduated trust to control autonomous actions:

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Day 0-7 | No actions -- findings only |
| **advisory** | Day 8-30 | SAFE: drop unused/duplicate indexes, VACUUM |
| **autonomous** | Day 31+ | MODERATE: create indexes, reindex |

HIGH-risk actions always require manual confirmation, regardless of trust level.

The executor checks all of these gates before acting:

1. Trust level matches the action's risk category
2. Trust ramp timeline has been met
3. Per-tier toggles are enabled
4. Maintenance window is active (if configured)
5. Emergency stop is not set
6. Database is not a replica

---

## Advisory Lock

pg_sage acquires PostgreSQL advisory lock `710190109` (`hashtext('pg_sage')`) at startup. This prevents multiple sidecar instances from running against the same database simultaneously. If the lock is held, the sidecar waits or exits.

---

## Network Behavior

### LLM disabled (default)

When `llm.enabled: false` (the default), pg_sage makes **zero outbound network connections** beyond the PostgreSQL connection. All analysis is performed locally using the rules engine.

### LLM enabled

When `llm.enabled: true`, pg_sage makes HTTP POST requests to the configured `llm.endpoint`. These requests contain **metadata only** -- never row data.

What is sent to the LLM:

| Data | Example |
|---|---|
| Schema DDL | `CREATE TABLE public.orders (id bigint NOT NULL, ...)` |
| EXPLAIN plans | `Seq Scan on orders (cost=0.00..1234.00 rows=50000 ...)` |
| Parameterized query text | `SELECT * FROM orders WHERE customer_id = $1 AND status = $2` |
| Aggregate metrics | `mean_exec_time=450ms, calls=12000, n_dead_tup=50000` |
| Finding summaries | `Unused index: idx_orders_legacy (0 scans in 30d)` |

What is **never** sent: row data, column values, passwords, connection strings, API keys, PII. Query text from `pg_stat_statements` contains parameterized placeholders (`$1`, `$2`), not literal values.

---

## API Security

### API Key Authentication

Set `SAGE_API_KEY` to require a Bearer token on all API requests:

```bash
export SAGE_API_KEY="your-secret-key-here"
```

All requests must include `Authorization: Bearer <key>`. Requests with missing or invalid keys receive `401 Unauthorized`.

Always set `SAGE_API_KEY` in production. Without it, the sidecar accepts all requests without authentication.

### TLS

Enable TLS by setting certificate and key paths:

```bash
export SAGE_TLS_CERT="/path/to/cert.pem"
export SAGE_TLS_KEY="/path/to/key.pem"
```

When configured, the sidecar enforces TLS 1.2 as the minimum protocol version.

### Input Validation

- **Table names** are validated against a strict regex. No SQL injection is possible through resource URIs or tool arguments.
- **Query IDs** are validated as integers only.
- **Resource URIs** are matched against a known allowlist.

### Request Limits

- **Body size**: Maximum 1 MB per request.
- **Rate limiting**: Configurable via `SAGE_RATE_LIMIT` (default: 60 requests per minute per IP).
- **Request timeout**: 30 seconds per API request.
- **Pool exhaustion protection**: When the connection pool is exhausted, database-backed methods return `503`.

### Security Headers

All responses include `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and `Cache-Control: no-store`.

---

## Circuit Breakers

Circuit breakers prevent pg_sage from becoming the incident during a database crisis.

### Database Circuit Breaker

Tracks consecutive failed collector/analyzer cycles. When failures exceed the threshold, the breaker opens and pg_sage stops all collection and analysis. Backs off exponentially with periodic probe attempts to recover.

### LLM Circuit Breaker

Independent breaker for the LLM endpoint. When the LLM is unavailable, all LLM-powered features degrade gracefully to Tier 1 (rules engine) behavior. The breaker auto-recovers after the backoff period.

### Daily Token Budget

The `llm.token_budget` setting (default: 50,000) caps total LLM tokens per day. When exhausted, all LLM features are disabled until the next calendar day.

---

## Emergency Stop

Halt all autonomous activity immediately by setting the emergency stop flag in `sage.config`:

```sql
UPDATE sage.config SET value = 'true' WHERE key = 'emergency_stop';
```

Or use the web UI emergency stop button, or the REST API `POST /api/v1/emergency-stop`.

Resume with:

```sql
UPDATE sage.config SET value = 'false' WHERE key = 'emergency_stop';
```

Or use the web UI resume button, or the REST API `POST /api/v1/resume`.

---

## Audit Trail

### Action Log (`sage.action_log`)

Every autonomous action is recorded with:

- The SQL that was executed
- The rollback SQL to reverse it
- Execution timestamp and outcome
- The finding that triggered the action
- Before/after state

### API Request Log

API requests are logged for audit purposes.

Both tables are subject to retention policies (configurable via `retention.actions_days`).

---

## Production Checklist

1. **Set `SAGE_API_KEY`** -- never run the API server without authentication in production.
2. **Enable TLS** -- set `SAGE_TLS_CERT` and `SAGE_TLS_KEY`. Use a reverse proxy for automatic certificate renewal.
3. **Start in observation mode** -- deploy with `trust.level: observation` and review findings for at least a week.
4. **Set a maintenance window** -- restrict autonomous actions to low-traffic periods.
5. **Review findings before escalating trust** -- move to `advisory` then `autonomous` only after confirming recommendations are appropriate.
6. **Set a token budget** -- cap LLM spend with `llm.token_budget`.
7. **Use a dedicated database role** -- grant only the required privileges listed above.
8. **Monitor pg_sage itself** -- check Prometheus metrics and circuit breaker state.
