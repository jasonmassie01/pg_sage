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

Sage SRE investigations need only `pg_monitor`; without `pg_read_all_stats` their activity
probes report `no_privilege` instead of a partial answer. See
[Sage SRE: permissions and data flow](sage-sre-permissions-and-data-flow.md).

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
- **Never ignores live DDL risk.** Active workload, pending locks, replica lag, large table size, and missing lock-timeout evidence keep DDL in reviewed PR/script mode.
- **Never drops replication slots or changes sequence capacity autonomously.** WAL/replication playbooks are read-only diagnostics, and sequence-exhaustion remediation is generated as a reviewed forward-fix migration.
- **Never runs maintenance during known IO saturation.** Bloat autopilot blocks autonomous `VACUUM` candidates when IO pressure evidence is present and emits script/review output instead.
- **Never rewrites application queries autonomously.** Query rewrites are generated as reviewable PR/script artifacts with semantic and plan verification steps.
- **Never parameterizes application code autonomously.** Parameterization candidates are change-control artifacts, not direct database actions.
- **Never promotes role-level memory settings without review.** Repeated per-query `work_mem` patterns can become a reviewed `ALTER ROLE` candidate, but they require approval because the blast radius is role-wide.
- **Never uses ALTER SYSTEM.** Configuration changes are made through the YAML config file, not database-side settings.
- **Never phones home.** Zero hardcoded external endpoints. All outbound connections are to user-configured addresses only.

---

## Trust Model

pg_sage uses graduated trust to control autonomous actions:

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Configured | No actions -- cases and recommendations only |
| **advisory** | Configured | Auto executes eligible typed SAFE actions; higher risk queues |
| **autonomous** | Configured | Auto executes eligible typed SAFE/MODERATE actions; HIGH queues |

HIGH-risk actions always require manual approval, regardless of trust level.

The executor checks all of these gates before acting:

1. Execution mode is explicitly `auto` for automatic mutation
2. The per-database executor is enabled
3. Trust level matches the typed action contract's risk category
4. Trust ramp timeline and per-tier toggles have been met
5. Maintenance window is active when the contract requires it
6. Emergency Stop is not set
7. Database is not a replica

`manual` disables all background queueing and execution at every trust level;
trust never promotes it to `auto`. Emergency Stop and executor disablement are
rechecked for each candidate and immediately before the mutating SQL call.

---

## Advisory Lock

pg_sage acquires PostgreSQL advisory lock `710190109` (`hashtext('pg_sage')`) at startup. This prevents multiple sidecar instances from running against the same database simultaneously. If the lock is held, the sidecar waits or exits.

---

## Network Behavior

### No LLM configured (default)

LLM features are on by default (`llm.enabled: true`), but a provider is contacted only once both `llm.endpoint` and `llm.api_key` are set. Until then, or with `llm.enabled: false`, pg_sage makes **no LLM requests**: all analysis is performed locally using the rules engine, and startup logs one line saying the LLM is not configured.

### LLM configured

When `llm.enabled: true` and both `llm.endpoint` and `llm.api_key` are set, pg_sage makes HTTP POST requests to the configured `llm.endpoint`. These requests contain **metadata only** -- never row data.

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

`SAGE_API_KEY` is a legacy config field and does not secure the current v1
web/API path.

### SSO Account Linking

SSO users are matched on the identity provider's issuer and subject, never on
email. A first SSO sign-in whose verified email belongs to an existing account
is refused (HTTP `403`) until the account is linked by one of these explicit
acts:

- **Link SSO (self-service).** A user signed in with a password opens their
  account page (click their email in the sidebar, `#/profile`) and chooses
  **Link SSO**. `GET /api/v1/auth/oauth/authorize?intent=link` requires that
  session. After the provider sign-in, the identity is bound to the signed-in
  user if the provider asserts `email_verified`, the verified email equals the
  account email (case-insensitive), the account has no identity yet, and the
  identity is not bound to another account. Otherwise the callback returns
  `409` (or `401` for an unverified email). The account keeps its id, role and
  password.
