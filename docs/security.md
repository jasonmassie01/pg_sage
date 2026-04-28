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

The sidecar bootstraps the `sage` schema and tables on first connect. Either
connect with a role that can create that schema, or pre-create it and grant the
sidecar role ownership/write privileges:

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
- **Never modifies user data.** Autonomous actions are limited to maintenance and schema operations such as `ANALYZE`, guarded index DDL, and approved incident actions. Never runs `INSERT`, `UPDATE`, or `DELETE` on user tables.
- **Never directly executes high-risk DDL.** Rewrite-heavy or forward-fix-only schema changes become migration-safety cases with preflight evidence, generated scripts, verification SQL, and PR/CI metadata for human review.
- **Never drops replication slots or changes sequence capacity autonomously.** WAL/replication playbooks are read-only diagnostics, and sequence-exhaustion remediation is generated as a reviewed forward-fix migration.
- **Never runs maintenance during known IO saturation.** Bloat autopilot blocks autonomous `VACUUM` candidates when IO pressure evidence is present and emits script/review output instead.
- **Never rewrites application queries autonomously.** Query rewrites are generated as reviewable PR/script artifacts with semantic and plan verification steps.
- **Never promotes role-level memory settings without review.** Repeated per-query `work_mem` patterns can become a reviewed `ALTER ROLE` candidate, but they require approval because the blast radius is role-wide.
- **Never uses ALTER SYSTEM.** Configuration changes are made through the YAML config file, not database-side settings.
- **Never phones home.** Zero hardcoded external endpoints. All outbound connections are to user-configured addresses only.

---

## Trust Model

pg_sage uses graduated trust to control autonomous actions:

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Configured | No actions -- cases and recommendations only |
| **advisory** | Configured | Queue or execute SAFE actions based on policy |
| **autonomous** | Configured | SAFE + approved MODERATE actions, bounded by maintenance windows |

HIGH-risk actions always require manual approval, regardless of trust level.

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

### Session Authentication

The web UI and `/api/v1/*` endpoints use session-cookie authentication. On
first startup against a metadata database with no users, pg_sage creates
`admin@pg-sage.local` and prints a one-time initial password to stderr.

API clients log in and reuse the `sage_session` cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
```

`SAGE_API_KEY` is a legacy config field and does not secure the current v0.9
web/API path.

### TLS

pg_sage currently serves HTTP. Terminate TLS at a reverse proxy, Kubernetes
Ingress, Cloud Run, load balancer, or other trusted edge. Restrict direct access
to the API/dashboard listener to trusted networks.

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

The `llm.token_budget_daily` setting (default: 500,000) caps total LLM tokens per day. When exhausted, all LLM features are disabled until the next calendar day.

---

## Emergency Stop

Halt all autonomous activity immediately by setting the emergency stop flag in `sage.config`:

```sql
UPDATE sage.config SET value = 'true' WHERE key = 'emergency_stop';
```

Or use the web UI emergency stop button, or the authenticated REST API:

```bash
curl -b cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/emergency-stop --data '{}'
```

Resume with:

```sql
UPDATE sage.config SET value = 'false' WHERE key = 'emergency_stop';
```

Or use the web UI resume button, or the authenticated REST API:

```bash
curl -b cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/resume --data '{}'
```

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

1. **Protect the dashboard/API listener** -- use a private network, reverse proxy, or identity-aware edge.
2. **Terminate TLS at the edge** -- do not expose plain HTTP directly to the internet.
3. **Start in observation mode** -- deploy with `trust.level: observation` and review findings for at least a week.
4. **Set a maintenance window** -- restrict autonomous actions to low-traffic periods.
5. **Review findings before escalating trust** -- move to `advisory` then `autonomous` only after confirming recommendations are appropriate.
6. **Set a token budget** -- cap LLM spend with `llm.token_budget_daily`.
7. **Use a dedicated database role** -- grant only the required privileges listed above.
8. **Capture and rotate the initial admin password** -- then use named users or OAuth for operators.
9. **Monitor pg_sage itself** -- check Prometheus metrics and circuit breaker state.
