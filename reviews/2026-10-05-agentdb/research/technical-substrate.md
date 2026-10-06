# Technical substrate: PostgreSQL primitives for "databases for AI agents"

- **Status:** research input for `AGENTDB-SPEC.md` (external, technical substrate)
- **Date:** 2026-10-05. All web sources were fetched on 2026-10-05; publication dates are given
  where the source states one.
- **Scope:** PostgreSQL 14 to 18, plus PostgreSQL 19 (beta 4) where it changes an answer;
  self-managed, RDS/Aurora, Cloud SQL/AlloyDB, Azure Flexible Server, and the copy-on-write
  vendors; MCP, the MCP Registry and A2A.
- **Method:** primary documentation, release notes, vendor docs and engineering posts (see
  section 15). pg_sage code was read in `C:/Users/jmass/pg_sage-agentdb-spec/sidecar`. No
  database was contacted.
- **Conventions:** **UNVERIFIED** marks a claim not confirmed from a primary source in this
  session. "pg_sage today" notes cite `sidecar/...:line`. Snippets are illustrations to adapt,
  not tested migrations.

---

## 0. Headline findings

1. **The MCP spec has moved past 2025-11-25.** The current revision is **2026-07-28**. It makes
   MCP stateless: there is no `initialize` handshake and no `Mcp-Session-Id`, every request
   carries its version in `_meta`, and `server/discover` is mandatory. Multi round-trip requests
   (MRTR) replace server-initiated elicitation and sampling. Tasks moved into an extension.
   Roots, Sampling, Logging and Dynamic Client Registration are deprecated [S-MCP3, S-MCP4].
   pg_sage already serves both eras (`sidecar/internal/mcp/protocol.go:11-24`). Its HTTP
   authentication, though, is a static bearer token. The 401 it returns carries
   `WWW-Authenticate: Bearer realm="pg_sage MCP"` with no RFC 9728 `resource_metadata`
   (`sidecar/internal/api/mcp_principal.go:20-33`). Authorization is OPTIONAL in MCP. A server
   that supports it over HTTP, however, MUST publish Protected Resource Metadata, MUST check
   the token's audience, and MUST NOT pass tokens through [S-MCP5].
2. **A READ ONLY transaction is not a security boundary.** Of the published bypasses, the
   archived reference MCP server's (`COMMIT; DROP SCHEMA ...`) is the best known [S-DD1].
   Volatile functions, `dblink`, `postgres_fdw` (until PG19), `COPY TO` and untrusted PL
   languages also get past it [S-PG-TX, S-PG19]. pg_sage's EXPLAIN ANALYZE guard already
   layers the right defences: a parser allowlist, then a catalog proof, then
   `BEGIN READ ONLY`, then a timeout (`sidecar/internal/explain/analyze_guard.go:15-20`,
   `sidecar/internal/explain/explain.go:319-335`). Make that guard the standard for every
   agent-facing SQL path.
3. **Per-role settings are defaults, not limits.** Anyone can `SET` a USERSET parameter
   (`statement_timeout`, `lock_timeout`, `work_mem`, `default_transaction_read_only`, all three
   idle and transaction timeouts). The `SET` privilege on parameters only means anything for
   superuser-only parameters [S-PG-PRIV]. Only superuser-only (SUSET) parameters such as
   `temp_file_limit`, `log_statement` and `pgaudit.log` are hard per-role controls. Community
   PostgreSQL cannot cap CPU, I/O or total memory per role.
4. **Non-human identity is real in PG18, but only for self-managed servers.** The `oauth`
   pg_hba method plus a validator module (none ships in core) [S-PG-OAUTH, S-PG-OAUTHV] puts
   an IdP identity into `SYSTEM_USER`. The managed services use their own IAM tokens instead:
   RDS tokens live 15 minutes and are checked only at login [S-RDSIAM], Cloud SQL tokens live
   1 hour [S-CSQLIAM], and Entra tokens live 5 to 60 minutes [S-AZENTRA]. `VALID UNTIL`
   applies to passwords only, and none of these mechanisms ends a session that is already
   open.
5. **Do not use GUC-based tenancy in RLS for agents that run raw SQL.** An agent that can run
   `SET app.tenant_id = ...` can impersonate another tenant. Key RLS on role identity
   (`current_user`) instead, or give each agent its own database. Also check views without
   `security_invoker` (PG15+), BYPASSRLS, table owners, the FK/unique covert channel, and two
   CVEs (CVE-2024-10976, CVE-2025-8713) [S-PG-RLS, S-CVE1, S-CVE2].
6. **DDL guards have holes.** Only superusers can create event triggers (RDS master user,
   `cloudsqlsuperuser`). Event triggers do not fire for TRUNCATE, roles, databases,
   tablespaces or ALTER SYSTEM [S-PG-ETDEF, S-PG-ETM16]. The right to DROP comes with
   ownership and cannot be granted or revoked [S-PG-PRIV]. So agents must never own objects,
   and `TRUNCATE` must be revoked separately.
7. **Copy-on-write branching is a provider feature.**
   - Neon and Lakebase branch instantly, whatever the database size [S-NEON1, S-LAKE].
   - DBLab clones 1 TiB in about 10 s [S-DBLAB].
   - Aurora allows 15 copy-on-write clones; the 16th is a full copy [S-AUR].
   - Cloud SQL "fast clone" is a metadata-only, same-zone operation [S-CSQLCLONE].
   - Xata branches in seconds [S-XATA].
   - Supabase and PlanetScale Postgres branches carry no data by default [S-SUPABR, S-PSBR].
     Azure offers only PITR to a new server [S-AZBKP].
   - Self-managed PG18 gains `file_copy_method = clone` for
     `CREATE DATABASE ... STRATEGY FILE_COPY` [S-PG18RN, S-PG-RES18].

   pg_sage has a `clone.Provider` seam with a DBLab adapter. The snapshot adapter interface
   has no production implementation (section 6.4).
8. **"Undo" is only partial.** PITR restores the whole cluster [S-PG-PITR]. Logical decoding
   sees DML only: it misses DDL, and it needs `REPLICA IDENTITY FULL` to capture old values
   [S-WAL2JSON]. For agents, the most dependable undo is to branch before acting, plus the
   provider's instant restore (Neon restores in seconds and keeps a backup branch
   [S-NEONRESTORE]).
9. **Version facts that matter now:**
   - PG14 reaches end of life on **2026-11-12**, 38 days after this research [S-PGVER].
   - PG19 GA is planned for October 2026. Beta 4 reverted SQL/PGQ, online checksums,
     `FOR PORTION OF`, `MERGE/SPLIT PARTITIONS` and the `pg_get_*_ddl()` functions [S-PG19B4].
   - PG19 also blocks `postgres_fdw` writes from READ ONLY transactions [S-PG19].
   - pg_sage's parser (`pg_query_go` v6.2.2, `sidecar/go.mod:13`) uses the **PG17 grammar**.
     The latest release is 6.2.5 (2026-09-30), still PG17, so PG18-only syntax fails closed
     [S-PGQGO].
10. **Agent memory has DBA-actionable defects.**
    - pgvector 0.8.3 (2026-06-17) fixed possible HNSW index corruption during vacuum, and 0.8.4
      fixed more vacuum bugs [S-PGV-CL]. RDS and Azure list 0.8.2, Cloud SQL lists 0.8.5
      [S-RDSEXT, S-AZEXT, S-CSQLEXT].
    - `NOTIFY` serializes commits behind a global lock [S-RECALL].
    - pg_textsearch corpus statistics span all rows, so they ignore RLS [S-PGTS].

---

## 1. Version baseline (what exists where)

Minor versions as of 2026-10-05: 18.6, 17.11, 16.15, 15.19, 14.24 [S-PGVER].

| Version | GA / EOL | Agent-relevant additions (source) |
|---|---|---|
| **14** | 2021-09-30 / **2026-11-12** | `pg_read_all_data`, `pg_write_all_data`, `pg_database_owner`; `idle_session_timeout`; `compute_query_id` with `query_id` in `pg_stat_activity`; `pg_stat_statements.toplevel`; `password_encryption` defaults to SCRAM; `DETACH PARTITION CONCURRENTLY`; `client_connection_check_interval` [S-PG14RN] |
| **15** | 2022-10-13 / 2027-11-11 | `PUBLIC` loses `CREATE` on schema `public` in new databases, and `public` is owned by `pg_database_owner`; `security_invoker` views; `GRANT SET / ALTER SYSTEM ON PARAMETER`; `pg_checkpoint`; `CREATE DATABASE ... STRATEGY` (WAL_LOG default, FILE_COPY); `MERGE`; `jsonlog`; logical replication row filters and column lists [S-PG15RN] |
| **16** | 2023-09-14 / 2028-11-09 | `CREATEROLE` needs `ADMIN` on the target; `createrole_self_grant`; `GRANT ... WITH INHERIT/SET`; `pg_create_subscription`, `pg_use_reserved_connections` with `reserved_connections`; `SYSTEM_USER`; libpq `require_auth`; regex and include in pg_hba; logical decoding on standbys; `pg_stat_io` [S-PG16RN] |
| **17** | 2024-09-26 / 2029-11-08 | `MAINTAIN` privilege and `pg_maintain`; `transaction_timeout`; login event triggers; `event_triggers` GUC; `allow_alter_system`; incremental backup (`pg_basebackup --incremental`, `pg_combinebackup`, `summarize_wal`); failover slots (`sync_replication_slots`); `pg_createsubscriber`; `sslnegotiation=direct`; maintenance commands run with a safe `search_path`; `MERGE ... RETURNING` [S-PG17RN] |
| **18** | 2025-09-25 / 2030-11-14 | `oauth` authentication with `oauth_validator_libraries`; MD5 deprecation warnings; SCRAM pass-through for `postgres_fdw`/`dblink`; `pg_signal_autovacuum_worker`; `pg_stat_get_backend_io()`/`_wal()`; byte columns in `pg_stat_io`; finer `log_connections`; `log_lock_failures`; `file_copy_method`; `idle_replication_slot_timeout`; `NOT NULL ... NOT VALID`; `NOT ENFORCED`; virtual generated columns become the default; `WITHOUT OVERLAPS`/`PERIOD`; `RETURNING OLD/NEW`; `uuidv7()`; `pg_dump --statistics`; async I/O; checksums on by default; AFTER triggers run as the role that queued them [S-PG18RN] |
| **19** (beta 4, 2026-09-24; GA planned Oct 2026) | n/a | `REPACK [CONCURRENTLY]`; `pg_stat_lock`; autovacuum scoring and parallel autovacuum; READ ONLY transactions can no longer write through `postgres_fdw`; `pg_read/write_all_data` cover large objects; `GRANT ... GRANTED BY`; `password_expiration_warning_threshold`; RADIUS removed; sequences replicate logically; `effective_wal_level` (logical decoding without a restart); `log_lock_waits` on by default; `max_locks_per_transaction` default 128; JIT off by default; OAuth `oauth_ca_file`, validator-defined HBA options, `PQAUTHDATA_OAUTH_BEARER_TOKEN_V2` [S-PG19] |

Treat PG19 items as provisional until GA. Beta 4 shows that features can still be reverted
[S-PG19B4].

---

## 2. Identity and access

### 2.1 Primitives and the ownership trap

- **Roles and privileges.** Grants exist per object type. `ALTER DEFAULT PRIVILEGES` applies
  only to objects created *later*, and only by the role named in `FOR ROLE` (default: the
  current role). A per-schema `REVOKE` can only undo a per-schema `GRANT` [S-PG-ADP].
- **Ownership is the real super-privilege.** The right to ALTER or DROP an object belongs to
  its owner and cannot be granted or revoked. Members of the owning role inherit it
  [S-PG-PRIV]. Corollary: **an agent must never own, or be a member of a role that owns, the
  objects it is not allowed to destroy.**
- **Dangerous privileges to keep away from agents:**
  - `TRUNCATE` is a separate privilege.
  - `TRIGGER` lets the grantee attach code that runs with the privileges of whoever later
    modifies the table [S-PG-PRIV].
  - `pg_read_server_files`, `pg_write_server_files` and `pg_execute_server_program` bypass
    every database-level permission check and can lead to superuser-level access
    [S-PG-PREDEF].
- **Schema `public`.** Since PG15, new databases no longer let `PUBLIC` create objects in
  `public`. Databases created before PG15 keep the old grant through upgrade and restore, so
  revoke it on PG14 and on upgraded clusters [S-PG15RN].

Least-privilege layout for agent databases (PG16+ grant options; on PG14 and 15 use the role
attributes `INHERIT`/`NOINHERIT` instead):

```sql
-- Owner role: NOLOGIN, owns everything, never handed to an agent.
CREATE ROLE app_owner NOLOGIN;
CREATE SCHEMA app AUTHORIZATION app_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;            -- needed on PG14 / upgraded DBs
REVOKE ALL ON DATABASE appdb FROM PUBLIC;              -- CONNECT and TEMP default to PUBLIC
ALTER DEFAULT PRIVILEGES FOR ROLE app_owner REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- Capability roles (NOLOGIN), granted to agent logins.
CREATE ROLE cap_read  NOLOGIN;
CREATE ROLE cap_write NOLOGIN;
GRANT USAGE ON SCHEMA app TO cap_read, cap_write;
GRANT SELECT ON ALL TABLES IN SCHEMA app TO cap_read;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA app TO cap_write; -- no TRUNCATE/TRIGGER
ALTER DEFAULT PRIVILEGES FOR ROLE app_owner IN SCHEMA app GRANT SELECT ON TABLES TO cap_read;
ALTER DEFAULT PRIVILEGES FOR ROLE app_owner IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO cap_write;
ALTER DEFAULT PRIVILEGES FOR ROLE app_owner IN SCHEMA app GRANT USAGE ON SEQUENCES TO cap_write;

-- One login per agent; inherits capabilities but cannot SET ROLE into them (PG16+).
CREATE ROLE agent_7f3a LOGIN CONNECTION LIMIT 5;
GRANT CONNECT ON DATABASE appdb TO agent_7f3a;
GRANT cap_write TO agent_7f3a WITH INHERIT TRUE, SET FALSE;
```

### 2.2 Predefined roles (agents vs. the AI DBA)