- **One-time link grant (admin).** For a user who cannot sign in with a
  password, an admin issues a grant with
  `POST /api/v1/users/{id}/oidc-link-grant` or **Issue SSO link** on the Users
  page. The response carries a token once; the dashboard shows it as a
  `#/link-sso?grant=...` link. The grant works once, expires after 15 minutes,
  is replaced by a newer grant for the same user, and is stored only as a
  SHA-256 hash. The holder opens the link, which calls
  `POST /api/v1/auth/oauth/link-grant`, then signs in at the provider; the
  same email and uniqueness checks apply.
- **SSO-only users.** `POST /api/v1/users` with `"sso_only": true` and no
  password creates an account without a password. It cannot sign in until an
  admin issues it a link grant.

An admin removes a link with `DELETE /api/v1/users/{id}/oidc` (**Unlink SSO**
on the Users page); this also ends the user's sessions. `GET /api/v1/users`
reports `sso_linked`, `sso_issuer` and `password_login` for each user, and
`GET /api/v1/auth/sso` reports the signed-in user's own status. Link, unlink,
grant issue and grant use are recorded in `sage.auth_audit` with the acting and
target user ids; audit rows never contain emails or grant tokens.

The callback is a browser navigation, so when the request accepts HTML a
failure redirects to the dashboard with an `sso_error` code
(`link_required`, `link_conflict`, `unverified` or `failed`) that the login
page, or the account page for a signed-in link, shows as a message. Other
clients receive the JSON status (`403`, `409` or `401`). The grant landing
page removes the grant from the address and browser history as soon as it
reads it.

### SSO Login Validation

pg_sage's SSO login follows OpenID Connect for `oidc` and `google`
providers:

- **PKCE.** Every authorization request carries an S256 `code_challenge`; the
  callback redeems the code with its `code_verifier`. GitHub (OAuth 2, no
  `id_token`) uses PKCE too.
- **Nonce.** Each login sends a fresh random `nonce`, kept server-side with
  the single-use `state`. An `id_token` whose nonce differs, or has none, is
  refused with `401`.
- **`id_token` validation.** The token endpoint must return an `id_token`.
  pg_sage verifies its signature against the provider's JWKS (`jwks_uri`
  from discovery, through `coreos/go-oidc`), its issuer, its audience (the
  client id) and its expiry. Userinfo is consulted only when the token has
  no email, and must name the same subject.
- **Discovery.** The discovery document must name the configured issuer
  (`oauth.issuer_url`, trailing slash ignored) and publish `jwks_uri`;
  otherwise SSO stays disabled and startup logs why.

Every refusal is recorded in `sage.auth_audit` as `login_failed` with a
reason code (`nonce_mismatch`, `id_token_invalid`, `id_token_missing`,
`state_invalid`, `subject_mismatch`, `email_unverified`, `link_required`,
`not_authorized`, `exchange_failed`).

### Group to Role Mapping

Map IdP groups to pg_sage roles with `oauth.role_mapping`:

```yaml
oauth:
  groups_claim: groups          # id_token (or userinfo) claim with the groups
  unmapped_users: deny          # deny (default) | default_role
  role_mapping:
    - {group: pg-sage-admins,    role: admin}
    - {group: dba,               role: operator}
    - {group: engineering,       role: viewer}
```

- Group names match exactly (case-sensitive). The claim may be an array of
  strings or a single string.
- A user in several mapped groups gets the highest role.
- The IdP is the source of truth: the mapped role is applied at every SSO
  login, and a change is audited as `user_role_changed` with
  `"source": "idp"`. A role set in the UI is overwritten at the next login.
  The last admin is never demoted this way; pg_sage logs a warning instead.
- **Unmapped users are denied by default** (`403`, `sso_error=not_authorized`):
  listing groups names who may sign in, and an IdP tenant usually holds far
  more people than that. Set `unmapped_users: default_role` to give them
  `oauth.default_role` instead.
- Without `role_mapping`, every SSO user gets `oauth.default_role`
  (`viewer`), as before.

