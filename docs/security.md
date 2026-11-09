# Security

pg_sage observes and optimizes PostgreSQL through catalog views, statistics and query plans. This page documents what it can read and change, the security model, privacy controls, and operational safeguards.

For Databricks deployments, also read the
[native implementation and identity boundaries](databricks-native.md#architecture-and-identity).
The password-user and session/SSO examples below describe standalone deployments.
Databricks Apps uses platform sign-in, local pg_sage roles and a separate App
service principal for outbound OAuth. Use the [Lakebase grants and limits](lakebase.md)
instead of assuming every standalone grant or setting is available.

---

## Required Database Grants

pg_sage connects as a regular database user -- no superuser required. Connect it
as a dedicated non-superuser role without `BYPASSRLS`: as a superuser it bypasses
row-level security and holds every agent role, so startup logs a warning and the
posture check raises AP-14 ([agent governance](agent-governance.md)). On managed
services, `rds_superuser`, `cloudsqlsuperuser`, `azure_pg_admin` and the like are
admin roles to run these grants from, not roles to run pg_sage as. The grants are
the same on PostgreSQL 14 to 18.

```sql
CREATE USER sage_agent WITH PASSWORD 'YOUR_PASSWORD';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;    -- new objects in this schema
-- CREATE INDEX and CREATE STATISTICS need table ownership; for autonomous index work:
-- GRANT <table owner role> TO sage_agent;
GRANT pg_signal_backend TO sage_agent;           -- for query termination
```

`CREATE` on a schema lets pg_sage create new objects there; grant it on each
schema with tables it should index. `CREATE INDEX`, `CREATE STATISTICS` and other
table DDL also need the privileges of the table's owner, which pg_sage gets as a
member of the owner role. That membership is the largest grant: it is what makes
the [SQL boundary](#what-pg_sage-can-change) matter. On PostgreSQL 17+,
`GRANT pg_maintain TO sage_agent` lets it `VACUUM`, `ANALYZE` and `REINDEX` tables
it does not own. `GET /api/v1/onboarding/trust` (the dashboard's onboarding guide)
lists which of these grants the role holds, with the SQL for each missing one.
Agent governance creates and retires agent roles only when pg_sage's role has
`CREATEROLE` on PostgreSQL 16+ (`ALTER ROLE sage_agent CREATEROLE`); without it,
or below 16, agent governance is posture-only ([agent governance](agent-governance.md)).

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

## What pg_sage can read

Most of what pg_sage reads is catalog views and statistics:

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

Beyond the catalog it reads:

| What | When | What it keeps |
|---|---|---|
| `EXPLAIN` plans | Optimizer, query analysis and the EXPLAIN endpoint (`explain.enabled`; REST and MCP need the operator role or the `propose` scope) | The plan; no rows |
| `EXPLAIN ANALYZE` | When the caller asks for it on the EXPLAIN endpoint, inside a `READ ONLY` transaction with `explain.timeout_ms`, and only for a statement pg_sage proves free of side effects (anything else gets the plan alone, with `analyze_refused`) | Executing the statement reads the rows it touches; pg_sage keeps the plan, timings and row counts, never result rows |
| Retention row counts | A table whose owner declared a [retention contract](configuration.md#retention-contracts): the dry run counts eligible rows, and each batch reports how many it deleted | Counts only |
| Live session text | `pg_stat_activity.query` for lock and incident evidence (`pg_read_all_stats`) | Literals redacted, cut to 256 bytes ([LLM data](#llm-configured)) |

Agents get plan-only EXPLAIN under their own database role through agent
governance (`analyze` is refused), so a plan never describes a table the agent
cannot read. pg_sage never reads `pg_authid.rolpassword` or password hashes.

---

## What pg_sage can change

Each kind of change has a gate. None of them is free-form SQL run as pg_sage's
role: the API and MCP expose typed actions and tools
([MCP tools and scopes](#mcp-tools-and-scopes)).

| Change | What runs | Gate |
|---|---|---|
| Executor actions | `CREATE INDEX CONCURRENTLY`, `DROP INDEX`, `REINDEX`, `VACUUM`, `ANALYZE`, `CREATE STATISTICS`, table storage parameters, allowlisted `ALTER SYSTEM` settings, query hints, cancelling or terminating a session | A typed action contract per class with its risk; execution mode, trust level, trust ramp, maintenance window, emergency stop and the standing policy's refusal set ([Trust Model](#trust-model), [One trust system](configuration.md#one-trust-system), [standing policy](configuration.md#agent-native-autonomy)). HIGH-risk actions always need approval |
| Retention deletes | `DELETE` of application rows older than the declared window, at most 1,000 rows per batch | Only on a table whose owner declared a [retention contract](configuration.md#retention-contracts) with `declare_table_contract` (`approve` scope) naming the column; a dry run at least 24 hours old first; policy consent to the `retention` class; a batch with any row outside the predicate or relation rolls back. Irreversible, so the trust ledger never runs it above L1 |
| Agent roles | `CREATE ROLE`/`ALTER ROLE` of `sage_agent_*` and `sage_agentb_*` roles and their grants; `NOLOGIN` and session termination on a kill | Typed contracts `guard_role_ensure` and `guard_role_retire` through the executor, operator-approved, and only when pg_sage's role has `CREATEROLE` on PostgreSQL 16+; a kill or freeze only takes access away and needs no approval ([agent governance](agent-governance.md#kill-switch-freeze-and-unfreeze)) |
| Managed provider settings | RDS, Aurora and Cloud SQL parameter changes through the provider API | Proposed only from tuning findings and for allowlisted settings ([Model output is data](#model-output-is-data)) |
| The `sage` schema | pg_sage's own tables, functions and audit chains | Created and migrated at startup; pg_sage writes there freely, and retention trims it (`retention.*`) |

Model-authored SQL never runs through the executor ([Model-authored SQL](#model-authored-sql)).

### Safeguards on those changes

- **Never directly executes high-risk DDL.** Rewrite-heavy or forward-fix-only schema changes become migration-safety cases with preflight evidence, generated scripts, verification SQL, and PR/CI metadata for human review.
- **Never ignores live DDL risk.** Active workload, pending locks, replica lag, large table size, and missing lock-timeout evidence keep DDL in reviewed PR/script mode.
- **Never resolves its own DDL through `public`.** Every executor DDL session (transactional
  and `CONCURRENTLY`) runs with `search_path = pg_catalog, pg_temp`, so a function or
  operator another role plants in `public` cannot bind into an index pg_sage builds (the
  CVE-2018-1058 class). Targets must be schema-qualified: an unqualified table fails with
  `relation "..." does not exist` and the action is recorded `failed`; the optimizer names
  the analyzed table's schema in every index it recommends. Operator classes are the one
  exception, because creating one takes a superuser or a trusted extension, so another role
  cannot plant one. A bare operator class in a `CREATE INDEX` (`gin_trgm_ops`,
  `vector_l2_ops`) runs with its schema named: pg_catalog's class when there is one, else
  the class of that name and access method that belongs to an installed extension. A class
  outside every extension, or defined by more than one extension, is refused before the
  statement runs. Functions, types and tables are never resolved this way. Before pg_sage
  drops an index, it
  re-renders the index's re-create rollback under the same path, so an extension operator
  class or a user type in a partial-index predicate comes back schema-qualified and the
  rollback still runs; index replacement reads the old index the same way. The session's
  settings are reset before the connection returns to the pool; a connection whose reset
  fails is closed instead.
  Workload `EXPLAIN` and HypoPG what-if sessions keep the application's `search_path`.
- **Never drops replication slots or changes sequence capacity autonomously.** WAL/replication playbooks are read-only diagnostics, and sequence-exhaustion remediation is generated as a reviewed forward-fix migration.
- **Never runs maintenance during known IO saturation.** Bloat autopilot blocks autonomous `VACUUM` candidates when IO pressure evidence is present and emits script/review output instead.
- **Never rewrites application queries autonomously.** Query rewrites are generated as reviewable PR/script artifacts with semantic and plan verification steps.
- **Never parameterizes application code autonomously.** Parameterization candidates are change-control artifacts, not direct database actions.
- **Never promotes role-level memory settings without review.** Repeated per-query `work_mem` patterns can become a reviewed `ALTER ROLE` candidate, but they require approval because the blast radius is role-wide.
- **Database settings are provider-gated.** Supported typed configuration actions
  require their execution/approval contracts; YAML config is separate from
  PostgreSQL settings. Lakebase system settings are provider-managed, and
  `ALTER SYSTEM` procedures are withheld. Supported database-level settings
  require database ownership and parameter-specific validation.
- **Never trusts a function outside `pg_catalog` or an allowlisted extension in an expression index.** Before recommending an index whose keys or predicate call a function, pg_sage checks every overload of that name in `pg_catalog` and every member function of an allowlisted extension (`btree_gin`, `btree_gist`, `citext`, `cube`, `earthdistance`, `fuzzystrmatch`, `hstore`, `intarray`, `ltree`, `pg_trgm`, `postgis`). A function another role planted in `public` (or beside an extension in its schema) is never counted: a name with no such overload is refused with `function_schema_not_allowed`, a call qualified by another schema (`public.lower(...)`) is looked up only in that schema, a quoted function or schema name, a comment, or a character that pg_sage and PostgreSQL read differently (such as a no-break space before `(`) in the expression is refused because it cannot be verified, a name with any non-IMMUTABLE overload (such as `date_trunc`) is refused, and a failed catalog read refuses the index.
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

LLM features are on by default (`llm.enabled: true`), but need an endpoint and
configured authentication. API-key providers use `llm.api_key`; native Databricks
authentication uses `llm.auth: databricks` and SDK OAuth credentials. Until
configured, or with `llm.enabled: false`, analysis uses the local rules engine.

### LLM configured

When enabled and configured, pg_sage makes requests to the configured
`llm.endpoint`. Databricks Apps rejects AI API keys and alternate auth modes;
Gateway calls use the App principal's OAuth M2M identity and the configured HTTPS
workspace origin. These requests carry diagnostic metadata rather than table-row
results. See [Lakebase telemetry consent](lakebase.md#advanced-telemetry-auto_explain-and-sre-history)
for the separate optional provider payload path and its privacy boundaries.

What is sent to the LLM:

| Data | Example |
|---|---|
| Schema DDL | `CREATE TABLE public.orders (id bigint NOT NULL, ...)` |
| EXPLAIN plans | `Seq Scan on orders (cost=0.00..1234.00 rows=50000 ...)` |
| Parameterized query text | `SELECT * FROM orders WHERE customer_id = $1 AND status = $2` |
| Aggregate metrics | `mean_exec_time=450ms, calls=12000, n_dead_tup=50000` |
| Finding summaries | `Unused index: idx_orders_legacy (0 scans in 30d)` |

What is **never** sent: row data, column values, passwords, connection strings, API keys, PII. Query text from `pg_stat_statements` contains parameterized placeholders (`$1`, `$2`), not literal values. Live session text pg_sage stores (the `locks` snapshot, served by `/api/v1/snapshots/latest?metric=locks`) has its string and dollar-quoted literals redacted before it is written and is cut to 256 bytes.

### Model output is data

The configuration advisor's prompts carry query text and table names that any
database user can influence, so pg_sage treats every model response as data,
never as instructions to itself:

- A finding's `detail` keeps only a fixed set of model keys at the top level
  (`rationale`, `current_settings`, `recommended_settings`, `requires_restart`,
  `peak_active`, `bloat_pct`, `remediation_options`, `original_query`,
  `suggested_rewrite`, `object_identifier`, `severity`, and `recommended_sql`
  for query rewrites). Any other model key is kept under `detail.model`.
- Reserved keys are dropped wherever they appear in the model's JSON, and each
  drop is logged as a warning: `managed_change`, `what_if_verdict`, `drop_ddl`,
  `index_replace`, `approval_required` and any key starting with `sage_`
  (matched without regard to case). Only pg_sage's own gates write these keys.
- The WAL, memory, vacuum and connection advisors may only propose
  `ALTER SYSTEM SET/RESET` of a setting or `ALTER TABLE ... SET/RESET` of a
  storage parameter. Any other SQL they return becomes advisory, with no SQL
  and no action risk.
- The managed-change worker (RDS, Aurora and Cloud SQL parameter changes)
  proposes only intents found on `memory_tuning`, `vacuum_tuning`,
  `wal_tuning` or `connection_tuning` findings, and only for settings on the
  executor's allowlist. Anything else is refused and logged as
  `managed change not proposed: ... refused: ...`.

Database-derived text in a prompt (query text, log lines, object names,
observed DDL, table schemas) is wrapped in a labelled `<data>` block, and the
system prompt tells the model to treat it as data, never as instructions. A
test walks every model call site in the sidecar and fails when one sends a
system prompt without that rule.

#### Model-authored SQL

SQL written by the model is never shown as pg_sage's recommendation until
pg_sage has checked it. None of it runs through the executor.

- **RCA Tier 2** asks for one read-only diagnostic statement. It is kept only
  when pg_sage proves it read-only against the monitored database's catalog:
  a single `SELECT` that calls no volatile or `SECURITY DEFINER` function,
  takes no row locks and writes nothing. The check reads only the catalog,
  inside a `READ ONLY` transaction with a 2s `statement_timeout` and a 500ms
  `lock_timeout`. Otherwise the incident has no recommended SQL and its causal
  chain says "model SQL withheld" and why. The incident itself is still raised.
- **Migration advisor** scripts and the LLM DDL fallback's `safe_alternative`
  are kept only when every statement is `ALTER TABLE` (column, default,
  constraint and statistics changes), a column or constraint rename, `CREATE
  [UNIQUE] INDEX [CONCURRENTLY]`, `UPDATE`, `ANALYZE`, `SET`/`RESET` of
  `lock_timeout` or `statement_timeout`, `BEGIN` or `COMMIT`, and every table
  it names is the table under review. Administrative functions (`pg_*`,
  `set_config`, `dblink*`, `lo_*`) are refused. Otherwise the recommendation
  falls back to pg_sage's deterministic safe alternative (none for the DDL
  fallback), and the model's text appears in the causal chain as
  `model_text_unvalidated`, labelled "Model text, not validated by pg_sage".
- Both checks use the PostgreSQL parser that release binaries and images are
  built with. A build without cgo has no parser, so it withholds all model SQL.

---

## API Security

### Session Authentication

In standalone mode, the web UI and `/api/v1/*` endpoints use session-cookie authentication. On
first startup against a metadata database with no users, pg_sage creates
`admin@pg-sage.local` and prints a one-time initial password to stderr.

API clients log in and reuse the `sage_session` cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
```

The `sage_session` cookie is 32 random bytes (base64url). pg_sage stores only
its SHA-256 in `sage.sessions.token_hash`, so reading the `sage` schema (a role
with `pg_read_all_data`, a logical dump, a restored backup) or a server log
that records bind parameters does not yield a usable session. The row `id` is
not a secret and does not authenticate. Sessions last 24 hours (break-glass
sessions one hour); a malformed cookie is refused without a query.

- `POST /api/v1/auth/logout` ends the current session and records `logout`.
- `DELETE /api/v1/auth/sessions` ("sign out everywhere else") ends every other
  session of the signed-in user, keeps the one that sent it, answers
  `{"revoked": N}` and records `sessions_revoked` with the count.
- Unlinking a user's SSO identity, and the Databricks admin bootstrap, end all
  of that user's sessions.
- If the session store cannot be read (no database pool, a refused or dropped
  connection, a failover, a saturated pool) the API answers `503` with
  `Retry-After: 5` and
  `{"error":"session store unavailable","code":"auth_store_unavailable"}`,
  not `401`, so a database blip does not look like an expired session. The
  dashboard keeps the user signed in, shows "Session store unavailable,
  retrying" and clears it on the next successful request; a reload during
  the outage shows a retry screen instead of the login page. API clients
  should retry on this code and re-authenticate only on `401`. The cause is
  logged at ERROR at most once every 30 seconds, with a count of the
  suppressed repeats. The SSO link flow (`authorize?intent=link`) answers the
  same way. In standalone mode the session store is the monitored database;
  in fleet and meta modes it is the control database.
- `401` still means the session is over: no cookie, an unknown or expired
  session, a deactivated user, or a cookie that cannot be a session (that one
  is `401` even during an outage, because it is refused without a query).

Upgrading to the release that introduced hashed sessions deletes every
existing session once: every dashboard user signs in again after the upgrade.
The upgrade makes `token_hash` NOT NULL, so an older sidecar sharing the
database cannot create sessions (its logins, break-glass included, fail with
a not-null error instead of storing a plaintext session). Before downgrading
below that release, run
`ALTER TABLE sage.sessions ALTER COLUMN token_hash DROP NOT NULL`; upgrading
again deletes the unhashed sessions and restores the constraint.

`SAGE_API_KEY` is a legacy config field and does not secure the current v1
web/API path.

### Route Authorization

Signing in proves who the caller is; each route then decides which roles may
use it. Every API route declares one gate when it is registered, and the
router refuses a route that declares none:

| Gate | Who may call it |
|---|---|
| Public | Anyone: login, OAuth sign-in, `auth/mode`, `/health`, `/ready`, the dashboard |
| Self-authenticating | The handler checks its own credential: MCP bearer tokens, signed change events and SLI pushes, signed ChatOps callbacks, the specialist contract, break-glass, OIDC link grants |
| Viewer | Any signed-in role (admin, operator, viewer); reads only |
| Viewer write | Any signed-in role, on a short documented list of writes: logout, "sign out everywhere else", Ask Sage |
| Operator | Operators and admins |
| Admin | Admins only |

A signed-in user whose role is none of admin, operator or viewer is refused
everywhere except public and self-authenticating routes. The gate sits on the
route itself, not only in front of the router, so a route answers `401`
without a session and `403` for a role it does not allow even if the session
middleware is bypassed.

Only a canonical path can skip session authentication. A path with `.` or
`..` segments, doubled slashes or their percent-encoded spellings
(`/api/v1/specialist/../users`, `/api/v1/specialist/%2e%2e/users`,
`/api/v1/mcp%2F..%2Fusers`) needs a session even when it starts like a public
or self-authenticating route; the router redirects `..` paths to their clean
form first, and that form is checked again.

### User Offboarding

Deactivate a user rather than deleting them: **Deactivate** on the Users page,
or

```bash
curl -b cookies.txt -H 'Content-Type: application/json'   -X PUT http://localhost:8080/api/v1/users/42/status --data '{"disabled":true}'
```

Deactivation (admin only) takes effect at once and in one transaction:

- every session of the user ends, so an open dashboard returns to the login
  screen on its next request;
- the user's operator MCP tokens are revoked (`revoked_by` is
  `pg_sage: owner deactivated`);
- password login answers 401 (the same as a wrong password), SSO and
  Databricks Apps sign-in answer 403, an unused SSO link grant is refused, and
  a chat identity mapped to the user can no longer approve.

`{"disabled":false}` (**Reactivate**) restores sign-in only: ended sessions and
revoked tokens stay dead, so the user signs in again and an admin issues new
tokens. The last enabled admin cannot be deactivated (`403`), and an admin
cannot deactivate themselves. A deactivated admin does not count toward the
last-admin rule, so the last *enabled* admin also cannot be demoted or
deleted. Signing in with break-glass re-enables the break-glass account.
Both actions are audited as `user_disabled` and `user_enabled`.

Deleting (`DELETE /api/v1/users/{id}`) stays available. A user who changed
configuration can be deleted: `sage.config_audit.changed_by` keeps their id
without a foreign key (it is sealed in the audit chain and never rewritten)
and every config change also records the caller's email in
`changed_by_actor`, so the trail still names them. `sage.config` forgets its
updater (`ON DELETE SET NULL`). If another table still references the user,
delete answers `409` naming that table; deactivate the user instead.

### Databricks Apps Authentication

Apps mode requires `SAGE_AUTH_MODE=databricks_app`, the injected platform runtime
and an explicit administrator email. The platform ingress authenticates the
user; pg_sage authorizes the forwarded identity using its own stored roles.
There is no local password/session, break-glass login or local-token fallback.
Optional `SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS` restricts protected access to
exact email domains, checked before account lookup or provisioning on every
request, including existing administrators. Domain matches are case-insensitive;
subdomains and suffixes do not match implicitly. Invalid domain policies or an
administrator outside the policy stop startup. Denied identities receive
`403 databricks_app_audience_denied`. An omitted/empty environment setting retains
the platform-controlled audience. This gate neither proves current employment
nor replaces platform App access controls or pg_sage roles; deploy and test it
before broadening sharing to a group that may contain external identities.
Do not expose the native listener behind a proxy that permits user-supplied
identity headers. Background Lakebase/Gateway access uses the App principal,
not on-behalf-of user permissions. Agent transports and signed machine ingestion
without an equivalent platform identity/grant contract remain explicitly
unsupported. See the [App auth contract](databricks-apps.md#what-runs-where).

### Email Identity

An email address is one identity whatever its case. pg_sage stores emails
trimmed and in lower case, and every comparison is case-insensitive:
password login, the failed-login audit, the login throttle, SSO first sign-in,
link grants and the break-glass account. `POST /api/v1/users` answers `400`
for anything but a single bare address (`name@example.com`), and `409` for an
email that already exists in any case. `sage.users` enforces this with the
unique index `users_email_lower` on `lower(email)`.

Upgrading rewrites existing emails in lower case and creates that index, unless
two accounts have emails that differ only in case (or surrounding spaces).
Then the upgrade changes nothing, startup still completes, and pg_sage logs an
ERROR and opens a critical finding (`auth_email_case_duplicates` on
`sage.users`) naming the colliding user ids. While the duplicates exist, those
emails fail closed: password login is refused, the failed-login audit names no
account, and SSO answers `link_required`. Keep one account per email (delete or
rename the others, then re-grant roles and links to the one you keep) and
restart pg_sage. The next startup finishes the migration and resolves the
finding. In fleet mode, each database's `sage` schema is migrated on its own.

### SSO Account Linking

SSO users are matched on the identity provider's issuer and subject, never on
email. A first SSO sign-in whose verified email belongs to an existing account,
compared case-insensitively, is refused (HTTP `403`) until the account is linked
by one of these explicit acts:

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
  otherwise SSO stays unavailable and the log says why (see
  [SSO temporarily unavailable](#sso-temporarily-unavailable)).

#### SSO temporarily unavailable

pg_sage tries OIDC discovery once at startup and keeps trying in the
background, so an identity provider or DNS blip during a rollout never turns
SSO off until a restart. While discovery has not succeeded:

- `GET /api/v1/auth/oauth/config` answers
  `{"enabled": true, "ready": false, "reason": "..."}`, and the login page
  shows the SSO button disabled with **SSO temporarily unavailable**. The page
  asks again every 30 seconds and enables the button once SSO is ready.
- `GET /api/v1/auth/oauth/authorize`, the OAuth callback and
  `POST /api/v1/auth/oauth/link-grant` answer `503` with
  `{"code": "sso_not_ready"}` and `Retry-After`, not `404`. A link grant is
  not used up by a refused request. Password login and break-glass are
  unaffected.
- `pg_sage_sso_discovery_ready` is `0` and
  `pg_sage_sso_discovery_failures_total` counts the failed attempts.

The `reason` tells the two cases apart:

| `reason` | Cause | Retry |
|---|---|---|
| `identity provider unreachable` | Transport error, timeout, or a non-200 or unreadable answer from `/.well-known/openid-configuration` | Every 2 s, doubling to at most every 5 min (with jitter); a `WARN` log per attempt |
| `misconfigured` | `oauth.issuer_url` names an issuer the document does not, the document lacks the authorization, token or `jwks_uri` endpoints, or the provider or `oauth.issuer_url` setting is invalid (not an absolute http(s) URL) | Once a day; logged once at `ERROR`. Fix the configuration or the IdP, then restart |

Once discovery has succeeded, pg_sage re-reads the document every 24 hours.
A failed refresh keeps the last good endpoints and signing keys, so sign-in
keeps working through a later outage; the refresh is retried on the short
backoff. The `oauth.enabled: false` answers are unchanged: `enabled: false`
and `404 OAuth not configured`. GitHub uses static endpoints and is always
ready.

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
| `user_disabled`, `user_enabled` | Deactivation and reactivation (`detail.sessions_ended`, `detail.mcp_tokens_revoked`) |
| `user_role_changed` | Role changes from the API (`source: api`) or the IdP (`source: idp`) |
| `oidc_linked`, `oidc_unlinked`, `oidc_link_grant_*` | Account linking |
| `logout` | A user signed out |
| `sessions_revoked` | "Sign out everywhere else" (`detail.count`, `detail.scope`) |

Rows never contain passwords, tokens or email addresses; a failed password
login names the targeted account by id when it exists. (`sage.config_audit`
is different: from this release each config change records the caller's
email in `changed_by_actor`, so the change history survives deleting the
account.)

### Login Throttling

Password login (`POST /api/v1/auth/login`) has two limits, both over a
sliding 15-minute window and both answered with `429`:

- **Per email**: 5 attempts. A successful login clears the count.
- **Per client**: 20 failed logins across all emails, so one client cannot
  spray many accounts. Only failures count, and a successful login does not
  clear the earlier ones.

A client is its address; an IPv6 address counts as its whole /64, since one
customer usually holds that block. The same keying applies to break-glass
attempts and the general API rate limit. An unknown email, an SSO-only
account and a wrong password are refused alike (`401`) and in the same time,
so responses do not reveal which accounts exist. A database error during
login is also a `401` to the client and is logged with its cause.

The per-email limiter tracks at most 10,000 emails. When it is full it drops
the least-used entry, never a locked one; if every entry is locked, logins for
new emails are refused until entries expire, and
`pg_sage_login_limiter_saturated_total` counts each refusal. A rising value
means credential spraying.

**Behind an ingress or load balancer**, list its addresses in
`api.trusted_proxies`. Otherwise `X-Forwarded-For` is ignored, every user
shares the proxy's address, and 20 failed logins by anyone lock password
login for everyone for up to 15 minutes. The first request whose
`X-Forwarded-For` is ignored logs a warning naming `api.trusted_proxies`.
SSO and break-glass are not subject to the per-client failure limit.

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
  client address, an IPv6 address counting as its /64 (5 per 15 minutes).
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
  notification channel secrets are still sealed with `encryption_key` alone.
- **Changed `encryption_key` and database passwords or channel secrets.**
  After `encryption_key` changes, a stored database password that no longer
  opens fails that database with `database <id>: encryption key does not open
  this value`, and startup logs one ERROR per database id ("the stored
  password does not open with encryption_key; was encryption_key changed?")
  with `database_id`; reconnects do not repeat it. A sealed channel secret
  fails with `channel secret <key> does not open with encryption_key`. The
  failure is fast: no key is derived per read. To recover, either restore the
  old `encryption_key` and restart, or keep the new one and re-enter each
  named password and channel secret in the dashboard or through the API.
  Passwords written by v0.8.x key derivations still open and are re-sealed
  under the current key on first read.

### CSRF and Cross-Origin Requests

The session cookie is `HttpOnly` and `SameSite=Lax`, so another website cannot
send it on a write. `SameSite` does not separate *sites* from *origins*: a page
on a sibling subdomain or on another port of the same host is same-site and
would get the cookie. Every `/api/v1/*` route therefore also runs Go's
cross-origin protection (`http.CrossOriginProtection`), ahead of
authentication:

- `GET`, `HEAD` and `OPTIONS` always pass; they change nothing.
- A browser write passes when it sends `Sec-Fetch-Site: same-origin` (or
  `none`, a user-typed request). `same-site` and `cross-site` writes get 403,
  including the multipart CSV import (`POST /api/v1/databases/managed/import`),
  which an HTML form could otherwise submit.
- A browser that sends no `Sec-Fetch-Site` (older than 2023) is checked by its
  `Origin` header against the request's `Host`.
- A request with neither header is not from a browser (curl, SDKs, MCP
  clients, ChatOps callbacks) and passes to authentication unchanged.
- `api.trusted_origins` (`SAGE_API_TRUSTED_ORIGINS`, comma-separated) lists
  exact origins, `scheme://host[:port]`, whose cross-origin writes are
  accepted: a separately hosted UI, or a reverse proxy that rewrites `Host`
  for browsers that send no `Sec-Fetch-Site`. It is empty by default, ignored
  in Databricks App mode, and a malformed entry stops startup with the entry
  named. Every listed origin can submit writes with a signed-in user's
  cookie, so list only origins you control.

The API sends no CORS headers: the dashboard is served from the API's own
origin, and the Vite dev server proxies `/api` to the sidecar.

### TLS

Set `SAGE_TLS_CERT` and `SAGE_TLS_KEY` to PEM files and the API and dashboard
listener serves HTTPS only (TLS 1.2 or newer); session cookies are then marked
`Secure`. Without them it serves plain HTTP, for use behind a TLS-terminating
reverse proxy, Kubernetes Ingress, Cloud Run or load balancer. Behind such a
proxy the password-login session cookie is `Secure` only when the proxy's
address is in `api.trusted_proxies` (default: loopback) and it sends
`X-Forwarded-Proto: https`; the header from any other peer is ignored, and
the first such request logs a warning naming `api.trusted_proxies`. SSO and
break-glass session cookies are always `Secure`.

- Setting only one of the two is a startup error, so a typo can't silently
  downgrade to HTTP.
- At startup a missing file, a key that doesn't match the certificate, or an
  expired or not-yet-valid certificate stops the sidecar with a message naming
  the variable and file.
- Expiry is watched while the sidecar runs, not only at load: the serving
  certificate is re-checked every hour, and a certificate with under 14
  days left logs a warning, one with under 3 days an error, and an expired
  one an error again, each once per certificate. `/ready` then reports
  `"details":{"tls_expiring":true}` (under 14 days) without failing
  readiness. `/metrics` exposes `pg_sage_api_tls_cert_not_after_seconds`
  (the serving certificate's expiry, Unix time) and
  `pg_sage_api_tls_reload_failures_total` (reloads that failed and kept the
  old certificate); both are absent when the API serves plain HTTP.
- Rotation needs no restart: on new connections pg_sage checks the two files'
  modification times at most every 5 seconds and loads a changed pair. The
  check follows symlinks, so cert-manager and Kubernetes secret volume swaps
  work. A pair that fails to load is logged and counted, and the current
  certificate keeps serving until the files change again.
- The Prometheus listener (`:9187`) stays plain HTTP; keep it on a private
  network.

Restrict direct access to the API/dashboard listener to trusted networks
either way.

### MCP tools and scopes

MCP exposes typed tools, never pg_sage's own SQL connection. The full model is in
[MCP for coding agents](mcp.md); in short:

- **Credentials.** Over HTTP (`POST /api/v1/mcp`) MCP accepts only MCP bearer tokens,
  never a session cookie, and a token authenticates nothing but MCP. Only each
  token's SHA-256 is stored. Over stdio, whatever launched the process is an agent.
- **Scopes.** Every tool declares one scope: `read`, `propose` or `approve`.
  `tools/list` shows only the tools the caller may call. Agent tokens can hold
  `read` and `propose`, never `approve`; an operator token follows its owner's
  role. A missing scope is refused with `scope_required`, and an agent asking for
  a person's decision with `approval_reserved_for_humans`.
- **Databases.** A token names the databases it may use; any other name is refused
  with `database_not_permitted`, whether that database exists or not.
- **Arguments.** Every call is checked against the tool's JSON schema
  (`invalid_arguments` otherwise). Proposals go through the same policy gate and
  trust levels as pg_sage's own actions.
- **Agent SQL.** `agent_query` runs one read-only `SELECT` as the agent's own
  database role, never pg_sage's, with timeouts and row and byte limits;
  `explain_query` gives agents plan-only EXPLAIN under that role.
- **Audit.** Every call is recorded with the actor `mcp:token:<id>`.

### Request Limits

- **Body size**: Maximum 1 MB per request.
- **Rate limiting**: Configurable via `SAGE_RATE_LIMIT` (default: 60 requests per minute per client address; an IPv6 client counts as its /64). Password login has its own limits ([Login Throttling](#login-throttling)).
- **Request timeout**: 30 seconds per API request.
- **Database unavailable**: When a handler cannot get a database connection
  (the pool has no free connection before the request deadline, or the
  connection attempt fails), the API answers `503` with
  `{"error":"database unavailable","code":"db_unavailable"}` and
  `Retry-After: 5`. Other server errors stay `500` `{"error":"internal error"}`;
  neither body carries the cause, which is logged server-side.

### Security Headers

Every `/api/v1/*` response (and `/mcp` in Databricks App mode) carries
`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: strict-origin-when-cross-origin`,
`Permissions-Policy: camera=(), microphone=(), geolocation=()` and
`Cache-Control: no-store`, including refusals (401, 403, 404, 415) and
responses that carry secrets (MCP and agent tokens, SSO link grants). The
event stream (`/api/v1/events`) alone sends `Cache-Control: no-cache`, as
server-sent events expect. `/ready` sends `no-store`; `/health` and the
dashboard's static files set no cache header. HSTS and CSP are not set:
add them at the TLS-terminating proxy.

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

The emergency stop halts all autonomous and operator-approved activity: no new
action, rollback or approved queue item starts, and statements pg_sage is
already running (a `CREATE INDEX CONCURRENTLY`, a `VACUUM`, an `ALTER` waiting
on its lock, a rollback) are cancelled with PostgreSQL's protocol cancel
request, which needs no extra grant on any provider. Each cancelled action's
`sage.action_log` row ends `failed` with a reason starting `emergency stop`.
pg_sage runs no DDL during a stop, so a cancelled `CREATE INDEX CONCURRENTLY`
can leave an INVALID index: its row names the index and the statement that
removes it (`DROP INDEX CONCURRENTLY IF EXISTS ...`).

Stop from the web UI emergency stop button, or the authenticated REST API:

```bash
curl -b cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/emergency-stop --data '{}'
```

When the dashboard and API are unreachable, stop from SQL. The function
records the change in `sage.config_audit` under the actor you name:

```sql
SELECT sage.emergency_stop(true, 'dba-on-call');
```

Every authorization reads the flag, so no new work starts once it is set. The
SQL stop does not reach the running process directly: the sidecar re-reads
every database's flag every 10 seconds, then shows the database as stopped
and cancels the statements it is running. To cancel sooner, use
`pg_cancel_backend` on the pg_sage sessions in `pg_stat_activity`. The same
re-read releases a database resumed from SQL. It never releases a stop that
the sidecar could not save (it retries the write instead), and it never
releases a database held by a fleet-wide stop (see below).

Resume with the web UI resume button, the authenticated REST API:

```bash
curl -b cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/resume --data '{}'
```

or from SQL:

```sql
SELECT sage.emergency_stop(false, 'dba-on-call');
```

On a schema older than v2.5.1, where `sage.emergency_stop()` does not exist,
upsert the flag directly (this writes no audit row):

```sql
INSERT INTO sage.config (key, value, updated_by)
VALUES ('emergency_stop', 'true', 'dba-on-call')
ON CONFLICT (key, COALESCE(database_id, 0))
DO UPDATE SET value = EXCLUDED.value, updated_at = now(),
              updated_by = EXCLUDED.updated_by;
```

From v2.5.1, bootstrap seeds `emergency_stop = 'false'` (`updated_by =
'bootstrap'`), so a plain
`UPDATE sage.config SET value = 'true' WHERE key = 'emergency_stop'` stops
the database. Like the upsert above, it writes no `sage.config_audit` row:
prefer `sage.emergency_stop()`. On a schema older than v2.5.1 that was never
stopped through the UI or API, the plain `UPDATE` matches no row and changes
nothing.

The stop response lists, per database, the `sage.action_log` ids of the
running actions it cancelled (`cancelled_actions`) and any cancel it could
not confirm (`cancel_failures`). A cancel failure does not undo the stop.

### Fleet-wide stop

**Stop all** (the API call without `?database=`) is a fleet-wide latch, not
only a stop of the databases that exist when you press it:

- Every database added while it holds is stopped too, whether it is added in
  the UI, by CSV import, by the meta-database reconcile or by a config
  reload. The overview reports the latch as `fleet_stopped`,
  `fleet_stopped_by` and `fleet_stopped_at` in the `summary` of
  `GET /api/v1/databases`, and the header badge reads "Fleet stopped by ...".
- Only **Resume all** lifts it. Resuming one database while it holds returns
  `409 fleet-wide stop is active: resume all first`, and a per-database
  `sage.emergency_stop(false, ...)` is overwritten with a stop at the next
  re-read.
- If **Resume all** cannot clear the flag of every database, the databases
  it cleared run, the others stay stopped, and the latch stays on: new
  databases still come up stopped. Run **Resume all** again.
- It survives a restart. In meta-database mode the latch is the meta
  database's `sage.config` row `fleet_emergency_stop`. If that row cannot be
  read at startup, the whole fleet starts stopped (by `system`). A YAML fleet
  has no control database, so each stopped database also carries
  `emergency_stop_scope = 'fleet'`. At startup, a database whose
  `emergency_stop` is `'true'` with that scope re-latches the whole fleet.

A stop of one database is unchanged: it stops that database only, and a
per-database resume lifts it.

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

### API Access Log

With `api.access_log` on (the default), the sidecar writes one structured
`api access` log line (not a table) for every mutating API request (POST,
PUT, PATCH, DELETE) whatever its status, the signed ChatOps callbacks
included, and for every `401` or `403`, the cross-origin check's refusals
included. Each line has the method, the route pattern (for example
`POST /api/v1/emergency-stop`; the path when the request was refused
before routing), the user id and role when signed in, the status and the
latency in milliseconds. Bodies, query strings, cookies and tokens are
never logged; successful reads are not logged. Set `api.access_log: false`
(restart to change) to turn it off; the auth, config and action tables
above keep their own records either way.

Both tables are subject to retention policies (configurable via `retention.actions_days`).

### Tamper-Evident Chains and SIEM Export

Each audit table (`action_log`, `auth_audit`, `config_audit`,
`guard_query_audit`, `guard_pgaudit_events`) is hash-chained: every write
appends a link to `sage.audit_chain_link`, and every inserted row carries
its own link. `GET /api/v1/audit/verify?database=<name>` (admin) checks a
chain; the scheduled verification (`audit.verify_interval_hours`) raises a
critical `audit_chain_broken` finding on `audit_trail:<chain>` when one
fails. A full verification reports, among others, `gap`, `hash_mismatch`,
`row_missing`, `unchained_row` and `orphan_row`: a row whose own link is no
longer in the chain, which is what deleting the newest links, or every
link of a chain, leaves behind.

SIEM export (`audit.siem.sinks`) ships every link, with its hash, as an
OCSF event. That copy is the defence against someone who rewrites a whole
chain, and against a chain that goes back because the database was
restored from a backup or PITR. Each sink keeps a cursor per source and
chain on the control database: the position and hash of the last link it
exported. Before each pass the exporter checks that the link at the
cursor still has that hash.

- **regressed**: the link is missing, or another link now holds its
  position (new writes after a restore reuse the positions the SIEM
  already has). Export of that chain to that sink stops; nothing is
  skipped. `GET /api/v1/audit/siem` shows the sink with
  `"status": "regressed"` and a `regressions` entry naming the source,
  chain and position, and the scheduled verification raises a critical
  `audit_chain_regressed` finding on `audit_trail:<chain>`. Other chains
  and other sinks keep exporting. A cursor saved before pg_sage kept
  hashes, or moved by an older sidecar during a rolling upgrade, is
  checked for existence only, then gets its hash.
- **reanchor**: after comparing the SIEM's copy with the database, an
  admin resumes export with
  `POST /api/v1/audit/siem/{sink}/reanchor` and a body
  `{"source": "<database or control>", "chain": "<chain>", "from_seq": N}`
  (`from_seq` defaults to 1, the whole chain). Export resumes at `N`; the
  re-exported events over positions the SIEM already holds carry
  `metadata.labels: ["fork"]`. The re-anchor runs on the export leader
  only (409 elsewhere), is recorded in `sage.auth_audit` as
  `siem_reanchored`, and is never automatic. It refuses a `from_seq` past
  the cursor, one onto the cursor's own position when another link now
  holds it, and one whose preceding link is missing. It cannot tell, below
  the cursor, which links the SIEM already has: links before `from_seq`
  are not re-sent. `from_seq: 1` (the default) is always safe; a higher
  value must be the first position where the database and the SIEM's copy
  differ (compare their hashes), or the new links before it are never
  exported. The finding resolves at the next
  verification once every cursor is anchored again. While export is off
  or failed to start, verification cannot check the cursors, so an open
  `audit_chain_regressed` finding stays open.

A cursor that goes back because the control database itself was restored
is not a regression: the link it names is still there, so export simply
re-sends what follows (delivery is at least once).

#### Chained writes need READ COMMITTED

A chain link is appended at commit, after a read of the chain's last
link. That read must see links other transactions just committed, which
only READ COMMITTED gives (a new snapshot per statement). Under
REPEATABLE READ or SERIALIZABLE two writers would take the same
position, and one would fail.

- pg_sage pins every session it opens to READ COMMITTED (`SET SESSION
  default_transaction_isolation = 'read committed'` when it connects), and
  writes `sage.action_log` in explicit READ COMMITTED transactions. A
  role or database default of another level therefore does not affect it.
- A transaction writing a chained table at another level is refused at
  commit with SQLSTATE `55000`, a message naming `transaction_isolation`
  and a hint with the fix. Nothing is written. READ UNCOMMITTED is
  accepted: PostgreSQL runs it as READ COMMITTED.
- At startup pg_sage reads the default its sessions would have without the
  pin (the role, the database and the server configuration) and logs one
  `WARN [audit-chain]` per database when it is not READ COMMITTED. Behind
  PgBouncer in transaction mode the pin is per server connection and can be
  missing on some, so set the default for pg_sage's role:

  ```sql
  ALTER ROLE sage_agent IN DATABASE app SET default_transaction_isolation = 'read committed';
  ```

  PgBouncer in session mode and direct connections need nothing.
  Poolers that pin a client to one server connection after any `SET`
  (Amazon RDS Proxy among them) pin pg_sage's connections for this
  reason; that costs multiplexing, not correctness.

#### Attribution trusts the role, not `application_name`

Any session can set `application_name`, so pg_sage never trusts it alone.
The database role a session logged in as decides.

- **pgaudit records** (`audit.pgaudit.correlate`) are kept in
  `sage.guard_pgaudit_events` with `correlated_by`:
  - `application_name`: `pg_sage agent:<principal>[:<action>]` from an
    agent role (`sage_agent_…` or `sage_agentb_…`). The record names that
    principal and action.
  - `role`: any other session of an agent role.
  - `statement`: `application_name = pg_sage` from pg_sage's own login
    role (`session_user` of its connection), linked to the action whose
    SQL it ran; pg_sage's other statements are not kept. The same name
    from any other role is not pg_sage's and is not kept either.
  - `application_name_unverified`: an agent's application name from a
    role that is not an agent role. The record is kept for forensics, with
    the claimed name in `application`, but `principal_id` and `action_id`
    are NULL, so it never joins an action's evidence.
- **SIEM severity of audit-row deletes**: a delete is Informational only
  when the link's `db_user` is pg_sage's role on that database
  (`current_user` of the exporter's connection) and its application name
  starts with `pg_sage`. Any other role's delete is Medium, whatever
  application name it set; a truncate is always High. On managed
  providers pg_sage's role is the configured user, including IAM and
  OAuth users.

---

## Production Checklist

1. **Protect the dashboard/API listener** -- use a private network, reverse proxy, or identity-aware edge.
2. **Use TLS** -- set `SAGE_TLS_CERT`/`SAGE_TLS_KEY` or terminate TLS at the edge; do not expose plain HTTP directly to the internet. Behind a TLS proxy, list it in `api.trusted_proxies` so session cookies are `Secure`.
3. **Start in observation mode** -- deploy with `trust.level: observation` and review findings for at least a week.
4. **Set a maintenance window** -- restrict autonomous actions to low-traffic periods.
5. **Review findings before escalating trust** -- move to `advisory` then `autonomous` only after confirming recommendations are appropriate.
6. **Set a token budget** -- cap LLM spend with `llm.token_budget_daily`.
7. **Use a dedicated database role** -- grant only the required privileges listed above.
8. **Select the deployment's authentication model.** For standalone, capture and rotate the initial admin password, then use named users or OAuth for operators.
   With SSO, set `oauth.role_mapping`, and configure a break-glass admin plus a notification channel.
   Offboard people by deactivating their account (see [User Offboarding](#user-offboarding)).
   In Databricks Apps, use platform access controls, the explicit administrator
   identity and reviewed pg_sage roles; there is no local password or break-glass fallback.
9. **Set `encryption_key`** (or `SAGE_ENCRYPTION_KEY_FILE`) -- API-set secrets are then encrypted at rest.
10. **Monitor pg_sage itself** -- check Prometheus metrics and circuit breaker state.