| Role | Since | Use for agents | Use for pg_sage (the AI DBA) |
|---|---|---|---|
| `pg_read_all_data` / `pg_write_all_data` | 14 | Avoid. They bypass table grants (but not RLS); read-only data agents should get explicit grants instead [S-PG-PREDEF] | Avoid. pg_sage does not need row data |
| `pg_database_owner` | 14 | n/a (implicit) | n/a |
| `pg_monitor` (`pg_read_all_settings`, `pg_read_all_stats`, `pg_stat_scan_tables`) | 10 | Avoid. Statistics views and query text can leak other tenants' SQL | **Yes** |
| `pg_checkpoint` | 15 | No | Optional |
| `pg_use_reserved_connections` with `reserved_connections` | 16 | No | **Yes.** pg_sage can still connect when agents use up every slot |
| `pg_create_subscription` | 16 | No | Only for clone and branch workflows |
| `pg_maintain` / `MAINTAIN` | 17 | No | **Yes on PG17+.** VACUUM, ANALYZE, REINDEX, CLUSTER, REFRESH MATVIEW and LOCK without ownership; on PG14 to 16 these need ownership or (managed) admin roles [S-PG17RN, S-PG-PRIV] |
| `pg_signal_backend` | 9.6 | No | **Yes** (kill switch). It cannot signal superuser-owned backends [S-PG-PREDEF] |
| `pg_signal_autovacuum_worker` | 18 | No | Yes, for cancelling autovacuum that blocks DDL [S-PG18RN] |
| `pg_read/write_server_files`, `pg_execute_server_program` | 11 | **Never** | Never |

PG19 extends `pg_read_all_data` and `pg_write_all_data` to large objects [S-PG19].

### 2.3 SET ROLE and SECURITY DEFINER pitfalls

- **A pooled `SET ROLE agent_x` isolates nothing.** Any user can run `RESET ROLE` or
  `SET ROLE NONE` and get back the *session user's* privileges [S-PG-SETROLE]. If an agent can
  submit raw SQL over a connection that logged in as a privileged role, the agent holds that
  role's privileges. **An agent that submits SQL must log in as its own role.**
- `SET ROLE` does not apply the target role's `ALTER ROLE ... SET` defaults. Those load only
  at login [S-PG-SETROLE, S-PG-ALTERROLE]. Per-role guardrails therefore do nothing for
  sessions that switch roles.
- `SET ROLE` is not allowed inside `SECURITY DEFINER` functions. A role can switch only to
  roles it holds the `SET` option on (PG16 grant option) [S-PG-SETROLE, S-PG16RN].
- **SECURITY DEFINER functions** must pin `search_path` to trusted schemas, with `pg_temp`
  last. They must also revoke the default `EXECUTE` from `PUBLIC` in the transaction that
  creates them [S-PG-CF]:

  ```sql
  BEGIN;
  CREATE FUNCTION app.grant_agent_tenant(t uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = app, pg_temp AS $$ ... $$;
  REVOKE ALL ON FUNCTION app.grant_agent_tenant(uuid) FROM PUBLIC;
  GRANT EXECUTE ON FUNCTION app.grant_agent_tenant(uuid) TO broker;
  COMMIT;
  ```

- **CREATEROLE** changed in PG16: a role needs `ADMIN OPTION` on the target to change most of
  its attributes or memberships, and `createrole_self_grant` controls whether the creator
  inherits from the roles it creates [S-PG16RN]. On PG14 and 15, a pg_sage role holding
  `CREATEROLE` is close to an administrator.
- The **`set_user`** extension (pgaudit project) provides audited role switching and can block
  `ALTER SYSTEM`, `COPY PROGRAM` and changes to `log_statement` for escalated sessions
  [S-SETUSER]. It suits break-glass access.

### 2.4 PostgreSQL 18 OAuth: what it enables for agent identity

Mechanics [S-PG-OAUTH, S-PG-OAUTHV, S-PG-LIBPQOAUTH]:

- pg_hba method `oauth` with `issuer` and `scope` (required), plus `validator`, `map` and
  `delegate_ident_mapping`. The server loads validators from `oauth_validator_libraries`.
- **The server does not validate tokens itself.** The validator module checks signature or
  introspection, issuer, audience, expiry and scopes, and returns the authenticated identity.
  **Core ships no validator.**
- The identity appears in `SYSTEM_USER` and in connection logs. Without `map`, the token's
  user name must equal the requested role; with `map`, pg_ident.conf rules apply.
  `delegate_ident_mapping=1` hands every authorization decision to the module, even
  anonymous access, which is a single point of failure.
- libpq's built-in client flow is the **device authorization grant**. It is optional at build
  time (`--with-libcurl`) and not available on Windows. Non-interactive clients (agents,
  services) instead supply a bearer token through `PQsetAuthDataHook`
  (`PQAUTHDATA_OAUTH_BEARER_TOKEN`).
- PG19 adds `oauth_ca_file`, a V2 token hook, and HBA options defined by validators [S-PG19].
- Validators available: Percona **pg_oidc_validator 1.1.0**, tested with Okta, Ping Identity,
  Keycloak and Microsoft Entra ID, and needing a C++23 toolchain [S-PERCONA-OIDC]; the
  CloudNativePG Keycloak validator [S-CNPG-KC].

```conf
# postgresql.conf (PG18+)
oauth_validator_libraries = 'pg_oidc_validator'
# pg_hba.conf
hostssl appdb all 0.0.0.0/0 oauth issuer="https://idp.example.com/realms/agents" scope="openid pg" map=agents
# pg_ident.conf  (IdP subject -> DB role)
agents  /^agent-([a-z0-9]+)@agents\.example\.com$  agent_\1
```

What this buys an agent platform:

- Per-agent database identity issued by the enterprise IdP: workload identities, short-lived
  tokens, central revocation through introspection.
- An attributable `SYSTEM_USER` in the database without any password store.
- Room for scope-based authorization inside the validator.

Caveats:

- The validator is part of the trusted computing base.
- Go drivers need their own OAUTHBEARER SASL support. **UNVERIFIED** whether pgx v5.9
  supports it; pg_sage uses pgx v5.9.2 (`sidecar/go.mod:12`).
- **UNVERIFIED:** RDS, Cloud SQL, AlloyDB and Azure document no way to enable the `oauth`
  pg_hba method. Each uses its own IAM token scheme (2.5).

### 2.5 Managed IAM database authentication

| Provider | How | Token lifetime | Notable constraints |
|---|---|---|---|
| RDS / Aurora PostgreSQL | `GRANT rds_iam TO user`; token from `generate-db-auth-token` (SigV4) used as the password | **15 min**; used only at authentication and does not affect the session afterwards | A role with `rds_iam` must log in through IAM, even the master user; cannot be combined with Kerberos; needs **300 to 1000 MiB** extra memory; CloudTrail and CloudWatch do not log it; tokens about 1 KB or more; not on Outposts [S-RDSIAM] |
| Cloud SQL for PostgreSQL | flag `cloudsql.iam_authentication`; automatic IAM authentication through the Auth Proxy and the Go/Java/Python connectors, or a manual token | **1 h** OAuth2 access token | Service-account users drop `.gserviceaccount.com` (`sa@proj.iam`); new IAM users get **no privileges** until granted; IAM groups (max 200 per instance) create member users at first login; 12,000 login attempts per minute per instance; logins lowercase; SSL required [S-CSQLIAM, S-CSQLIAMUSERS] |
| AlloyDB | flag `alloydb.iam_authentication=on`; principal needs `alloydb.databaseUser` and `alloydb.client` | **UNVERIFIED** (presumably the Google access-token lifetime) | Database user named after the IAM email [S-ALLOYIAM] |
| Azure Flexible Server | Entra admin creates roles with `pgaadauth_create_principal('name', isAdmin, isMfa)`; token for resource `https://ossrdbms-aad.database.windows.net` | **5 to 60 min** | Group sync (`pgaadauth.enable_group_sync`) every 30 min; managed identities and service principals can be group members; outbound access to Entra required for private networking [S-AZENTRA] |

**pg_sage implication.** "Short-lived credentials" on managed services means "short-lived
login tokens". Sessions outlive them. A kill switch must also terminate the sessions:

```sql
ALTER ROLE agent_7f3a NOLOGIN;                        -- or CONNECTION LIMIT 0
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'agent_7f3a';
```

On RDS, also remove `rds_iam` or detach the IAM policy, since the token is checked only at
authentication [S-RDSIAM].

### 2.6 Other short-lived credentials and certificates

- **Vault database secrets engine:** creates dynamic roles (`CREATE ROLE "{{name}}" ...
  VALID UNTIL '{{expiration}}'`) with default and maximum TTLs, and revokes them. Static-role
  rotation is also available [S-VAULT].
- **`VALID UNTIL` only expires the password.** It is ignored for non-password authentication
  and does not end open sessions [S-PG-CREATEROLE].
- **Certificate authentication:** `hostssl ... cert map=...` matches the certificate CN (or DN)
  to a role, optionally through pg_ident. `cert` behaves like `trust` with
  `clientcert=verify-full` [S-PG-CERT]. It suits self-managed agent fleets that already run a
  workload PKI (SPIFFE-style).
- **libpq `require_auth` (PG16)** lets a client refuse weaker authentication methods
  [S-PG16RN].

### 2.7 pg_sage today (identity)

- `sage.agent_identities` holds agent identities at the application layer
  (`sidecar/internal/agentdb/identity.go:24-60`). No file in `sidecar/internal/agentdb`
  creates, alters or grants PostgreSQL roles (a grep for `CREATE ROLE`, `ALTER ROLE` and
  `GRANT` finds nothing). Per-project passwords are derived for Supabase only
  (`sidecar/internal/agentdb/credentials.go:32-33`).
- MCP principals separate human from agent and grant the scopes `read`, `propose` and
  `approve`. Agents can never approve (`sidecar/internal/mcp/principal.go:10-45`).
- pg_sage already recommends per-role `work_mem` through `ALTER ROLE ... SET`, advisory only
  (`sidecar/internal/analyzer/rules_workmem_promotion.go:14-30`).

---

## 3. Row-level security (RLS)

### 3.1 Semantics that matter for agents

All of the following come from [S-PG-RLS] unless marked:

- Enabling RLS with no policy denies everything.
- Superusers and `BYPASSRLS` roles always bypass RLS. Table owners bypass it unless
  `FORCE ROW LEVEL SECURITY` is set.
- Policies are per command. `USING` filters which rows are visible; `WITH CHECK` constrains
  new rows, and defaults to `USING` when omitted.
- Permissive policies are ORed together; restrictive policies are ANDed.
- Unique, primary-key and foreign-key checks ignore RLS. That opens a *covert channel*: an
  agent can probe whether a key exists.
- Subqueries in policies can see stale snapshots under concurrency.
- With `row_security = off`, a query that RLS would filter raises an error instead of silently
  returning fewer rows. That makes it the safe setting for dumps [S-PG-CLIENT].
- `pg_read_all_data` and `pg_write_all_data` do **not** bypass RLS [S-PG-PREDEF].
- **Views:** by default, access runs with the *view owner's* privileges and the *owner's* RLS.
  PG15 `security_invoker = true` applies the invoker's privileges and RLS instead
  [S-PG-VIEW, S-PG15RN]. On PG14, any view over an RLS table can bypass RLS for whoever may
  read the view.
- `security_barrier` views evaluate their own quals before user-supplied ones. Only
  `LEAKPROOF` functions and operators are pushed below the barrier [S-PG-VIEW].

### 3.2 Isolation patterns, ranked for agents that run arbitrary SQL

| Pattern | Spoofable by the agent? | Notes |
|---|---|---|
| **Database (or branch) per agent or tenant** | No | Strongest isolation, and works with every pooler. Costs: a connection pool per database, and `pg_stat_statements` entries multiply (5.2). It is the natural fit for copy-on-write branches |
| **Role per agent; policies on `current_user`/`session_user` or a mapping table** | No, provided the agent logs in as itself (2.3) | Works with transaction poolers only when each agent has its own pool keyed by (db, user) |
| **GUC tenant (`current_setting('app.tenant_id')`)** | **Yes.** Any session can `SET app.tenant_id`; it is USERSET | Fine for application code paths that never pass raw SQL from an agent. With transaction pooling use `set_config('app.tenant_id', $1, true)` / `SET LOCAL` |
| **SECURITY DEFINER "context setter" that verifies a signed token, then stores it** | Hard, if the storage is not a plain GUC (for example a temp table owned by the definer) | **UNVERIFIED as a pattern.** Custom code, so review it like crypto |
| **Schema per agent** | Partly; `search_path` is USERSET | Weaker than per-database. `queryid` differs per schema (5.2) |

Policy keyed on login identity:

```sql
ALTER TABLE app.memories ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.memories FORCE ROW LEVEL SECURITY;          -- owner too
CREATE POLICY agent_rows ON app.memories
  FOR ALL TO cap_write
  USING (agent_id = (SELECT a.id FROM app.agents a WHERE a.db_role = current_user))
  WITH CHECK (agent_id = (SELECT a.id FROM app.agents a WHERE a.db_role = current_user));
CREATE INDEX ON app.memories (agent_id);                     -- policy column first
```

### 3.3 Performance

- **Index the policy columns.** Postgres evaluates the policy for each candidate row, so an
  unindexed policy column turns reads into sequential scans [S-SUPARLS].
- **Wrap stable functions in a scalar subquery** (`(SELECT auth.uid())`) so the planner runs
  them once as an initPlan instead of per row [S-SUPARLS].
- **Scope policies with `TO <role>`**, so they are not evaluated for roles they do not apply
  to [S-SUPARLS].
- **Mark only truly leak-free helpers `LEAKPROOF`.** Non-leakproof functions in user quals
  cannot be pushed below a security barrier, which can force bad plans [S-PG-VIEW].
- Supabase publishes these as guidelines without benchmark figures [S-SUPARLS]. No primary
  benchmark is cited here.

### 3.4 Known pitfalls and CVEs

- **CVE-2024-10976** (fixed in 17.1, 16.5, 15.9, 14.14, 13.17). RLS in subqueries, CTEs,
  security-invoker views or SQL functions could ignore a role change between planning and
  execution [S-CVE1].