### Authentication Audit

`sage.auth_audit` records, with actor and target user ids, time and the
client address (`source_ip`, honouring `api.trusted_proxies`):

| Event | When |
|---|---|
| `login_succeeded` / `login_failed` | Password and SSO sign-ins (`detail.method`, `detail.reason`) |
| `break_glass_login` / `break_glass_login_failed` | Every break-glass attempt |
| `user_created`, `user_deleted` | Admin user administration |
| `user_role_changed` | Role changes from the API (`source: api`) or the IdP (`source: idp`) |
| `oidc_linked`, `oidc_unlinked`, `oidc_link_grant_*` | Account linking |

Rows never contain passwords, tokens or email addresses; a failed password
login names the targeted account by id when it exists.

### Break-glass Admin

A local admin for when the IdP is down. It is off unless configured:

```yaml
oauth:
  break_glass:
    enabled: true
    # password_hash: prefer SAGE_BREAK_GLASS_PASSWORD_HASH or its _FILE form
```

```bash
# bcrypt hash of the break-glass password (keep the password offline)
htpasswd -bnBC 12 "" 'the-break-glass-password' | tr -d ':\n'
export SAGE_BREAK_GLASS_PASSWORD_HASH_FILE=/run/secrets/pg_sage_break_glass_hash

curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST https://sage.example.com/api/v1/auth/break-glass \
  --data '{"password":"the-break-glass-password"}'
```

- It signs in as the dedicated admin `break-glass@pg-sage.local`, which has
  no stored password (the normal login cannot use it). If that email belongs
  to another account, break-glass refuses (`409`).
- The session lasts one hour.
- Every use is logged at ERROR, recorded as `break_glass_login`, and
  broadcast as a critical `security_break_glass` notification to **every
  enabled notification channel**, regardless of notification rules. Configure
  at least one channel. Failed attempts are audited and rate-limited per
  client address (5 per 15 minutes).
- After use, review what the session did and rotate the password.

### Secrets at Rest

Secrets set through the API or UI (`llm.api_key`, `clone.dle_token`,
`alerting.slack_webhook_url`, `alerting.pagerduty_routing_key`) are stored in
`sage.config` sealed with AES-256-GCM under a key derived from
`encryption_key` (argon2id with the deployment's random salt). A stored value
looks like `sage-enc:v1:<key id>:<ciphertext>`; the key id is a public
fingerprint of the key. Each value is bound to its key name and scope, so it
cannot be copied to another key. Audit rows never hold the value or its
ciphertext.

- **Key source.** `encryption_key`, `SAGE_ENCRYPTION_KEY`, or
  `SAGE_ENCRYPTION_KEY_FILE` (a file a secrets manager fills; trailing newline
  trimmed; setting both the variable and `_FILE` is an error).
- **Startup migration.** With a key configured, startup seals existing
  plaintext secret rows and logs how many.
- **No key.** Secrets stay plaintext, as before, and startup warns once when
  any are stored.
- **Rotation.** Set the new passphrase as `encryption_key` and the old one as
  `encryption_key_previous` (or `SAGE_ENCRYPTION_KEY_PREVIOUS[_FILE]`) and
  restart: values sealed under the old key are read and re-sealed under the
  new one. Then remove `encryption_key_previous`. A value no configured key
  can open is ignored with an ERROR naming its key and key id. Rotation
  currently covers config secrets only: per-database passwords and
  notification channel secrets are still sealed with `encryption_key` alone
  and must be re-entered after a rotation.

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

### Authentication Log (`sage.auth_audit`)

Logins, break-glass use, and user and role changes, with actor, time and
client address; see [Authentication Audit](#authentication-audit).

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
   With SSO, set `oauth.role_mapping`, and configure a break-glass admin plus a notification channel.
9. **Set `encryption_key`** (or `SAGE_ENCRYPTION_KEY_FILE`) -- API-set secrets are then encrypted at rest.
10. **Monitor pg_sage itself** -- check Prometheus metrics and circuit breaker state.