- **CVE-2025-8713** (fixed in 17.6, 16.10, 15.14, 14.19, 13.22; disclosed 2025-08-14).
  Optimizer statistics could expose sampled rows that RLS hid, or that sat behind a view the
  user could not read [S-CVE2].
- `service_role`-style keys (BYPASSRLS) handed to an MCP client undo RLS entirely. That is the
  root cause of the Supabase MCP exfiltration demonstration [S-GA, S-SUPARLS].
- **BM25 statistics:** pg_textsearch corpus statistics cover every indexed row, so relevance
  scores can leak term frequencies across RLS boundaries [S-PGTS].
- `pg_dump` (PG18) adds `--no-policies` [S-PG18RN]. Dumps taken by a role subject to RLS fail
  under `row_security = off` instead of silently omitting rows [S-PG-CLIENT].

---

## 4. Per-role guardrails

### 4.1 What can be set per role, and who can override it

| Setting | Since | Who may change it in session | Hard per-role limit? | Notes |
|---|---|---|---|---|
| `CONNECTION LIMIT` (role or database) | old | n/a | **Yes**, but approximate under races; **never enforced for superusers**; prepared transactions and background workers do not count [S-PG-CREATEROLE] | Kill switch: `CONNECTION LIMIT 0` |
| `statement_timeout` | old | **Any user** (USERSET) | No | Measured from arrival of the command [S-PG-CLIENT] |
| `lock_timeout` | 9.3 | Any user | No | Applies per lock acquisition; pointless if ≥ `statement_timeout` [S-PG-CLIENT] |
| `idle_in_transaction_session_timeout` | 9.6 | Any user | No | Protects the xmin horizon and limits bloat [S-PG-CLIENT] |
| `idle_session_timeout` | **14** | Any user | No | **Can break poolers.** Apply only to direct agent logins [S-PG-CLIENT] |
| `transaction_timeout` | **17** | Any user | No | If ≤ `statement_timeout` or the idle timeout, the longer one is ignored; prepared transactions are exempt [S-PG-CLIENT, S-PG17RN] |
| `default_transaction_read_only` | old | Any user | **No.** `SET default_transaction_read_only = off` or `BEGIN READ WRITE` defeats it | See 12.5 |
| `work_mem`, `hash_mem_multiplier`, `max_parallel_workers_per_gather`, `temp_buffers` | old | Any user | No | One query can use `work_mem` × nodes × (workers + 1) [S-PG-RES] |
| **`temp_file_limit`** | 9.2 | **Superusers, or roles granted `SET` on it** | **Yes** | Per process; cancels the transaction; temporary *tables* do not count [S-PG-RES] |
| `log_statement`, `log_min_duration_statement`, `pgaudit.log` | n/a | Superuser only (SUSET) | **Yes** (audit cannot be turned off by the agent) | RDS shows the master user running `ALTER USER ... SET pgaudit.log` [S-RDSPGA2] |
| `session_replication_role` | old | Superuser or granted | n/a | `replica` disables FK checks and triggers; never grant it [S-PG-CLIENT] |
| `search_path` | old | Any user | No | Pin in SECURITY DEFINER functions |

Key rules:

- `ALTER ROLE ... SET` values are **session defaults applied at login**. A session can override
  them, and `SET ROLE` does not apply them [S-PG-ALTERROLE].
- **The `SET` privilege on a parameter only matters for parameters that normally need a
  superuser** [S-PG-PRIV]. You can grant `SET` on `temp_file_limit` to a broker role, but you
  cannot revoke an agent's ability to `SET statement_timeout`.

Enforcement options for overridable limits:

1. **Parse and refuse.** Reject `SET`, `RESET`, `set_config`, `BEGIN ... READ WRITE`,
   `SET TRANSACTION` and `DO` in the agent's SQL. Do it at the MCP layer (pg_sage) or the
   pooler (PgDog parses queries and tracks `SET` [S-PGDOG]).
2. **Re-apply per transaction.** The executor wraps each agent statement in
   `BEGIN; SET LOCAL statement_timeout ...; <stmt>; COMMIT` over the extended protocol, so the
   agent cannot smuggle in a second statement (12.5).
3. **Watchdog.** Use `pg_stat_activity` with `pg_cancel_backend`/`pg_terminate_backend` under
   `pg_signal_backend`, which cannot touch superuser sessions [S-PG-PREDEF].

Guardrail bundle (illustration):

```sql
ALTER ROLE agent_7f3a CONNECTION LIMIT 5;
ALTER ROLE agent_7f3a SET statement_timeout = '15s';           -- default only
ALTER ROLE agent_7f3a SET lock_timeout = '2s';
ALTER ROLE agent_7f3a SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE agent_7f3a SET transaction_timeout = '60s';         -- PG17+
ALTER ROLE agent_7f3a SET temp_file_limit = '1GB';             -- hard: superuser sets it
ALTER ROLE agent_7f3a SET pgaudit.log = 'write, ddl, role';    -- hard: superuser sets it
ALTER ROLE agent_7f3a IN DATABASE appdb SET work_mem = '16MB'; -- default only
```

On managed services, the master user can set superuser-only parameters per role. RDS
documents this for `pgaudit.log` [S-RDSPGA2]. Cloud SQL's `cloudsqlsuperuser` is not a
superuser [S-CSQLUSERS]. **UNVERIFIED:** whether `cloudsqlsuperuser` or `azure_pg_admin` may
`ALTER ROLE ... SET temp_file_limit`.

### 4.2 What community PostgreSQL cannot limit, and workarounds

| Resource | Native per-role control | Workarounds |
|---|---|---|
| CPU | None | Separate compute per agent (branch, replica, serverless endpoint with a capacity cap); kill long statements; `max_parallel_workers_per_gather = 0` as an overridable default; **EDB Resource Manager** (EPAS-only resource groups) [S-EDBRM] |
| Disk I/O | None | Same; on Linux, cgroups for the whole instance. `pg_cgroups` was archived on 2024-07-12 and only ever worked at cluster level [S-PGCG] |
| Total memory | None (only per-node `work_mem`) | Keep agents off big `work_mem`; low `hash_mem_multiplier`; separate compute |
| WAL / write rate | None | Watch per-backend WAL (PG18, 5.4) and terminate; isolate on a branch |
| Storage quota per role, schema or database | **None** | Database or branch per agent plus `pg_database_size()` monitoring with automatic `CONNECTION LIMIT 0`; provider project limits (Neon per plan) |
| Locks held | `lock_timeout` (overridable) | `log_lock_waits` (on by default in PG19), the PG19 `pg_stat_lock` view, terminate blockers [S-PG19] |

---

## 5. Attribution and audit

### 5.1 Who did this? Identity signals and how far to trust them

| Signal | Set by | Trust |
|---|---|---|
| `application_name` | Client | **Spoofable**; a label, not identity |
| sqlcommenter comments (`/*key='value',traceparent='...'*/`) | Client ORM or framework; W3C trace context [S-SQLCOMM] | Spoofable; good for correlation (Cloud SQL and AlloyDB query insights consume it) |
| `current_user` / `session_user` (`usename` in `pg_stat_activity`) | Login | Trustworthy if each agent logs in as itself |
| `SYSTEM_USER` (PG16) | Authentication method plus authenticated ID, e.g. OAuth or cert identity | **Best**: ties a session to the IdP or certificate subject [S-PG16RN, S-PG-OAUTHV] |
| Connection logs (`log_connections`, finer stages in PG18; `%L` client IP in `log_line_prefix`) | Server | Trustworthy [S-PG18RN] |

### 5.2 pg_stat_statements: cost per agent

- Keyed by `userid`, `dbid`, `toplevel` (PG14) and `queryid`. Counters include `calls`, time,
  rows, shared/local/temp blocks, `wal_records`/`wal_bytes`/`wal_fpi`, JIT, and
  `parallel_workers_to_launch/launched` (PG18). `stats_since` and `minmax_stats_since` arrived
  in PG17, which also renamed `blk_read_time` to `shared_blk_read_time` [S-PG-PGSS, S-PG17RN].
- PG18 assigns query IDs to `CREATE TABLE AS` and `DECLARE`, parameterizes `SET` values and
  adds `wal_buffers_full` [S-PG18RN]. PG19 adds generic and custom plan counts [S-PG19].
- `pg_stat_statements.max` defaults to 5,000 entries. The least-executed entries are evicted
  (counted in `pg_stat_statements_info`), and changing the limit needs a restart [S-PG-PGSS].
- **Agent hazard:** identical SQL text resolved through different `search_path`s gets
  different entries, and queryids are not stable across major versions or platforms
  [S-PG-PGSS]. Schema-per-agent designs flood the hash table. Per-agent cost then needs
  `userid` rollups plus sampling (5.4).

```sql
SELECT r.rolname AS agent, sum(s.total_exec_time) AS ms, sum(s.wal_bytes) AS wal,
       sum(s.temp_blks_written) AS temp_blks, sum(s.calls) AS calls
FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
GROUP BY r.rolname ORDER BY ms DESC;
```

### 5.3 pg_stat_activity

- `query_id` appears in `pg_stat_activity`, `EXPLAIN VERBOSE` and logs (`%Q`) once
  `compute_query_id` is on. Its default `auto` turns it on when pg_stat_statements is loaded
  (PG14) [S-PG14RN].
- Combine `usename`, `application_name`, `backend_start`, `xact_start`, `state` and
  `wait_event` for live attribution and kill decisions.

### 5.4 Per-backend I/O and WAL (PG18)

- `pg_stat_get_backend_io(pid)` and `pg_stat_get_backend_wal(pid)` return per-backend I/O and
  WAL statistics; `pg_stat_reset_backend_stats(pid)` resets them [S-PG18RN].
- `pg_stat_io` now reports bytes (`read_bytes`, `write_bytes`, `extend_bytes`) [S-PG18RN].
  PG19 adds full-page-write bytes to the WAL statistics [S-PG19].
- **UNVERIFIED:** whether these statistics survive after the backend exits. Design for
  periodic sampling, joined to `pg_stat_activity.usename`.

### 5.5 pgaudit

- **Session audit:** `pgaudit.log` takes the classes `READ`, `WRITE`, `FUNCTION`, `ROLE`,
  `DDL`, `MISC`, `MISC_SET` and `ALL`, with `-` to exclude a class.
- **Object audit:** `pgaudit.role` audits only relations that role has privileges on.
- Options: `log_relation`, `log_parameter`, `log_statement_once`.
- Settings are superuser-only. They can be set per database or per role, and do not follow
  role inheritance or `SET ROLE`.
- Logging is best effort, not transactional, and failed statements may be logged. Superusers
  cannot be audited reliably.
- Version scheme: 1.6 for PG14, 1.7 for PG15, then 16.x, 17.x and 18.x [S-PGAUDIT].
- Managed support:
  - RDS supports it on all versions through `shared_preload_libraries`, with per-user and
    per-database configuration [S-RDSPGA, S-RDSPGA2].
  - Cloud SQL enables it with the flag `cloudsql.enable_pgaudit` [S-CSQLEXT].
  - Azure lists 18.0 for PG18 and 16.0 for PG17, via preload [S-AZEXT].

```sql
ALTER ROLE agent_7f3a SET pgaudit.log = 'write, ddl, role, misc_set';
ALTER ROLE agent_7f3a SET pgaudit.log_parameter = on;    -- beware PII in logs
```

### 5.6 Event triggers as DDL guards (and their limits)

- **Events:** `ddl_command_start`, `ddl_command_end`, `sql_drop`, `table_rewrite`, and
  `login` (PG17) [S-PG-ETDEF].
- **Coverage:** `CREATE`, `ALTER`, `DROP`, `COMMENT`, `GRANT`, `REVOKE`,
  `IMPORT FOREIGN SCHEMA`, `REINDEX` (PG17), `REFRESH MATERIALIZED VIEW`, `SECURITY LABEL`
  and `SELECT INTO` [S-PG-ETDEF, S-PG17RN].
- **Not covered:**
  - shared objects (databases, roles, tablespaces, parameter privileges);
  - `ALTER SYSTEM`;
  - event-trigger commands themselves;
  - **`TRUNCATE`**, which the PG16 firing matrix omits [S-PG-ETDEF, S-PG-ETM16].
- **Failure semantics:** if a `ddl_command_start` trigger raises, the command does not run. If
  a `ddl_command_end` trigger raises, the statement rolls back. Event triggers never run in
  aborted transactions [S-PG-ETDEF].
- **Who can create them:** superusers only [S-PG-CET].
  - RDS lets the master user create them. They are not allowed on read replicas, and they
    must be dropped before a major version upgrade [S-RDSET].
  - Cloud SQL's `cloudsqlsuperuser` can create them [S-CSQLUSERS].
  - **UNVERIFIED** for Azure.
- **Escape hatches:** `event_triggers = off` (PG17) or single-user mode disables them
  [S-PG-CET]. An agent must not hold a role that can set it.

Guard: block destructive DDL from agent roles and allow it from the migration runner.

```sql
CREATE FUNCTION guard.block_agent_drop() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF pg_has_role(session_user, 'agents', 'MEMBER') THEN
    RAISE EXCEPTION 'agent % may not run %', session_user, tg_tag
      USING ERRCODE = 'insufficient_privilege';
  END IF;
END $$;
CREATE EVENT TRIGGER no_agent_drop ON ddl_command_start
  WHEN TAG IN ('DROP TABLE','DROP SCHEMA','DROP INDEX','ALTER TABLE','DROP VIEW')
  EXECUTE FUNCTION guard.block_agent_drop();
-- TRUNCATE is invisible to event triggers: revoke it, or add per-table triggers.
REVOKE TRUNCATE ON ALL TABLES IN SCHEMA app FROM cap_write;
CREATE TRIGGER no_truncate BEFORE TRUNCATE ON app.memories
  FOR EACH STATEMENT EXECUTE FUNCTION guard.block_truncate();
```

### 5.7 Capturing an agent's changes with logical decoding (and using it for undo)

- **Requirements:**
  - `wal_level = logical`, plus a slot and an output plugin (`pgoutput`, `wal2json`, or
    `test_decoding`).
  - Slots hold WAL and catalog rows until they are consumed. An abandoned slot fills the disk
    and can force a shutdown to prevent wraparound [S-PG-LD].
  - PG18 adds `idle_replication_slot_timeout` [S-PG18RN].
  - PG19 can enable logical decoding from `wal_level = replica` without a restart
    (`effective_wal_level`) [S-PG19].
- **wal2json** emits INSERT/UPDATE/DELETE, with old keys or identity according to `REPLICA
  IDENTITY`. TRUNCATE appears in format v2 only. **It never emits DDL.** Options
  `include-xids`, `include-timestamp`, `include-lsn` and table filters exist [S-WAL2JSON].
- **Undo recipe:** decode the agent's transactions (filter by xid or by the agent's role via an
  audit join), then invert them: DELETE becomes INSERT of the old row, and UPDATE becomes
  UPDATE back to the old image (needs `REPLICA IDENTITY FULL`). Limits:
  - DDL, `TRUNCATE` (v1), sequences, large objects and side effects outside the database
    cannot be inverted.
  - Concurrent writers may have built on the rows since.
- **Simpler for PG18:** `RETURNING OLD.*, NEW.*` [S-PG18RN] lets an executor (pg_sage's
  `Executor.Apply`) write its own undo log in the same statement.
- **Managed:** RDS, Cloud SQL and Azure support logical decoding: `wal2json` is listed on RDS
  and Azure, and Cloud SQL lists `pgoutput` and `pglogical` [S-RDSEXT, S-AZEXT, S-CSQLEXT].
  **UNVERIFIED in this session:** the enabling flags (`rds.logical_replication`,
  `cloudsql.logical_decoding`, Azure `wal_level`).
- Azure **automatically drops** an unused logical slot when storage passes a threshold
  [S-AZLIMITS]. A capture pipeline must survive losing its slot.

---

## 6. Branching and cloning

### 6.1 Inside PostgreSQL

| Primitive | Speed and cost | Constraints |
|---|---|---|
| `CREATE DATABASE d TEMPLATE t STRATEGY WAL_LOG` (PG15 default) | Copies block by block and WAL-logs every block, so time and WAL grow with size; good for small templates [S-PG-CREATEDB] | **No other session may be connected to the template**; not allowed in a transaction block; needs `CREATEDB` [S-PG-CREATEDB] |
| `... STRATEGY FILE_COPY` | One WAL record per tablespace; needs a checkpoint before and after [S-PG-CREATEDB] | Same |
| **PG18 `file_copy_method = clone`** with `FILE_COPY` | Lets the kernel share blocks (`copy_file_range` on Linux and FreeBSD, `copyfile` on macOS) [S-PG-RES18]. The `pg_upgrade --clone` docs name Btrfs, XFS created with reflink, and APFS as reflink filesystems [S-PG-UPG] | Same template constraint. **UNVERIFIED:** timings, ZFS behaviour, and whether managed services allow it (unlikely, since they control storage) |
| `pg_dump` / `pg_restore -j` | Proportional to data plus index rebuilds; PG18 can also dump planner statistics (`--statistics`) [S-PG18RN] | Logical copy; fine for schema-only or seed branches |
| Logical replication / `pg_createsubscriber` (PG17) | Initial sync proportional to size, then continuous [S-PG17RN] | Creates a writable copy that stays in sync until detached |
| `pg_combinebackup` / `pg_upgrade --clone` / `--copy-file-range` | Block sharing on reflink filesystems [S-PG-UPG] | Cluster level |

Template-based agent sandboxes on one self-managed cluster (PG18, reflink filesystem):

```sql
-- postgresql.conf: file_copy_method = clone   (PG18+)
-- "golden" template is a quiesced copy refreshed by pg_sage, never the live production DB
CREATE DATABASE agent_7f3a_sbx TEMPLATE golden_app STRATEGY FILE_COPY OWNER app_owner;
-- teardown
DROP DATABASE agent_7f3a_sbx WITH (FORCE);            -- PG13+
```

### 6.2 Provider and vendor copy-on-write landscape

| Provider | Mechanism | Speed (documented) | Cost model (documented) | API surface | Limits and caveats |
|---|---|---|---|---|---|
| **Neon** | Copy-on-write branches on the pageserver | "Instantaneous"; branch from any point in the history window (6 h Free, up to 7 d Launch, up to 30 d Scale) [S-NEON1, S-NEONPRICE] | Launch $0.106/CU-h, Scale $0.222/CU-h; storage $0.35/GB-month; instant-restore history $0.20/GB-month; 10 (Launch) or 25 (Scale) branches included, then **$1.50 per branch-month**; separate "Agent plan" for platforms [S-NEONPRICE] | `POST /projects/{id}/branches` with `parent_id`, `parent_lsn`, `parent_timestamp`, `init_source: schema-only`, `expires_at`, `protected`, optional `endpoints` [S-NEONAPI]; MCP server with `prepare/complete_database_migration` [S-NEONMCP] | Protected branches: Launch 2, Scale 5 [S-NEONPROT]. Anonymized branches are beta (6.3). Databricks said in May 2025 that over 80% of Neon databases were created by agents [S-DBX] |
| **Databricks Lakebase** | Neon-style copy-on-write | Instant, independent of size [S-LAKE] | **Expiring branches pay only for changed data; permanent branches pay for full size** [S-LAKE] | Branch API (details not in fetched page) | Limit on concurrently active compute [S-LAKE] |
| **Xata** (open source, Apache 2.0, on CloudNativePG) | Storage-level copy-on-write | Terabytes "in seconds" [S-XATA, S-XATAOSS] | n/a (BYOC/self-host) | CLI and API: `branches`, `clone`, `roll`, `stream` [S-XATA] | Storage technology **UNVERIFIED**; the OSS README advises against public multi-tenant use [S-XATAOSS] |
| **DBLab Engine** (Postgres.ai, Apache 2.0) | ZFS (default) or LVM thin clones | **1 TiB in about 10 s**; dozens of clones per host [S-DBLAB] | One VM plus storage | REST API, CLI, UI; PG10 to 18 [S-DBLAB] | Single host; for RDS and other managed sources it needs a VM that refreshes from the source [S-DBLAB]. **pg_sage already has an adapter** (6.4) |
| **Aurora PostgreSQL** | Storage copy-on-write clone (`restore-db-cluster-to-point-in-time --restore-type copy-on-write`) | Volume creation is fast; you still add an instance afterwards (minutes, **UNVERIFIED**) [S-AUR] | Clone pays only for pages it changes; shared pages bill to the source [S-AUR] | RDS API; cross-account via RAM; cross-VPC [S-AUR] | **15 copy-on-write clones, then full copies**; same Region only [S-AUR] |
| **Cloud SQL** | Standard clone (full backup, then copy) or **fast clone** (Instant Snapshots, same zone, metadata-only) | Fast clone takes seconds to minutes; standard clone grows with size [S-CSQLCLONE] | A new instance at normal price (**UNVERIFIED**) | `gcloud sql instances clone`, also point-in-time | Whole instance; no cross-project; replicas cannot be cloned [S-CSQLCLONE] |
| **AlloyDB** | Clone from continuous backup into a new cluster | Long-running operation; no time stated [S-ALLOYBKP] | New cluster | API | PITR window 14 d default, 1 to 35 d [S-ALLOYBKP] |
| **Azure Flexible Server** | PITR or fast restore (snapshot only) to a **new server** | Minutes to hours [S-AZBKP] | New server | ARM/CLI | No copy-on-write branches. **HorizonDB** (preview) is log-structured but documents no branching [S-HORIZON] |
| **Tiger Data** | `tiger service fork` (CLI) [S-TIGERCLI] | **UNVERIFIED** ("zero-copy forks" marketing not confirmed) | **UNVERIFIED** | CLI and MCP (`tiger mcp install`) [S-TIGERCLI] | n/a |
| **Supabase** | Branches are fresh environments built from migrations | n/a | Compute billed (**UNVERIFIED** rate) | GitHub per-PR preview branches; dashboard branching (beta) | **No production data by default**; seed files or "Include data" option [S-SUPABR] |
| **PlanetScale Postgres** | Branches are new clusters, empty or restored from a backup | Minutes [S-PSBR] | Dev branches about $5/month [S-PSBR] | Console/API | **No deploy requests; no schema merge between branches** [S-PSBR] |
| **Prisma Postgres** | `npx create-db` throwaway databases | Seconds [S-PRISMA] | Free; auto-deleted after **24 h** unless claimed [S-PRISMA] | CLI | Not a branch of existing data |

### 6.3 Takeaways

- Data-bearing copy-on-write branches exist only at Neon, Lakebase, Xata, DBLab (any source,
  self-hosted), Aurora (capped at 15), and Cloud SQL fast clone (same zone). **pg_sage cannot
  build storage-level copy-on-write.** It can orchestrate it (DELEGATE) and enforce lifecycle
  on top: TTL, quota and teardown.
- For self-managed PG18 on Btrfs or XFS, `CREATE DATABASE ... STRATEGY FILE_COPY` with
  `file_copy_method=clone` is a cheap in-cluster branch that **pg_sage can build itself**. It
  shares the cluster's CPU, WAL and connections with production, so it suits sandboxes, not
  prod-isolated rehearsals.

### 6.4 pg_sage today (cloning)

- `clone.Provider` exposes `Create/Destroy/SnapshotAge` (`sidecar/internal/clone/provider.go:8-23`).
- **DLEProvider** calls the DBLab HTTP API (`sidecar/internal/clone/dle_provider.go:29-133`).
  It is wired into the MCP migration runtime
  (`sidecar/cmd/pg_sage_sidecar/mcp_migration_runtime.go:36`).
- **SnapshotProvider** wraps a `SnapshotAPI` described as "implemented by RDS/Aurora, Cloud
  SQL, or AlloyDB adapters" (`sidecar/internal/clone/snapshot_provider.go:15-21`). A grep for
  `LatestSnapshot(` finds no non-test implementation of `SnapshotAPI`.
- `gameday.LocalProvider` hands out an operator-named database and copies nothing
  (`sidecar/internal/gameday/local.go:14-49`).
- Migration rehearsal uses the provider and refuses stale clones
  (`sidecar/internal/migration/rehearsal/orchestrator.go:14-40`).
- **Gaps:**
  - no adapters for Neon, Lakebase or Aurora copy-on-write branches, or Cloud SQL fast clone;
  - no in-cluster template provider (`CREATE DATABASE ... TEMPLATE`, PG18 clone).

---

## 7. Data protection for agents

### 7.1 PostgreSQL Anonymizer (`anon`)

- Masking rules are declared as security labels:
  `SECURITY LABEL FOR anon ON COLUMN t.c IS 'MASKED WITH FUNCTION anon.fake_email()'`.
  Masked roles are labelled the same way.
- Methods: anonymous dumps, static masking, dynamic masking, replica masking, masking views,
  masking data wrappers. Techniques include fakes, partial scrambling, shuffling and
  generalization, plus PII detection functions [S-ANON].
- Managed availability:
  - **Azure:** `anon` 2.5.1 on PG13 to 18, via preload [S-AZEXT].
  - **Cloud SQL:** `postgresql_anonymizer` **1.0.0** behind `cloudsql.enable_anon`, an old
    major [S-CSQLEXT].
  - **RDS:** not listed [S-RDSEXT].
  - **Neon:** anonymized branches (beta) use the extension with **static** masking. The branch
    is unavailable while masking runs, and foreign-key columns cannot be masked directly
    [S-NEONANON].

```sql
-- Mask in a branch before an agent sees it (static), PG14+ with anon 2.x
SECURITY LABEL FOR anon ON COLUMN app.users.email IS 'MASKED WITH FUNCTION anon.dummy_free_email()';
SECURITY LABEL FOR anon ON COLUMN app.users.full_name IS 'MASKED WITH FUNCTION anon.dummy_name()';
SELECT anon.anonymize_database();   -- static masking: rewrites data in place (branch only!)
```

### 7.2 Masking without extensions (works everywhere)

Column privileges plus a `security_barrier` view in a schema the agent can read:

```sql
REVOKE SELECT ON app.users FROM cap_read;
GRANT SELECT (id, created_at, plan) ON app.users TO cap_read;        -- column-level grant
CREATE VIEW agent_api.users WITH (security_barrier, security_invoker = false) AS
  SELECT id, plan, left(email, 1) || '***@' || split_part(email, '@', 2) AS email_masked
  FROM app.users;                                                     -- owner reads, agent sees mask
GRANT USAGE ON SCHEMA agent_api TO cap_read;
GRANT SELECT ON agent_api.users TO cap_read;
```

Here `security_invoker = false` is deliberate: the view reads as its owner. Combine it with an
RLS predicate in the view, or `security_barrier` predicates, for row scoping [S-PG-VIEW].

### 7.3 Synthetic data

- **Snaplet Seed** (supabase-community, MIT) generates data from the schema, deterministically
  [S-SEED].
- **Neosync** was acquired by Grow Therapy and archived on 2025-08-30 [S-NEOSYNC].
- Xata advertises PII-removed staging branches. The mechanism (pgstream transformers) is
  **UNVERIFIED** [S-XATA].
- Generated data protects PII but loses the skew that makes production plans realistic. For
  pg_sage's rehearsal evidence, prefer masked production clones and keep planner statistics
  (PG18 `pg_dump --statistics`) [S-PG18RN].

---

## 8. Safety nets

### 8.1 PITR and WAL archiving (self-managed)

- **Core:**
  - `archive_mode`, plus `archive_command` or an `archive_library` module;
  - a base backup plus continuous WAL;
  - recovery targets by time, LSN, xid or name (`pg_create_restore_point()`).
  - **PITR restores the whole cluster, never one table or database** [S-PG-PITR].
  - PG17 adds incremental base backups (`summarize_wal`, `pg_combinebackup`) [S-PG17RN].
- **pgBackRest 2.59.3** (2026-10-04) fixes possibly weak encryption subkeys and salts, so
  encrypted repositories should upgrade. 2.59.x supports PG19 betas. Features: block
  incremental, multiple repositories, encryption, parallel and delta restore [S-PGBR-REL,
  S-PGBR].
- **Barman 3.20.1** (2026-09-29, EDB) [S-BARMAN].
- **WAL-G:** delta backups, PITR, LZ4/LZMA/ZSTD/Brotli, libsodium/PGP/KMS encryption; actively
  developed [S-WALG].
- **pg_sage hook:** before an agent's risky action, `SELECT pg_create_restore_point('agent-7f3a-<action-id>')`
  gives an exact named recovery target, provided the user can execute it (superuser by
  default; **UNVERIFIED** on managed services).

### 8.2 Provider PITR and instant restore

| Provider | Restore model | Window | Speed |
|---|---|---|---|
| RDS | New instance; logs shipped to S3 every 5 min | Within backup retention | Volumes load lazily from S3 after "available" [S-RDSPITR] |
| Aurora | Cluster PITR or copy-on-write clone (6.2) | Retention | **UNVERIFIED** |
| Cloud SQL | PITR as a clone to a new instance [S-CSQLCLONE] | Retention | Fast clone is same-zone only |
| AlloyDB | New cluster; microsecond granularity | 14 d default, 1 to 35 d [S-ALLOYBKP] | Not stated |
| Azure Flexible | New server; latest, custom or **fast restore** (snapshot, no log replay) | 7 to 35 d; up to 7 on-demand backups; LTR up to 10 years via `pg_dump` | Minutes to hours [S-AZBKP] |
| Azure HorizonDB (preview) | Snapshot | **7 d fixed** in preview [S-HORIZON] | n/a |
| Neon | **Instant restore of a root branch in place**, with an automatic `_old_` backup branch; Time Travel queries | 6 h to 30 d by plan | **Seconds** [S-NEONRESTORE] |

### 8.3 Deletion protection

- **RDS:** deletion protection is on by default for console-created instances. Deletion
  offers a final snapshot and retained automated backups, and AWS Support may recover an
  instance up to 6 days after deletion [S-RDSDEL].
- **Cloud SQL:** deletion protection blocks only instance deletion. It does not cover stop,
  edit, backup deletion or project deletion, and replicas do not inherit it [S-CSQLDEL].
- **Azure:** resource locks; deleted servers can sometimes be restored [S-AZBKP].
- **Neon:** protected branches cannot be deleted or reset, and roles on child branches get new
  passwords [S-NEONPROT].
- **Inside the database** (agent-level), each control matches a section above:
  - no ownership (2.1);
  - `REVOKE TRUNCATE` and `BEFORE TRUNCATE` triggers (5.6);
  - an event trigger on `ddl_command_start` and `sql_drop` (5.6);
  - **pg-safeupdate**, which rejects `UPDATE`/`DELETE` without `WHERE`. Load it with
    `session_preload_libraries` or `LOAD`. A session can turn it off with
    `SET safeupdate.enabled = 0`, and `WHERE 1=1` defeats it, so it is a seatbelt, not a lock
    [S-SAFEUPD].

### 8.4 Undo approaches compared

| Approach | Granularity | Covers DDL? | Cost | Notes |
|---|---|---|---|---|
| Branch before act, then discard or promote (Neon reset-from-parent, DBLab, template DB) | Whole database | Yes | Low on copy-on-write | **Best default for agents.** Merge-back is the hard part: PlanetScale states it has no Postgres schema merge [S-PSBR] |
| Provider instant restore (Neon) | Branch | Yes | Seconds | Rewinds every writer on that branch [S-NEONRESTORE] |
| PITR to a new instance, then copy back the damaged objects | Cluster, then manual | Yes | Minutes to hours | Standard DBA recovery [S-PG-PITR] |
| Logical decoding inversion | Row | **No** | Slot management | 5.7 |
| Audit triggers (pgMemento; supa_audit) | Row, with JSONB images | pgMemento logs DDL through event triggers | Write amplification. supa_audit warns against >3k writes/s and was **archived Feb 2025** [S-SUPAAUDIT, S-PGMEMENTO] | pgMemento can rebuild past states [S-PGMEMENTO] |
| `RETURNING OLD/NEW` (PG18) executor undo log | Statement | No | Minimal | Only for writes pg_sage itself performs [S-PG18RN] |
| Temporal tables (`temporal_tables` extension; Azure 1.2.2) | Row history | No | Triggers | PG18 temporal *constraints* (`WITHOUT OVERLAPS`, `PERIOD`) are not system versioning, and PG19 `FOR PORTION OF` was reverted [S-AZEXT, S-PG18RN, S-PG19B4] |

---

## 9. Safe schema change

### 9.1 Lock primer

- Most `ALTER TABLE` forms take **ACCESS EXCLUSIVE** [S-PG-ALTERTABLE]. The lesser locks are:
  - `VALIDATE CONSTRAINT`, `SET STATISTICS`, storage parameters: SHARE UPDATE EXCLUSIVE;
  - `ADD FOREIGN KEY`, `ENABLE/DISABLE TRIGGER`: SHARE ROW EXCLUSIVE.
- A waiting ACCESS EXCLUSIVE request queues every later reader behind it. Always set
  `lock_timeout` and retry:

```sql
SET lock_timeout = '2s';            -- per attempt; retry with backoff from the runner
SET statement_timeout = '15min';    -- for the validating/backfill steps only
ALTER TABLE app.memories ADD COLUMN embedding_model text;   -- metadata-only (no default)
```

PG18 adds `log_lock_failures` (logs `NOWAIT` failures) [S-PG18RN]. PG19 turns on
`log_lock_waits` by default and adds `pg_stat_lock` [S-PG19].

### 9.2 Non-blocking recipes

| Change | Recipe | Version |
|---|---|---|
| Index | `CREATE INDEX CONCURRENTLY`: two scans, waits for older transactions, not allowed in a transaction block. A failure leaves an **INVALID** index to drop or `REINDEX CONCURRENTLY` [S-PG-CI] | 8.2+, REINDEX CONCURRENTLY 12+ |
| FK or CHECK | `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE CONSTRAINT` (SHARE UPDATE EXCLUSIVE) [S-PG-ALTERTABLE] | all |
| NOT NULL | Add a valid `CHECK (c IS NOT NULL)` first, then `SET NOT NULL` skips the scan [S-PG-ALTERTABLE]; PG18 can also add NOT NULL constraints as `NOT VALID` [S-PG18RN] | all / 18 |
| Add column with default | Non-volatile default needs no rewrite. A volatile default, stored generated column, identity, or constrained domain forces a rewrite [S-PG-ALTERTABLE] | 11+ |
| Type change | No rewrite only if binary coercible, and indexes may still rebuild [S-PG-ALTERTABLE] | all |
| Detach partition | `DETACH PARTITION ... CONCURRENTLY` [S-PG14RN] | 14+ |
| Rebuild a bloated table | `REPACK CONCURRENTLY` [S-PG19], or pg_repack/pg_squeeze (Azure lists both) [S-AZEXT] | 19 (beta) |
| Constraint without enforcement | `NOT ENFORCED` CHECK and FK [S-PG18RN]; PG19 can `ALTER ... [NOT] ENFORCED` on CHECK [S-PG19] | 18 / 19 |

### 9.3 Tooling

- **pgroll** (Xata, Apache 2.0, pre-1.0, PG14+): expand and contract through versioned schemas
  of views, dual-write triggers and backfills, with instant rollback. Clients must set
  `search_path` to a schema version [S-PGROLL].
- **Reshape** (MIT): the same view-and-trigger approach, PG12+ [S-RESHAPE].
- **squawk:** lints migrations with its own CST parser (rust-analyzer style). Ships a GitHub
  Action, an LSP and a VS Code extension, and is configured in `.squawk.toml` [S-SQUAWK].
- **pg_sage today:** the migration classifier flags missing `lock_timeout` with a regex rule
  (`sidecar/internal/migration/classifier_patterns.go:120-122`). The LLM script generator
  includes `SET lock_timeout` (`sidecar/internal/migration/llm_scripts.go:243`). Rehearsal
  runs against a `clone.Provider` (6.4).
- **Rehearse on a clone:** Neon's MCP `prepare_database_migration` and
  `complete_database_migration` run migrations on a temporary branch first [S-NEONMCP]. DBLab
  supplies production-sized clones [S-DBLAB].

---

## 10. Connection handling for bursty agents

### 10.1 Poolers

| Pooler | Status | Agent-relevant facts |
|---|---|---|
| **PgBouncer** | 1.26.0 (2026-09-23) [S-PGBCL] | 1.21: protocol-level prepared statements in transaction mode (`max_prepared_statements`). 1.23: rolling restart, replication connections, user-name maps for cert and peer authentication. 1.24: `max_user_client_connections`, `max_db_client_connections`, `KILL_CLIENT`, `client_idle_timeout`. 1.25: LDAP, direct TLS, `transaction_timeout`, `scram_iterations`. 1.26: tracks every parameter PostgreSQL reports by default (**including `search_path`**), `pool_idle_timeout`, per-user/db `query_wait_timeout`, SCRAM and packet-buffer security fixes [S-PGBCL]. Transaction mode **never** supports session `SET/RESET`, `LISTEN`, SQL `PREPARE`, session advisory locks, `WITH HOLD` cursors or `LOAD` [S-PGBFEAT] |
| **PgDog** (Rust, AGPL-3.0) | Production-ready; sharding partly experimental [S-PGDOG] | Transaction pooling that **tracks `SET`**, parses queries with Postgres's own parser for read/write routing, supports RDS IAM and Azure Workload Identity |
| **Supavisor** (Elixir) | Transaction mode; session mode listed as future work [S-SUPAVISOR] | Multi-tenant; claims 1M connections per cluster |
| **pgcat** (Rust, MIT) | Stable per README [S-PGCAT] | `SET LOCAL` and `pg_advisory_xact_lock` in transaction mode |
| **RDS Proxy** | Managed | Pins the session on `SET`, `PREPARE/EXECUTE/DEALLOCATE/DISCARD`, temp tables, cursors, `LISTEN`, `nextval/setval`, session advisory locks, `LOAD`, and statements over 16 KB. Supports an initialization query [S-RDSPROXY] |
| **Cloud SQL Managed Connection Pooling** | Enterprise Plus only | Transaction (default) or session mode; `max_prepared_statements`; IAM authentication on ports 6432 and 3307; no `SET/RESET`, `LISTEN`, `PREPARE` or advisory locks in transaction mode; `max_client_connections` 5,000 per pooler [S-CSQLMCP] |
| **Azure built-in PgBouncer** | Not on Burstable tiers [S-AZLIMITS] | Azure recommends transaction mode and reserves 15 connections [S-AZLIMITS] |

### 10.2 Serverless and edge drivers

- **Neon serverless driver:** HTTP for one-shot queries and non-interactive transactions;
  WebSocket for interactive transactions and node-postgres compatibility [S-NEONSLS].
- **Cloudflare Hyperdrive:** pooling plus caching of popular queries, on by default, for
  Workers [S-HYPERDRIVE].

### 10.3 Implications

- **Tenant context and guardrails must be transaction-scoped:** `SET LOCAL` or
  `set_config(k, v, true)`. Session state disappears, or leaks, under transaction pooling
  [S-PGBFEAT].
- **Per-agent pools:**
  - Pool by (database, role) so `current_user` RLS keeps working.
  - Cap each agent at the pooler with `max_user_client_connections` (1.24+) as well as with
    the role's `CONNECTION LIMIT` [S-PGBCL].
  - Keep `reserved_connections` with `pg_use_reserved_connections` for pg_sage and humans
    (PG16+) [S-PG16RN].
- **IAM storms:** IAM authentication on RDS costs memory (300 to 1000 MiB) [S-RDSIAM]. A pooler
  that holds IAM-authenticated server connections avoids paying it per agent connection.

---

## 11. Agent state and memory workloads

### 11.1 Vectors

- **pgvector:**
  - 0.8.0 (2024-10-30) added **iterative index scans**. They counter overfiltering when a
    `WHERE` clause removes most of the approximate nearest-neighbour candidates. Settings:
    `hnsw.iterative_scan = strict_order|relaxed_order`, `hnsw.max_scan_tuples` (default
    20,000), `hnsw.scan_mem_multiplier`, `ivfflat.iterative_scan`, `ivfflat.max_probes`
    [S-PGV, S-PGV080].
  - Releases since: 0.8.1 (2025-09-04, PG18), 0.8.2 (2026-02-25, parallel HNSW build buffer
    overflow fixed), **0.8.3 (2026-06-17, possible index corruption during HNSW vacuum
    fixed)**, 0.8.4 (2026-06-30, "graph not repaired" and insert-during-vacuum fixes), up to
    **0.8.7 (2026-10-01)** [S-PGV-CL].
  - **Index dimension limits:** `vector` 2,000; `halfvec` 4,000; `bit` 64,000; `sparsevec`
    1,000 non-zeros [S-PGV].
  - **HNSW builds are much faster when the graph fits in `maintenance_work_mem`.** pgvector
    emits a notice once it no longer fits. Builds parallelize with
    `max_parallel_maintenance_workers`. Filtered workloads should use partial indexes or
    partitioning [S-PGV].
  - **Managed versions:** RDS 0.8.2 (PG17/18) [S-RDSEXT], Azure 0.8.2 [S-AZEXT], Cloud SQL
    0.8.5 [S-CSQLEXT]. **DBA check pg_sage can ship:** warn about pgvector below 0.8.3 on
    tables with HNSW indexes and heavy delete or update churn, citing the changelog.
- **pgvectorscale** (Tiger, PostgreSQL license):
  - Index and filtering: StreamingDiskANN, Statistical Binary Quantization, label-filtered
    search.
  - Benchmark claim: on 50M 768-dimension vectors at 99% recall, 28× lower p95 latency and
    16× higher QPS than a Pinecone storage-optimized index, at 75% lower cost (vendor
    benchmark).
  - Not on RDS or Cloud SQL [S-PGVS, S-RDSEXT, S-CSQLEXT].
- **VectorChord** (TensorChord, successor to pgvecto.rs, AGPLv3 or ELv2):
  - Method: IVF with RaBitQ and reranking, compatible with pgvector types.
  - Vendor claims: 100M 768-dimension vectors on an i4i.xlarge, and a 100M build in about
    20 min.
  - Current version: v1.1.1 [S-VCHORD].
- **pg_diskann:** Azure's DiskANN index, 0.6.5 [S-AZEXT].

### 11.2 Lexical / BM25

- **ParadeDB `pg_search`:** Tantivy through pgrx; AGPL-3.0 or commercial; not on RDS; ParadeDB
  Cloud and PaaS images [S-PARADE].
- **pg_textsearch** (Tiger, PostgreSQL license):
  - v1.5.0, GA, PG17 and 18 (PG19 best effort).
  - Syntax: `CREATE INDEX ... USING bm25(col)`, `ORDER BY col <@> 'query'`; Block-Max WAND
    and parallel builds.
  - **Caveats:** RLS-blind corpus statistics, per-partition statistics, no 2PC after index
    creation [S-PGTS].
- VectorChord-bm25 is a companion extension [S-VCHORD].

### 11.3 JSONB, queues, scheduling

- **JSONB / TOAST:**
  - Values over about 2 kB are compressed and moved out of line.
  - An `UPDATE` that does not touch an out-of-line value has no TOAST cost, but any change
    rewrites that value **in full**. Agents that append to a large JSON "memory" document
    therefore rewrite the whole document on every turn.
  - Maximum 1 GB per value [S-PG-TOAST].
  - Model memories as rows (append-only, partitioned by time or agent), not one growing
    document.
  - PG19 changes the default TOAST compression to lz4 (reported by secondary sources;
    **UNVERIFIED** in the fetched release notes).
- **pgmq** (v1.10.0, PostgreSQL license):
  - Semantics: visibility-timeout queues, exactly-once processing within the timeout, archive
    tables.
  - **SQL-only install for managed services without the extension** [S-PGMQ]. Not listed on
    RDS, Cloud SQL or Azure [S-RDSEXT, S-CSQLEXT, S-AZEXT].
- **LISTEN/NOTIFY:** `NOTIFY` takes a global lock at commit. At tens of thousands of concurrent
  writers, Recall.ai saw every commit serialize behind
  `AccessExclusiveLock on object 0 of class 1262` (March 2025) [S-RECALL]. The article reports
  a later upstream fix; its version is **UNVERIFIED**. Do not build agent fan-out on `NOTIFY`.
- **pg_cron:**
  - Runs in one database (`cron.database_name`); use `cron.schedule_in_database` for others.
  - Jobs run with the scheduling user's rights; `cron.max_running_jobs` defaults to 32.
  - **`cron.job_run_details` grows without bound** unless purged.
  - Available on RDS, Cloud SQL (`cloudsql.enable_pg_cron`) and Azure [S-PGCRON, S-CSQLEXT,
    S-AZEXT].

### 11.4 What breaks at scale (checklist for pg_sage)

1. **Long agent transactions** pin the xmin horizon, which causes bloat and vacuum debt. Use
   `idle_in_transaction_session_timeout` and `transaction_timeout` (both overridable), and
   kill from the watchdog (4.1).
2. **Queue churn** (pgmq/SKIP LOCKED) piles up dead tuples. Tune autovacuum per table;
   partition queues; purge the archive.
3. **HNSW:**
   - builds that spill out of `maintenance_work_mem`;
   - vacuum cost, and corruption on pgvector < 0.8.3;
   - indexes past the dimension limits (switch to `halfvec`);
   - recall collapse under selective filters (iterative scans and partial indexes).
4. **Thousands of per-agent databases or schemas:**
   - one connection pool per database;
   - `pg_stat_statements` eviction at 5,000 entries [S-PG-PGSS];
   - catalog growth;
   - autovacuum workers shared across every database;
   - one pg_cron database.
5. **Connection storms:** each agent opens a pool. Cap at the pooler and the role, and keep
   reserved slots for pg_sage (10.3).
6. **NOTIFY fan-out** serializes commits (11.3).
7. **Growing JSONB documents:** TOAST rewrites and WAL amplification.
8. **Logical slots** left behind by abandoned capture pipelines fill the disk; Azure drops
   them automatically [S-AZLIMITS].

---

## 12. MCP, the MCP Registry, A2A, and read-only enforcement

### 12.1 MCP specification timeline

| Revision | Key changes (source) |
|---|---|
| 2025-06-18 | JSON-RPC batching removed; **structured tool output** (`outputSchema`, `structuredContent`); **MCP servers become OAuth resource servers with Protected Resource Metadata**; clients MUST send RFC 8707 resource indicators; **elicitation**; resource links; `MCP-Protocol-Version` header [S-MCP1] |
| 2025-11-25 | OIDC discovery; icons; **incremental scope consent** through `WWW-Authenticate`; tool-name guidance; enum and default improvements; **URL-mode elicitation**; **tool calling inside sampling**; **Client ID Metadata Documents** recommended; **experimental tasks**; RFC 9728 alignment; JSON Schema 2020-12 as default [S-MCP2] |
| **2026-07-28 (current)** | **Protocol sessions and `Mcp-Session-Id` removed**; no `initialize`; version and client capabilities in every request's `_meta`; **`server/discover` mandatory**; `subscriptions/listen` replaces the GET stream and resource subscribe; `ping` and `logging/setLevel` removed; **tasks moved to an extension (`io.modelcontextprotocol/tasks`) with `tasks/get` and `tasks/update`**; **MRTR (`InputRequiredResult`) replaces server-initiated requests**; `resultType` required; SSE resumability removed; OpenTelemetry `_meta` keys; `Mcp-Method`/`Mcp-Name` headers and `x-mcp-header`; `ttlMs` and `cacheScope` on list results; `iss` validation (RFC 9207); **Roots, Sampling, Logging and DCR deprecated**; feature lifecycle policy with a minimum 12-month deprecation window [S-MCP3, S-MCP4] |

Points that matter for a database MCP server such as pg_sage:

- **Authorization** (2026-07-28) [S-MCP5]:
  - Optional overall. A stdio server should read credentials from the environment instead.
  - If supported over HTTP: the server is an OAuth 2.1 resource server, **MUST serve RFC 9728
    Protected Resource Metadata**, MUST validate that tokens were issued for its own audience,
    and **MUST NOT accept or forward other tokens**.
  - It returns 401 with `WWW-Authenticate: Bearer resource_metadata=..., scope=...`, and 403
    `insufficient_scope` for step-up.
  - Clients SHOULD use CIMD rather than the deprecated DCR.
  - Extensions: **Enterprise-Managed Authorization (stable)** and **Client Credentials
    (draft)** for machine-to-machine use [S-MCPEXTAUTH].
- **Tools** [S-MCP6]:
  - Annotations (`readOnlyHint`, etc.) are **untrusted unless the server is trusted**.
  - `tools/list` may vary by the caller's authorization, but not by connection.
  - Servers MUST validate inputs, enforce access control, rate-limit and sanitize outputs.
  - Stateful work (a database transaction, a branch) uses **explicit server-minted handles**,
    checked against the caller on every call.
- **Elicitation** [S-MCP7]:
  - Form mode MUST NOT ask for secrets; URL mode handles sensitive interactions.
  - Clients must show which server is asking and offer decline and cancel.
  - Under MRTR, approval becomes a retry carrying `inputResponses`.
  - This channel is a natural fit for "confirm this destructive action". It is **not**
    authorization: the server must bind the request to the authenticated user.
- **Security best practices** [S-MCP8]: confused deputy (per-client consent), the token
  passthrough ban, SSRF during metadata discovery, **state-handle hijacking** (bind handles to
  the user, not to possession), and scope minimization (no omnibus `db:*` scopes).

**pg_sage today (MCP).**

- Version handling (`sidecar/internal/mcp/protocol.go:11-24`):
  - `ModernVersion = "2026-07-28"`, `LatestLegacyVersion = "2025-11-25"`;
  - supports 2025-06-18 and 2025-03-26;
  - answers `server/discover` and `subscriptions/listen`.
- Tools return `structuredContent` (e.g. `sidecar/internal/mcp/agent_tools_write.go:126-130`)
  and set `readOnlyHint` (`sidecar/internal/mcp/registry.go:13,41`).
- **Gap:** HTTP authentication uses opaque pg_sage MCP tokens. There is no
  `/.well-known/oauth-protected-resource`, and the 401 carries no `resource_metadata`
  (`sidecar/internal/api/mcp_principal.go:20-33`). Hosted MCP clients that expect OAuth
  discovery cannot onboard without manual token setup.

### 12.2 MCP Registry

- Preview launched on 2025-09-08; **API freeze (v0.1) on 2025-10-24**. GA is planned, and the
  preview may still bring breaking changes or data resets [S-MCPREG].
- Publishing uses `mcp-publisher` and a `server.json`. The registry stores **metadata, not
  code**.
- Namespaces are verified through GitHub OAuth/OIDC (`io.github.<user>`) or DNS/HTTP for
  domains.
- **No security scanning or moderation is documented** [S-MCPREG]. Treat the registry as
  discovery, not as a trust signal.

### 12.3 A2A (Agent2Agent)

- **v1.0.0 on 2026-03-12; v1.0.1 on 2026-05-28.** v0.3.0 dated from 2025-07-30.
- 1.0 changes [S-A2AREL]:
  - OAuth modernized: implicit and password flows removed; device code and PKCE added.
  - The protocol definition is separated from the transport mappings.
  - `tasks/list` added.
  - Multi-tenancy for gRPC.
- Spec content: agent cards (which can be signed), JSON-RPC, gRPC and HTTP/REST bindings;
  OAuth2, OIDC, mTLS and API keys; a task lifecycle with input-required and auth-required
  states. The site says A2A joined the Agentic AI Foundation [S-A2A].
- Relevance: A2A is how *other agents* might delegate a DBA task to pg_sage. MCP stays the
  tool interface.

### 12.4 How database MCP servers implement "read-only"

| Server | Mechanism | Known bypass or limit |
|---|---|---|
| Anthropic reference `@modelcontextprotocol/server-postgres` (archived 2025-05-29) | Wraps each query in `BEGIN TRANSACTION READ ONLY` | **`COMMIT; DROP SCHEMA public CASCADE;`**: node-postgres simple queries accept several statements, so `COMMIT` ends the read-only transaction (Datadog, 2025-08-21). About 21,000 weekly npm downloads after archival. Zed's fork 0.1.4 fixes it with prepared statements and per-call connections [S-DD1, S-ARCHIVED] |
| Postgres MCP Pro (crystaldba) | "Restricted" mode: pglast parse rejects `COMMIT`/`ROLLBACK`, read-only transaction, execution time limit | Its docs warn that unsafe procedural languages can get around the protection [S-PGMCPPRO] |
| Supabase MCP | `read_only=true` runs SQL as a read-only Postgres user; project scoping; feature groups; results wrapped in anti-injection instructions | **Prompt-injection exfiltration** (General Analysis, 2025-07-08): a support ticket made an agent running with `service_role` (BYPASSRLS) read `integration_tokens` and write them back into the ticket. Read-only does not stop exfiltration [S-GA, S-SUPAMCP] |
| Neon MCP | `readonly=true` hides write tools; OAuth remote server | Neon advises against production use [S-NEONMCP] |
| MCP Toolbox for Databases (Google) | Prebuilt `execute_sql` versus custom parameterized tools; integrated IAM | Arbitrary-SQL tools are only as safe as the database role (**UNVERIFIED** detail) [S-TOOLBOX] |
| **pg_sage** EXPLAIN (ANALYZE) | pg_query parse allows **exactly one SELECT**, then a **catalog proof** that every function, operator, cast and type is immutable or stable, plus a deny-list of SQL-running functions such as `query_to_xml`, then relation and view checks, then `BEGIN READ ONLY` with `SET LOCAL statement_timeout` and `transaction_read_only` | Anything unknown is refused (`sidecar/internal/sqlast/readquery_cgo.go:12-29`, `sidecar/internal/explain/analyze_guard.go:15-46`, `sidecar/internal/explain/explain.go:319-335`). Parser grammar is PG17 [S-PGQGO] |

### 12.5 Bypass catalogue for READ ONLY alone

A READ ONLY transaction blocks INSERT, UPDATE, DELETE, MERGE, `COPY FROM` into non-temporary
tables, every CREATE, ALTER and DROP, COMMENT, GRANT, REVOKE, TRUNCATE, and EXPLAIN ANALYZE or
EXECUTE of those. The documentation calls it "a high-level notion of read-only that does not
prevent all writes to disk" [S-PG-TX]. Bypass classes:

1. **Transaction-control injection.** With the simple query protocol, a multi-statement string
   runs in one implicit transaction unless it contains transaction control [S-PG-PROTO], so
   `COMMIT; <write>` escapes [S-DD1]. A Parse message (extended protocol) **cannot contain
   more than one statement** [S-PG-PROTO].
2. **Default-based read-only.** `default_transaction_read_only` is USERSET, so
   `SET default_transaction_read_only = off` or `BEGIN READ WRITE` defeats a role-level
   default [S-PG-CLIENT]. An explicit `BEGIN READ ONLY` cannot be switched to read-write after
   the first query, nor inside a subtransaction ("transaction read-write mode must be set
   before any query") [S-PG-VARC].
3. **Side-effecting functions that READ ONLY allows** (pg_sage's guard lists the same set):
   - `pg_terminate_backend` and `pg_cancel_backend` (same role, or `pg_signal_backend`);
   - `set_config`; session advisory locks, which survive the transaction;
   - `pg_sleep` and cartesian joins (denial of service);
   - `lo_*` server-file functions (privileged);
   - **`dblink_exec`**, which opens a new, non-read-only connection;
   - **`postgres_fdw` writes, until PG19** [S-PG19];
   - untrusted procedural-language functions [S-PGMCPPRO].
4. **`COPY ... TO` a file or `PROGRAM`** for privileged roles (`pg_write_server_files`,
   `pg_execute_server_program`, superuser). Only `COPY FROM` is restricted
   [S-PG-TX, S-PG-PREDEF]. This is derived from the documented restriction list; **UNVERIFIED
   by test.**
5. **Over-broad read roles.** Superuser or BYPASSRLS read roles see every tenant and every
   secret. `pg_read_all_data` bypasses grants but not RLS [S-PG-PREDEF].
6. **Data exfiltration** through the agent itself ("lethal trifecta"). Read-only does nothing
   here; only data minimization, masking, output caps and human review help [S-GA].
7. **Hot standby.** A replica is truly read-only at the server, but item 3 (except writes) and
   item 6 still apply.

**Recommended layered design** (generalize pg_sage's guard to every agent SQL path):

- **L0 Identity:** a per-agent login; no ownership; only the grants it needs; no dangerous
  predefined roles; RLS plus column grants.
- **L1 Transport:** extended protocol only (pgx with arguments, or
  `QueryExecModeCacheStatement`). Never `QueryExecModeSimpleProtocol` for agent SQL; pg_sage
  uses simple protocol only for PgBouncer admin (`sidecar/internal/sre/pooler/admin.go:27`).
- **L2 Parse allowlist:** exactly one statement; deny transaction control, `SET`/`RESET`, `DO`,
  `CALL`, `COPY`, `LOCK`, `LISTEN`/`NOTIFY`, `PREPARE`/`EXECUTE`, and `EXPLAIN ANALYZE` of
  non-SELECT statements.
- **L3 Catalog proof:** functions, operators, casts and types are non-volatile and not on a
  deny-list (`dblink*`, `pg_terminate_backend`, `lo_*`, `set_config`, `query_to_xml*`,
  advisory locks); follow views to a bounded depth.
- **L4 Server enforcement:** `BEGIN READ ONLY`, `SET LOCAL statement_timeout`/`lock_timeout`,
  a role-level `temp_file_limit`, or a hot-standby route.
- **L5 Output controls:** row and byte caps, masking (7.x), result framing that resists
  injection, and per-call audit carrying the agent's identity (5.1).

---

## 13. Requirement-to-primitive map: build, delegate, or cannot

**BUILD** means pg_sage implements it in the sidecar against standard SQL and catalog
interfaces, through `policy.Gate` and `Executor.Apply`. **DELEGATE** means pg_sage orchestrates
a provider or IdP API and verifies the result. **CANNOT** means no supported primitive exists;
pg_sage can only detect, alert and recommend.

| # | Requirement | Primitive(s) | Min PG | Managed caveats | pg_sage | Notes |
|---|---|---|---|---|---|---|
| 1 | Distinct database identity per agent | Login role per agent; capability roles; PG16 `WITH INHERIT/SET` | 14 (16 for grant options) | Master or admin role creates roles everywhere | **BUILD** | Provision, rotate and retire roles; never share a login among agents (2.1, 2.3) |
| 2 | IdP-issued, short-lived agent credentials | PG18 `oauth` and a validator; RDS/Cloud SQL/AlloyDB/Entra IAM tokens; Vault dynamic roles | 18 (oauth) | Managed: provider IAM only (**UNVERIFIED** no `oauth`) | **DELEGATE** (token issuance) + **BUILD** (role mapping, grants) | Tokens are checked only at login; pair with session kill (2.5) |
| 3 | Revocation and kill switch | `NOLOGIN`/`CONNECTION LIMIT 0`, `REVOKE CONNECT`, `pg_terminate_backend` with `pg_signal_backend` | 14 | Cannot signal superuser backends | **BUILD** | Reversible: record the prior attributes (2.5) |
| 4 | Least privilege, no destruction | No ownership; no TRUNCATE/TRIGGER; default privileges; revoke `public` CREATE | 14 (15 changes the `public` default) | Same | **BUILD** | Audit grants continuously (2.1) |
| 5 | Row or tenant isolation | RLS on `current_user`; FORCE RLS; `security_invoker` views | 15 for `security_invoker` | Same; keep minors current for the CVEs | **BUILD** (advise and verify) | GUC tenancy is spoofable by raw-SQL agents (3.2) |
| 6 | Hard isolation | Database, branch or instance per agent | n/a | Copy-on-write needs Neon/Lakebase/Xata/DBLab/Aurora | **DELEGATE** (branch) / **BUILD** (CREATE DATABASE) | 6.1 to 6.3 |
| 7 | Statement, lock and transaction time limits | `statement_timeout`, `lock_timeout`, idle timeouts, `transaction_timeout` | 14 (17 for `transaction_timeout`) | Same | **BUILD** (re-apply per transaction plus watchdog) | Role defaults can be overridden (4.1) |
| 8 | Temp/spill limit per agent | `temp_file_limit` (SUSET) | 14 | Needs master/superuser to set per role (**UNVERIFIED** on Cloud SQL and Azure) | **BUILD** | Hard limit (4.1) |
| 9 | CPU, I/O and memory caps per agent | None in community PG | n/a | Provider compute caps per branch or instance | **CANNOT** (in-DB) / **DELEGATE** (separate compute) | EDB RM is EPAS-only; pg_cgroups archived (4.2) |
| 10 | Storage quota per agent | None | n/a | Provider project or branch limits | **CANNOT** (enforce) / **BUILD** (monitor and fence) | `pg_database_size` then `CONNECTION LIMIT 0` (4.2) |
| 11 | Connection caps and burst absorption | Role `CONNECTION LIMIT`; PgBouncer `max_user_client_connections`; `reserved_connections` | 14 (16 for reserved) | RDS Proxy pinning; Cloud SQL MCP needs Enterprise Plus | **BUILD** (configure and verify) / **DELEGATE** (managed poolers) | 10.1, 10.3 |
| 12 | Trustworthy attribution | Per-agent login; `SYSTEM_USER`; `log_connections`; pgaudit | 16 (`SYSTEM_USER`) | pgaudit through preload everywhere | **BUILD** | `application_name` and sqlcommenter are hints only (5.1) |
| 13 | Cost attribution per agent | `pg_stat_statements` by `userid`; PG18 per-backend I/O and WAL; provider billing tags | 14 (18 per-backend) | Provider bills per instance or branch | **BUILD** (in-DB) + **DELEGATE** (billing APIs) | Watch queryid explosion (5.2) |
| 14 | Immutable audit of agent DML and DDL | pgaudit per role (SUSET); log shipping | 14 | RDS → CloudWatch; Cloud SQL flag; Azure preload | **BUILD** (configure) / **DELEGATE** (log storage, WORM) | Best effort, not transactional (5.5) |
| 15 | Block destructive DDL | Event triggers (`ddl_command_start`, `sql_drop`); REVOKE TRUNCATE; BEFORE TRUNCATE triggers | 14 (17 adds REINDEX, `event_triggers` GUC) | Creation needs master or `cloudsqlsuperuser`; RDS: not on replicas, drop before major upgrade; Azure **UNVERIFIED** | **BUILD** | TRUNCATE, roles, databases and ALTER SYSTEM are invisible to event triggers (5.6) |
| 16 | Read-only agent SQL that cannot be bypassed | L0 to L5 layers; hot standby route | 14 | Same | **BUILD** (generalize the explain guard) | READ ONLY alone is not enough (12.5) |
| 17 | Branch, sandbox or fork per task | Neon/Lakebase branch API; Aurora copy-on-write clone; Cloud SQL fast clone; DBLab; PG18 FILE_COPY clone | 18 for in-cluster clone | Azure: PITR only; Supabase/PlanetScale: no data copy | **DELEGATE** (providers) + **BUILD** (template provider, TTL, teardown) | Adapters missing except DBLab (6.4) |
| 18 | Masked data for agents | `anon` static or dynamic masking; masking views; column grants | 14 | Azure anon 2.5.1; Cloud SQL anon 1.0.0; RDS none; Neon beta | **BUILD** (views, grants, anon where available) / **DELEGATE** (Neon anonymized branches) | 7.x |
| 19 | Point-in-time recovery | WAL archiving (pgBackRest/Barman/WAL-G); provider PITR | 14 (17 incremental) | Restores to a new instance or server | **DELEGATE** (run backups) + **BUILD** (backup assurance, restore drills, restore points) | Cluster granularity only (8.1, 8.2) |
| 20 | Fast undo of one agent's changes | Branch before act; Neon instant restore; executor undo via `RETURNING OLD/NEW`; logical decoding inversion | 18 for RETURNING OLD | Logical decoding flags per provider (**UNVERIFIED**) | **BUILD** (executor undo, decoding) / **DELEGATE** (branch restore) | No DDL undo except by branch or PITR (8.4) |
| 21 | Deletion protection | RDS/Cloud SQL deletion protection; Azure locks; Neon protected branches | n/a | Cloud SQL protection is narrow | **DELEGATE** (set and verify) | 8.3 |
| 22 | Safe migrations by agents | `lock_timeout` plus retries; CONCURRENTLY; NOT VALID/VALIDATE; pgroll; squawk; rehearsal on a clone | 14 (18 NOT NULL NOT VALID; 19 REPACK) | Same | **BUILD** (classifier, rehearsal) + **DELEGATE** (clone) | pg_sage parser is PG17 grammar (9.x, 12.4) |
| 23 | Approvals and human in the loop | MCP elicitation (form or URL) under MRTR; pg_sage `approve` scope (humans only) | n/a | Client support varies (**UNVERIFIED**) | **BUILD** | Bind approvals to the authenticated user, not to elicitation alone (12.1) |
| 24 | MCP authorization that enterprises accept | RFC 9728 PRM; audience validation; scopes and step-up; CIMD; Enterprise-Managed Authorization extension | n/a | n/a | **BUILD** (resource server) + **DELEGATE** (authorization server = IdP) | Current gap at `sidecar/internal/api/mcp_principal.go:20-33` (12.1) |
| 25 | Agent memory (vectors, BM25, JSON, queues) | pgvector ≥ 0.8.3; pgvectorscale/VectorChord/pg_diskann; pg_textsearch/pg_search; pgmq; pg_cron | pgvector: 13+; pg_textsearch 17+ | RDS: pgvector 0.8.2, no pgvectorscale/pg_search/pgmq extension; Azure: pg_diskann | **BUILD** (health checks, tuning advice) | 11.4 checklist |
| 26 | PG lifecycle hygiene | Version support policy; minor updates for CVEs | n/a | Provider extended support | **BUILD** (detect and advise) | PG14 EOL 2026-11-12; PG19 GA in October 2026 (1) |

---

## 14. Open questions and UNVERIFIED items to close before the spec relies on them

1. Can any managed service enable the PG18 `oauth` pg_hba method? None found; **UNVERIFIED.**
2. Does pgx v5.x support OAUTHBEARER SASL? **UNVERIFIED.** It decides whether pg_sage can log
   in to PG18 clusters with IdP tokens.
3. Do `cloudsqlsuperuser` and `azure_pg_admin` allow `ALTER ROLE ... SET temp_file_limit`, and
   can `azure_pg_admin` create event triggers? **UNVERIFIED.**
4. Do PG18 per-backend I/O and WAL statistics survive after the backend exits? **UNVERIFIED.**
5. How fast is `CREATE DATABASE ... STRATEGY FILE_COPY` with `file_copy_method=clone` on XFS or
   Btrfs, and does it work on ZFS? **UNVERIFIED.** Benchmark before advertising it.
6. Tiger fork mechanics and pricing; Xata storage layer; Aurora clone end-to-end time.
   **UNVERIFIED.**
7. Exact logical-decoding flag names and restart needs on RDS, Cloud SQL and Azure.
   **UNVERIFIED** in this session.
8. In which PostgreSQL version did the NOTIFY global-lock fix land (Recall.ai mentions an
   upstream fix)? **UNVERIFIED.**
9. Is the PG19 default TOAST compression lz4 in the final release notes? **UNVERIFIED.**
10. Does `COPY ... TO PROGRAM` really run inside a READ ONLY transaction? Derived from the docs;
    **UNVERIFIED by test** (do not run it against production).

---

## 15. Sources

All accessed 2026-10-05. A publication or release date is listed where the source gives one.

**PostgreSQL core**
- [S-PGVER] Versioning policy (minors, EOL dates): https://www.postgresql.org/support/versioning/
- [S-PG14RN] PG14 release notes: https://www.postgresql.org/docs/14/release-14.html (GA 2021-09-30)
- [S-PG15RN] PG15 release notes: https://www.postgresql.org/docs/15/release-15.html (GA 2022-10-13)
- [S-PG16RN] PG16 release notes: https://www.postgresql.org/docs/16/release-16.html (GA 2023-09-14)
- [S-PG17RN] PG17 release notes: https://www.postgresql.org/docs/17/release-17.html (GA 2024-09-26)
- [S-PG18RN] PG18 release notes: https://www.postgresql.org/docs/18/release-18.html (GA 2025-09-25)
- [S-PG19] PG19 release notes (beta): https://www.postgresql.org/docs/19/release-19.html
- [S-PG19B4] PostgreSQL 19 Beta 4 announcement: https://www.postgresql.org/about/news/postgresql-19-beta-4-released-3386/ (2026-09-24)
- [S-PG-PREDEF] Predefined roles (PG18): https://www.postgresql.org/docs/current/predefined-roles.html
- [S-PG-PRIV] Privileges: https://www.postgresql.org/docs/current/ddl-priv.html
- [S-PG-GRANT] GRANT: https://www.postgresql.org/docs/current/sql-grant.html
- [S-PG-ADP] ALTER DEFAULT PRIVILEGES: https://www.postgresql.org/docs/current/sql-alterdefaultprivileges.html
- [S-PG-ALTERROLE] ALTER ROLE: https://www.postgresql.org/docs/current/sql-alterrole.html
- [S-PG-CREATEROLE] CREATE ROLE: https://www.postgresql.org/docs/current/sql-createrole.html
- [S-PG-SETROLE] SET ROLE: https://www.postgresql.org/docs/current/sql-set-role.html
- [S-PG-CF] CREATE FUNCTION (SECURITY DEFINER safety): https://www.postgresql.org/docs/current/sql-createfunction.html
- [S-PG-OAUTH] OAuth authorization/authentication (PG18): https://www.postgresql.org/docs/18/auth-oauth.html
- [S-PG-OAUTHV] OAuth validator design (PG18): https://www.postgresql.org/docs/18/oauth-validator-design.html and https://www.postgresql.org/docs/18/oauth-validators.html
- [S-PG-LIBPQOAUTH] libpq OAuth support (PG18): https://www.postgresql.org/docs/18/libpq-oauth.html
- [S-PG-CERT] Certificate authentication: https://www.postgresql.org/docs/current/auth-cert.html
- [S-PG-RLS] Row security policies: https://www.postgresql.org/docs/current/ddl-rowsecurity.html
- [S-PG-VIEW] CREATE VIEW (`security_invoker`, `security_barrier`): https://www.postgresql.org/docs/current/sql-createview.html
- [S-PG-CLIENT] Client connection defaults (timeouts, `default_transaction_read_only`, `row_security`): https://www.postgresql.org/docs/current/runtime-config-client.html
- [S-PG-RES] Resource consumption (`temp_file_limit`, `work_mem`): https://www.postgresql.org/docs/current/runtime-config-resource.html
- [S-PG-RES18] `file_copy_method` (PG18): https://www.postgresql.org/docs/18/runtime-config-resource.html
- [S-PG-TX] SET TRANSACTION (READ ONLY semantics): https://www.postgresql.org/docs/current/sql-set-transaction.html
- [S-PG-VARC] `check_transaction_read_only` source (REL_18_STABLE): https://raw.githubusercontent.com/postgres/postgres/REL_18_STABLE/src/backend/commands/variable.c
- [S-PG-PROTO] Frontend/backend message flow (Parse is single-statement): https://www.postgresql.org/docs/current/protocol-flow.html
- [S-PG-PGSS] pg_stat_statements: https://www.postgresql.org/docs/current/pgstatstatements.html
- [S-PG-ETDEF] Event trigger definitions: https://www.postgresql.org/docs/current/event-trigger-definition.html
- [S-PG-ETM16] Event trigger firing matrix (PG16): https://www.postgresql.org/docs/16/event-trigger-matrix.html
- [S-PG-CET] CREATE EVENT TRIGGER: https://www.postgresql.org/docs/current/sql-createeventtrigger.html
- [S-PG-LD] Logical decoding concepts: https://www.postgresql.org/docs/current/logicaldecoding-explanation.html
- [S-PG-CREATEDB] CREATE DATABASE (PG18): https://www.postgresql.org/docs/18/sql-createdatabase.html
- [S-PG-UPG] pg_upgrade `--clone` / `--copy-file-range` (PG18): https://www.postgresql.org/docs/18/pgupgrade.html
- [S-PG-PITR] Continuous archiving and PITR: https://www.postgresql.org/docs/current/continuous-archiving.html
- [S-PG-ALTERTABLE] ALTER TABLE: https://www.postgresql.org/docs/current/sql-altertable.html
- [S-PG-CI] CREATE INDEX (CONCURRENTLY): https://www.postgresql.org/docs/current/sql-createindex.html
- [S-PG-TOAST] TOAST: https://www.postgresql.org/docs/current/storage-toast.html
- [S-CVE1] CVE-2024-10976: https://www.postgresql.org/support/security/CVE-2024-10976 (fixed 2024-11 minors)
- [S-CVE2] CVE-2025-8713: https://www.wiz.io/vulnerability-database/cve/cve-2025-8713 (disclosed 2025-08-14)

**Identity providers and credentials**
- [S-RDSIAM] RDS IAM database authentication: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html
- [S-CSQLIAM] Cloud SQL IAM authentication: https://docs.cloud.google.com/sql/docs/postgres/iam-authentication
- [S-CSQLIAMUSERS] Cloud SQL IAM users: https://docs.cloud.google.com/sql/docs/postgres/add-manage-iam-users
- [S-CSQLUSERS] Cloud SQL users / `cloudsqlsuperuser`: https://docs.cloud.google.com/sql/docs/postgres/users
- [S-ALLOYIAM] AlloyDB IAM authentication: https://docs.cloud.google.com/alloydb/docs/database-users/manage-iam-auth
- [S-AZENTRA] Azure Flexible Server Entra authentication: https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/how-to-configure-sign-in-azure-ad-authentication (updated 2026-09-06)
- [S-PERCONA-OIDC] Percona pg_oidc_validator: https://github.com/percona/pg_oidc_validator
- [S-CNPG-KC] CloudNativePG Keycloak OAuth validator: https://github.com/cloudnative-pg/postgres-keycloak-oauth-validator
- [S-VAULT] Vault PostgreSQL secrets engine: https://developer.hashicorp.com/vault/docs/secrets/databases/postgresql
- [S-SETUSER] set_user extension: https://github.com/pgaudit/set_user

**RLS, audit and attribution**
- [S-SUPARLS] Supabase RLS guide (performance, views, service keys): https://supabase.com/docs/guides/database/postgres/row-level-security
- [S-PGAUDIT] pgAudit: https://github.com/pgaudit/pgaudit
- [S-RDSPGA] RDS pgAudit: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.pgaudit.html
- [S-RDSPGA2] RDS pgAudit per-user/database settings: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Appendix.PostgreSQL.CommonDBATasks.pgaudit.exclude-user-db.html
- [S-RDSET] RDS event triggers: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.FeatureSupport.EventTriggers.html
- [S-SQLCOMM] sqlcommenter: https://google.github.io/sqlcommenter/
- [S-WAL2JSON] wal2json: https://github.com/eulerto/wal2json
- [S-SUPAAUDIT] supa_audit (archived 2025-02): https://github.com/supabase/supa_audit
- [S-PGMEMENTO] pgMemento: https://github.com/pgMemento/pgMemento
- [S-SAFEUPD] pg-safeupdate: https://github.com/eradman/pg-safeupdate
- [S-PGCG] pg_cgroups (archived 2024-07-12): https://github.com/cybertec-postgresql/pg_cgroups
- [S-EDBRM] EDB Resource Manager: https://www.enterprisedb.com/docs/epas/latest/database_administration/10_edb_resource_manager/

**Branching, cloning, backup and restore**
- [S-NEON1] Neon branching: https://neon.com/docs/introduction/branching
- [S-NEONAPI] Neon create branch API: https://api-docs.neon.tech/reference/createprojectbranch
- [S-NEONPRICE] Neon pricing: https://neon.com/pricing
- [S-NEONANON] Neon data anonymization (beta): https://neon.com/docs/workflows/data-anonymization
- [S-NEONPROT] Neon protected branches: https://neon.com/docs/guides/protected-branches
- [S-NEONRESTORE] Neon instant restore: https://neon.com/docs/introduction/branch-restore
- [S-NEONMCP] Neon MCP server: https://github.com/neondatabase/mcp-server-neon
- [S-DBX] Databricks to acquire Neon (2025-05-14): https://www.databricks.com/company/newsroom/press-releases/databricks-agrees-acquire-neon-help-developers-deliver-ai-systems
- [S-LAKE] Databricks Lakebase branches: https://docs.databricks.com/aws/en/oltp/projects/branches
- [S-AUR] Aurora cloning: https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Managing.Clone.html
- [S-CSQLCLONE] Cloud SQL clone (standard and fast clone): https://docs.cloud.google.com/sql/docs/postgres/clone-instance
- [S-ALLOYBKP] AlloyDB backup and recovery: https://docs.cloud.google.com/alloydb/docs/backup/overview
- [S-AZBKP] Azure Flexible Server backup and restore (updated 2026-07-16): https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-backup-restore
- [S-HORIZON] Azure HorizonDB overview (preview; updated 2026-09-22): https://learn.microsoft.com/en-us/azure/horizondb/overview
- [S-DBLAB] DBLab Engine: https://github.com/postgres-ai/database-lab-engine
- [S-XATA] Xata docs: https://xata.io/docs
- [S-XATAOSS] Xata open-source platform: https://github.com/xataio/xata
- [S-TIGERCLI] Tiger CLI: https://github.com/timescale/tiger-cli
- [S-SUPABR] Supabase branching: https://supabase.com/docs/guides/deployment/branching
- [S-PSBR] PlanetScale Postgres branching: https://planetscale.com/docs/postgres/branching
- [S-PRISMA] Prisma Postgres `npx create-db`: https://www.prisma.io/docs/postgres/introduction/npx-create-db
- [S-RDSPITR] RDS point-in-time restore: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIT.html
- [S-RDSDEL] RDS deleting a DB instance: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_DeleteInstance.html
- [S-CSQLDEL] Cloud SQL deletion protection: https://docs.cloud.google.com/sql/docs/postgres/deletion-protection
- [S-PGBR] pgBackRest: https://pgbackrest.org/
- [S-PGBR-REL] pgBackRest releases (v2.59.3, 2026-10-04): https://pgbackrest.org/release.html
- [S-BARMAN] Barman (3.20.1, 2026-09-29): https://pgbarman.org/
- [S-WALG] WAL-G: https://github.com/wal-g/wal-g

**Data protection**
- [S-ANON] PostgreSQL Anonymizer docs: https://postgresql-anonymizer.readthedocs.io/en/stable/
- [S-SEED] Snaplet Seed: https://github.com/supabase-community/seed
- [S-NEOSYNC] Neosync (archived 2025-08-30): https://github.com/nucleuscloud/neosync

**Extensions and managed availability**
- [S-RDSEXT] RDS for PostgreSQL extension versions: https://docs.aws.amazon.com/AmazonRDS/latest/PostgreSQLReleaseNotes/postgresql-extensions.html
- [S-CSQLEXT] Cloud SQL extensions: https://docs.cloud.google.com/sql/docs/postgres/extensions
- [S-AZEXT] Azure Flexible Server extensions (updated 2026-07-13): https://learn.microsoft.com/en-us/azure/postgresql/extensions/concepts-extensions-versions
- [S-AZLIMITS] Azure Flexible Server limits (updated 2026-07-10): https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-limits

**Schema change**
- [S-PGROLL] pgroll: https://github.com/xataio/pgroll
- [S-RESHAPE] Reshape: https://github.com/fabianlindfors/reshape
- [S-SQUAWK] squawk: https://github.com/sbdchd/squawk
- [S-PGQGO] pg_query_go changelog (6.2.5 on 2026-09-30, PG17 grammar): https://raw.githubusercontent.com/pganalyze/pg_query_go/main/CHANGELOG.md

**Connections**
- [S-PGBCL] PgBouncer changelog (1.26.0, 2026-09-23): https://www.pgbouncer.org/changelog.html
- [S-PGBFEAT] PgBouncer features (pooling-mode compatibility): https://www.pgbouncer.org/features.html
- [S-PGDOG] PgDog: https://github.com/pgdogdev/pgdog
- [S-SUPAVISOR] Supavisor: https://github.com/supabase/supavisor
- [S-PGCAT] pgcat: https://github.com/postgresml/pgcat
- [S-RDSPROXY] RDS Proxy pinning: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-proxy-pinning.html
- [S-CSQLMCP] Cloud SQL managed connection pooling: https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling
- [S-NEONSLS] Neon serverless driver: https://neon.com/docs/serverless/serverless-driver
- [S-HYPERDRIVE] Cloudflare Hyperdrive: https://developers.cloudflare.com/hyperdrive/

**Agent state and memory**
- [S-PGV] pgvector README: https://github.com/pgvector/pgvector
- [S-PGV-CL] pgvector changelog (0.8.7, 2026-10-01): https://raw.githubusercontent.com/pgvector/pgvector/master/CHANGELOG.md
- [S-PGV080] pgvector 0.8.0 announcement (2024-10-30): https://www.postgresql.org/about/news/pgvector-080-released-2952/
- [S-PGVS] pgvectorscale: https://github.com/timescale/pgvectorscale
- [S-VCHORD] VectorChord: https://github.com/tensorchord/VectorChord
- [S-PARADE] ParadeDB: https://github.com/paradedb/paradedb
- [S-PGTS] pg_textsearch: https://github.com/timescale/pg_textsearch
- [S-PGMQ] pgmq: https://github.com/pgmq/pgmq
- [S-PGCRON] pg_cron: https://github.com/citusdata/pg_cron
- [S-RECALL] Recall.ai, "Postgres LISTEN/NOTIFY does not scale" (incident March 2025): https://www.recall.ai/blog/postgres-listen-notify-does-not-scale

**MCP, registry, A2A, and MCP security incidents**
- [S-MCP1] MCP 2025-06-18 changelog: https://modelcontextprotocol.io/specification/2025-06-18/changelog
- [S-MCP2] MCP 2025-11-25 changelog: https://modelcontextprotocol.io/specification/2025-11-25/changelog
- [S-MCP3] MCP 2026-07-28 changelog: https://modelcontextprotocol.io/specification/2026-07-28/changelog
- [S-MCP4] MCP versioning and deprecated-features registry: https://modelcontextprotocol.io/specification/versioning and https://modelcontextprotocol.io/specification/2026-07-28/deprecated
- [S-MCP5] MCP 2026-07-28 authorization: https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization
- [S-MCP6] MCP 2026-07-28 tools: https://modelcontextprotocol.io/specification/2026-07-28/server/tools
- [S-MCP7] MCP 2026-07-28 elicitation: https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation
- [S-MCP8] MCP security best practices (2026-07-28): https://modelcontextprotocol.io/specification/2026-07-28/basic/security_best_practices
- [S-MCPEXTAUTH] MCP authorization extensions: https://github.com/modelcontextprotocol/ext-auth
- [S-MCPREG] MCP Registry (preview 2025-09-08; API freeze 2025-10-24): https://github.com/modelcontextprotocol/registry
- [S-A2A] A2A specification (latest 1.0.x): https://a2a-protocol.org/latest/specification/
- [S-A2AREL] A2A releases (v1.0.0 2026-03-12, v1.0.1 2026-05-28): https://github.com/a2aproject/A2A/releases
- [S-DD1] Datadog Security Labs, SQL injection in the Postgres MCP server (2025-08-21): https://securitylabs.datadoghq.com/articles/mcp-vulnerability-case-study-SQL-injection-in-the-postgresql-mcp-server/
- [S-ARCHIVED] Archived reference Postgres MCP server (archived 2025-05-29): https://github.com/modelcontextprotocol/servers-archived/tree/main/src/postgres
- [S-PGMCPPRO] Postgres MCP Pro: https://github.com/crystaldba/postgres-mcp
- [S-GA] General Analysis, Supabase MCP data exfiltration (2025-07-08): https://www.generalanalysis.com/blog/supabase-mcp-blog
- [S-SUPAMCP] Supabase MCP security guidance: https://supabase.com/docs/guides/getting-started/mcp and https://github.com/supabase-community/supabase-mcp
- [S-TOOLBOX] MCP Toolbox for Databases: https://github.com/googleapis/genai-toolbox
