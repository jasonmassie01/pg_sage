# Agent governance

Agent governance is how pg_sage lets AI agents (MCP clients, coding agents, LangGraph or CrewAI
workers) work on your databases without letting them widen their own reach. It has these
parts:
- **Principals**: one identity per agent, with an accountable person (its sponsor), a profile
  of allowed capability classes and an environment ceiling.
- **Tokens and OAuth**: how an agent proves on the MCP endpoint that it is that principal.
- **Grants and activity**: time-bounded read access an operator approves, and what the agent
  did.
- **Two-person rules**: a change that widens an agent's reach needs a second person.
- **Agent posture**: a catalog check of how exposed each database is to agents.
- **The kill switch**: stops agents at once, at any trust level.

Posture works on every database with no configuration. Everything else needs a control
database: `agents.control_database` names the monitored database that holds agent governance
state, and in `mode: meta` the meta database is the control database. The gate, the kill
switch and freezes, grants and grant requests, activity, agent roles and taint use it. One
principal covers every database of the fleet: a token or a grant names the databases it
reaches.

The principal and token routes, and MCP sign-in (tokens and OAuth identity bindings), use
pg_sage's own store instead: the database that holds its users and sessions. In `mode: meta`
that is the meta database, so the two are the same. Otherwise it is the single monitored
database, or the fleet's primary database, and you set `agents.control_database` to that same
database. The gate reads principals from the control database only, so it refuses an agent
that exists only in another database as one that does not exist.

Without a control database, the kill, freeze, unfreeze, grant and activity routes answer 503,
agent requests are capped at approval (L2) and agent changes fail closed. The principal and
token routes still answer, and posture checks still run.

## Principals

A principal is one agent identity. pg_sage gives it two roles on each cluster it reaches:
`sage_agent_<id10>` for the direct lane and `sage_agentb_<id10>` for the brokered lane
(`agent_query`). `<id10>` is 10 lower-case base32 characters derived from the principal's id.
The roles are created through the executor as operator-approved (L2) actions. They are never
superuser or `BYPASSRLS`, hold no dangerous membership and own nothing. pg_sage records them in
`sage.guard_cluster_roles` on the control database.

| Field | Meaning |
|---|---|
| `id` | `agp_` and 20 lower-case base32 characters, assigned at creation |
| `name` | a unique slug: `^[a-z][a-z0-9-]{1,62}$` |
| `sponsor_user_id` | the accountable person, a pg_sage user id; required at creation |
| `sponsor_active` | true while the sponsor is an existing, enabled operator or admin |
| `profile` | the capability profile (see [Profiles](#profiles)); required |
| `env_ceiling` | `branch`, `dev`, `stage` or `prod` (default `dev`): the most production-like environment the agent may reach |
| `tenant` | optional free text, up to 200 characters |
| `status` | `active`, `frozen` or `retired` |
| `frozen_reason` | why it is frozen, while it is |
| `tainted` | true while an uncleared taint row exists (see [Taint](#taint)) |
| `created_by`, `created_at`, `updated_at` | who created it, and when it changed |

| Request | Who | What it does |
|---|---|---|
| `GET /api/v1/agents` `?status=&limit=&cursor=` | operator | Lists principals, 1 to 200 a page |
| `POST /api/v1/agents` `{"name", "sponsor_user_id", "profile", "env_ceiling", "tenant"}` | admin | Creates one (201) |
| `GET /api/v1/agents/{id}` | operator | Reads one, with `sponsor_active` and `tainted` |
| `PATCH /api/v1/agents/{id}` `{"sponsor_user_id", "profile", "env_ceiling", "status", "reason"}` | admin | Changes the sponsor, profile or ceiling, or retires it (`"status": "retired"`) |

Unknown body fields are refused. A missing or bad field answers 422, an unknown agent 404,
and a taken name or a change to a retired agent 409. A sponsor change while the agent has an
open freeze answers 409 with `code: sponsor_change_frozen`. `status` accepts only `retired`: freeze
and unfreeze have their own routes (see [the kill switch](#kill-switch-freeze-and-unfreeze)).

**Statuses.**
- `active`: the profile, the ceiling and the trust level decide what it may do.
- `frozen`: set by a freeze or a kill. Read tools still work; propose tools and `agent_*`
  tools answer `agent_frozen`, and it gets no new token.
- `retired`: final. Its tokens and OAuth identities stop authenticating at once. The leader
  sidecar drops its roles on every cluster after `agents.roles.retire_grace_days` (7), under
  the approval of the admin who retired it.

**Sponsor rules.**
- Every agent is created with a sponsor: 422 without one, or with a user that doesn't exist.
- The sponsor is an enabled operator or admin. Naming a viewer or a deactivated user, on
  create or on `PATCH`, answers 422.
- Deleting the sponsor's user clears the sponsor. A sponsor demoted to viewer or deactivated
  stays on record but no longer counts: `sponsor_active` reads false. Either way the agent is
  unsponsored: it keeps its read tools, its propose tools queue for a person (L2), and its
  `agent_*` tools answer `agent_unsponsored` until an admin assigns a sponsor with `PATCH`.
- Changing the sponsor doesn't widen anything, so one admin does it.
- The sponsor can't change while the agent has an open freeze (409 `sponsor_change_frozen`):
  unfreeze it first. The unfreeze after a kill refuses the sponsor as the second admin, so
  handing the sponsorship to someone else first would get around that rule.
- The sponsor is never the second person on their own agent's two-person decisions.

**Audit.** Every governance change writes one row to `sage.auth_audit`, the hash-chained
table that also records sign-ins and user administration, in the same transaction as the
change. If the audit row can't be written, the change isn't made and the request answers 500.
`actor_user_id` is the admin, `source_ip` the client address, and `target_user_id` the agent's
sponsor (or an operator token's owner).

| Event | Written by | `detail` |
|---|---|---|
| `agent_created` | `POST /api/v1/agents` | `principal_id`, `name`, `tenant`, `after` |
| `agent_updated` | `PATCH /api/v1/agents/{id}` | `principal_id`, `name`, `before`, `after`, `reason` |
| `agent_widened_single_operator` | a widening `PATCH` in single-operator mode | as `agent_updated`; `reason` is required |
| `agent_retired` | `PATCH /api/v1/agents/{id}` `{"status": "retired"}` | `principal_id`, `name`, `before_status`, `after_status`, `reason` |
| `agent_status_changed` | a freeze or unfreeze made through the principal store (tools and tests) | as `agent_retired` |
| `agent_token_minted` | `POST /api/v1/agents/{id}/tokens` | `token_id`, `name`, `kind`, `scopes`, `databases`, `expires_at`, `principal_id` |
| `mcp_token_created` | `POST /api/v1/mcp/tokens` | as `agent_token_minted`, with `owner_user_id` |
| `mcp_token_revoked` | `DELETE /api/v1/mcp/tokens/{id}` (not a repeat on a revoked token) | as `mcp_token_created` |

`before` and `after` are `{"sponsor_user_id", "profile", "env_ceiling"}`. No row carries a token
secret, its hash or its prefix. An empty `PATCH` changes nothing and writes nothing. Kills and
unfreezes are recorded in the action log (see [the kill switch](#kill-switch-freeze-and-unfreeze)).

**Levels.** Read tools work up to L3. Propose tools are capped at L2: a person approves each
proposal. `agent_*` tools reach L3 at most, and L2 while the agent is tainted. The trust level
and the gate can only lower these.

**Environment ceiling.** The effective ceiling is the narrower of the agent's `env_ceiling`
and its profile's ceiling. A database's environment comes from its verified label (see
[Environments](#environments)), never from the agent, and an unverified database counts as
`prod`. A request to a database above the effective ceiling is refused with
`agent_env_ceiling`.

### Profiles

pg_sage ships these profiles, fixed in this release. A profile name outside this table is
accepted, but it allows no class, so every `agent_*` request under it is refused.

| Profile | Classes | Ceiling |
|---|---|---|
| `readonly-analyst` | `read` | `prod` |
| `app-writer` | `read`, `write_insert`, `write_update` | `prod` |
| `coding-agent` | `read`, `write_insert`, `write_update`, `write_delete`, `ddl_additive`, `ddl_locking`, `sandbox` | `stage` |
| `legacy` | `read` | `prod` |

`legacy` is the profile of the principals created for migrated tokens (see
[Tokens](#tokens)).

### Taint

A tainted agent has read something that may carry instructions it should not follow. While an
uncleared row for it exists in `sage.guard_taint` (control database), its `agent_*` tools are
capped at L2, so a person approves each change. A row names the whole principal (task `*`) or
one task: the task an OAuth token's task claim names. Taint has no time expiry, and retention
never removes the rows. In this release pg_sage has no route that taints an agent or clears
its taint; an admin clears the rows by hand:

```sql
UPDATE sage.guard_taint SET cleared_at = now(), cleared_by = 'you@example.com'
WHERE principal_id = 'agp_...' AND cleared_at IS NULL;
```

## Tokens

An agent authenticates on the MCP endpoint with a pg_sage MCP token, or with an OAuth access
token (see [OAuth](#oauth)). A bearer token authenticates the MCP endpoint only, never the
rest of the API. [MCP tokens](mcp.md#tokens) has the fields and curl examples.

| | Agent token | Operator token |
|---|---|---|
| For | an agent principal | a person: an operator or admin user |
| Minted at | `POST /api/v1/agents/{id}/tokens` (admin; the **Agents** page) | `POST /api/v1/mcp/tokens` (admin; the **MCP tokens** page), which refuses `kind: agent` |
| Scopes | `read`, `propose`; never `approve` | `read`, `propose`, `approve` |
| Follows | its agent's status | its owner's role |

- `POST /api/v1/agents/{id}/tokens` takes `{"name", "scopes", "databases", "expires_in_days"}`.
  A frozen or retired agent gets no token.
- Every token lives 1 to 90 days (`expires_in_days`). Only its SHA-256 is stored; the
  plaintext is in the 201 response, once.
- `GET /api/v1/mcp/tokens` (admin) lists both kinds. An agent token's `principal_status` is
  its agent's status.
- `DELETE /api/v1/mcp/tokens/{id}` (admin) revokes a token of either kind.
- A freeze keeps the agent's tokens, which answer `agent_frozen` for anything but reads.
- A kill revokes them, and they stay revoked after an unfreeze: mint new ones.
- Retiring the agent stops its tokens.

**Legacy tokens.** At startup pg_sage binds every live agent token without a principal (one
minted before principals existed) to a new principal. Its name is a slug of the token's name,
its profile `legacy`, its `created_by` `migration:g1`, and it has no sponsor. Its read tools
keep working, its propose tools queue for a person, and its `agent_*` tools answer
`agent_unsponsored` until an admin assigns a sponsor. The migration is idempotent and safe
with several sidecars. Tokens of the removed agent provisioner are only counted, because
nothing accepts them any more; they go with the legacy tables. See
[Decommissioning the removed database provisioner](../sidecar/internal/decommission/README.md).

## OAuth

With `mcp.oauth` on, the MCP endpoint is an OAuth 2.1 resource server: it accepts access
tokens (JWTs) from the issuers you list, beside pg_sage's own MCP tokens.

```yaml
mcp:
  transport: http                # required for OAuth
  oauth:
    enabled: true
    resource: https://sage.example.com/api/v1/mcp   # the MCP endpoint's canonical URL
    issuers:
      - issuer: https://idp.example.com
        jwks_uri: ""             # empty: discovered from the issuer's metadata
        signing_algs: []         # default: RS*, PS*, ES* and EdDSA; never HS* or none
    task_claim: task_id          # empty ignores tasks
```

All four keys are safety-critical and take effect on restart. Issuer and JWKS URLs must be
https (http only on a loopback host).

- **Issuers.** A token must name a listed issuer, be signed with that issuer's key and be
  unexpired.
- **Audience.** Every monitored database is its own protected resource,
  `<resource>/databases/<name>`, served at that path. A token whose audience names one
  database's resource is refused at another's. At the server-wide resource (`resource`
  itself) the audience must name it, and the token reaches only the databases whose resources
  its audience also names. pg_sage publishes each resource's RFC 9728 metadata under
  `/.well-known/oauth-protected-resource`.
- **Scopes.** `read` is required for anything, `propose` adds the propose tools, and an OAuth
  token never gets `approve`.
- **Delegation.** Under RFC 8693 the token's `sub` is whom it acts for and `act.sub` is the
  actor, the agent. pg_sage binds the actor and records `sub` as on-behalf-of. Nested `act`
  claims are followed up to 8 deep; an actor from another issuer is refused.
- **Task claim.** `task_claim` names the claim that carries the runtime's task id. A missing
  claim is fine. A malformed one (not a printable string of at most 200 bytes) refuses the
  token, because the task is what per-task taint is keyed by.
- **Identity bindings.** An identity maps to a principal through
  `sage.guard_identity_bindings` in pg_sage's store: one row per `(issuer, subject)`,
  where the subject is the actor. There is no route for bindings yet; an admin inserts them:

  ```sql
  INSERT INTO sage.guard_identity_bindings (issuer, subject, principal_id, created_by)
  VALUES ('https://idp.example.com', 'agent-client-id', 'agp_...', 'you@example.com');
  ```

  An identity with no binding, or bound to a retired agent, is refused (403).

A bad token, audience or issuer answers 401 `invalid_token`, a missing scope 403
`insufficient_scope`, and an issuer or binding lookup that can't be reached 503. pg_sage can't
revoke an access token your issuer signed: a kill freezes the agent, which limits its OAuth
tokens to read tools. Retire the agent, or delete its binding row, to refuse them outright.

## Grants and activity

An agent reads data it isn't otherwise granted through time-bounded grants. In this release
every grant is approved by an operator (L2).

| Request | Who | What it does |
|---|---|---|
| `GET /api/v1/agents/{id}/grants` `?database&state&limit&cursor` | operator | Lists the agent's grants in one database |
| `POST /api/v1/agents/{id}/grants` `{"database", "capability", "objects": [{"object", "columns"}], "duration_minutes", "reason"}` | operator | Grants now: the operator's request is the approval (201) |
| `POST /api/v1/agents/{id}/grants/{grant}/revoke` `{"database"}` | operator | Revokes one grant |
| `GET /api/v1/agents/{id}/grant-requests` `?database&status&limit&cursor` | operator | Lists the agent's own requests |
| `POST /api/v1/agents/{id}/grant-requests/{request}/approve` `{"database"}` | operator | Runs a pending request as the grant, once |
| `POST /api/v1/agents/{id}/grant-requests/{request}/deny` `{"database"}` | operator | Closes a pending request |
| `GET /api/v1/agents/{id}/activity` `?database&from&to&limit&cursor` | operator | The agent's activity |

The agent asks with the MCP tool `agent_request_capability` (scope `propose`). Agent
governance checks the request first: status, sponsor, ceiling, profile, the database's column
classifications and rate limits. A request it allows waits 15 minutes for an operator. The
grant lists only the columns the database's environment allows. Every grant expires, after at
most `agents.capabilities.max_duration_minutes` (240), and the leader's reconciler revokes it
(every `agents.reconcile_interval_seconds`, 60). A refusal answers 409
`{"verdict": "blocked", "reason_code", "error", "fix"}`.

Activity shows, per database, the agent's live sessions, the statements `pg_stat_statements`
attributes to its roles, its `agent_query` audit rows and whether attribution is complete.
When attribution may have dropped statements, the audit rows are the record. `from` and `to`
default to the last 24 hours; `?database` narrows to one database and pages its audit rows
with `cursor`.

## Environments

Each database has a verified environment label: `branch`, `dev`, `stage` or `prod`. A label
is bound to the database's physical identity when it is set, and every evaluation re-reads
that identity. A changed identity, one identity seen under two labels, a branch without an
active clone receipt, or an identity too weak to verify all count as `prod`.

| Request | Who | What it does |
|---|---|---|
| `GET /api/v1/agent-environments/{database}` | operator | The label, its identity snapshot and what it evaluates to now |
| `PUT /api/v1/agent-environments/{database}` `{"label": "dev"}` | admin | Sets the label. Narrowing applies at once; widening answers 202 until a second admin repeats it |

A label the live identity can't verify is refused with 409 and the reason. Only a provider or
cluster identity verifies a label other than `prod`.

## Two-person rules

A change that widens what an agent may do needs a second person. Where the table allows it,
`agents.single_operator_mode: true` lets one admin apply the change instead, but only with a
reason, which pg_sage records for a review after the fact (a principal's widening is the
`agent_widened_single_operator` event in `sage.auth_audit`).

| Change | Request | Second person | Single-operator mode |
|---|---|---|---|
| Widen a principal: a higher `env_ceiling`, or any profile change | `PATCH /api/v1/agents/{id}` | Not available in this release: refused with 403 `two_person_required` | one admin, with a reason |
| Widen a database's environment label (toward `branch`) | `PUT /api/v1/agent-environments/{database}` | another admin repeats the same label within 24 hours | does not apply |
| Unfreeze after a kill | `POST /api/v1/agents/{id}/unfreeze` | another admin within 15 minutes, not the agent's sponsor | one admin, with a reason |
| Unfreeze after an operator freeze | `POST /api/v1/agents/{id}/unfreeze` | none: one admin | does not apply |
| Release a fleet or database kill | `POST /api/v1/agents/kill/release` | another admin within 15 minutes | one admin, with a reason |
| Ratify an agent's policy proposal that widens the policy | `POST /api/v1/policy/proposals/{id}/ratify` | a second admin; neither may be the agent's sponsor | one admin, with a reason |

Every request in the table is an admin's. The first admin's request waits as pending (202 for
an unfreeze, a release or a label), and the same admin repeating it doesn't count. Narrowing
changes (a lower ceiling, a narrower label, a freeze, a kill) need one person and apply at
once.

## Agent posture

pg_sage's agent posture check reports how exposed a PostgreSQL database is to the AI agents
and untrusted clients that connect to it. This covers MCP servers, coding agents, LangGraph
or CrewAI workers, and PostgREST/Supabase anonymous roles. It reads only the system catalog
and flags the grants, roles, policies and functions that let an agent do more than its job:
- an agent role with `BYPASSRLS`, or one that owns tables;
- a table the anonymous role can read with row-level security off;
- a definer function anyone can call and hijack through `search_path`.

Posture is on by default and needs no configuration. It works on any PostgreSQL 14–18,
self-managed or managed, and needs only pg_sage's usual `pg_monitor` role. Every fix it
proposes is a SQL script for you to review and run. pg_sage never runs a posture fix
itself: posture findings stop at autonomy level L1.

## Where you see it

- **First look.** The catalog-only first look of every new database has an **Agent posture**
  section: one check per detector, its findings with catalog evidence, and the fix script.
  It runs in the first look's own read-only transaction, under the same 5 s per-statement
  budget, and gets one retry for a check that a transient error (a timeout, a lock) degraded.
- **Findings.** The analyzer runs posture again:
  - on its first cycle;
  - whenever the catalog rows posture reads change;
  - once a day at `agents.posture.daily_at`.

  The catalog check uses pg_sage's own hash of roles, memberships, per-role settings, ACLs,
  ownership, row-level security, policies, view rules and extension versions. Data changes,
  `ANALYZE` and `VACUUM` don't trigger a run.
  - Each problem is a finding in `sage.findings` with the category `agent_posture:AP-NN`.
    It resolves on its own once a completed check no longer sees it.
  - The fix script is stored as `detail.manual_script`, with `detail.proposal_level = L1`.
  - The finding has no `recommended_sql`, so the executor never acts on it.

Every posture statement carries the `/* pg_sage first_look */ /* agent_posture AP-NN */` tag,
so its cost shows in `pg_stat_statements` and in pg_sage's performance gate.

## Which roles count

**Exposed roles** are the roles untrusted clients reach:
- `PUBLIC`, always;
- every role listed in `agents.exposed_roles`;
- `anon` and `authenticated`, added automatically when both exist (Supabase).

A role is listed under `missing` when it's configured but doesn't exist. Privileges are
read with `aclexplode` on the object's ACL, or its default ACL. A grant counts only when
the exposed role (or `PUBLIC`) also has `USAGE` on the object's schema. Privileges an
exposed role inherits through membership in another role are not followed.

**Agent roles** come in two kinds:
- **Registered** agent roles follow pg_sage's agent role naming: `sage_agent_` or
  `sage_agentb_` plus 10 lower-case base32 characters. pg_sage records the agent roles it
  creates in `sage.guard_cluster_roles` on the control database, and posture matches the
  role names those records use.
- **Client hints**: a role with a session whose `application_name` matches one of
  `agents.client_patterns`. Matching is anchored and case-insensitive, and the default
  patterns are `^mcp`, `^claude`, `^cursor`, `^codex`, `^langgraph` and `^crewai`. A hint
  is a guess, so the same problem on a hinted role is reported one severity lower.

pg_sage's own role is never counted as an agent.

## Detectors

| Id | Reports | Severity | Fix script |
|---|---|---|---|
| AP-01 | An agent role that is superuser or holds `BYPASSRLS`, `CREATEROLE`, `CREATEDB` or `REPLICATION`, or is a member (directly or not) of `pg_execute_server_program`, `pg_write_server_files`, `pg_read_server_files` or `pg_write_all_data` | critical (registered), warning (hint) | `ALTER ROLE … NO…`; `REVOKE <group> FROM …` for direct memberships |
| AP-02 | Objects owned by an agent role, in this database or the cluster's shared catalogs (from `pg_shdepend`), with up to five named | critical (registered), warning (hint) | `REASSIGN OWNED BY … TO app_owner` |
| AP-03 | A table with row-level security off that grants `SELECT`, `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE` to an exposed role or `PUBLIC`. Also a view granted to one that runs as its owner over such a table | critical | `ENABLE ROW LEVEL SECURITY` (write the policies first), or `REVOKE ALL … FROM …` |
| AP-04 | A permissive policy for an exposed role or `PUBLIC` whose `USING` or `WITH CHECK` is plain `true` | warning | `ALTER POLICY … USING (<predicate>)`, or `DROP POLICY` |
| AP-05 | A `SECURITY DEFINER` function or procedure that an exposed role (or `PUBLIC`, the default) can execute, with no `search_path` in its settings | warning | `ALTER FUNCTION … SET search_path = pg_catalog, pg_temp`; optionally `REVOKE EXECUTE` |
| AP-06 | A view in an exposed schema (one where an exposed role has `USAGE`) over a table with row-level security, without `security_invoker`. On PostgreSQL 14 it's reported as "no security_invoker available" | warning | PostgreSQL 15+: `ALTER VIEW … SET (security_invoker = true)`. PostgreSQL 14: revoke the view from exposed roles |
| AP-07 | A schema where `PUBLIC` holds `CREATE` (PostgreSQL 14's `public` schema by default) | warning | `REVOKE CREATE ON SCHEMA … FROM PUBLIC` |
| AP-08 | A non-superuser login role without a non-zero `statement_timeout` or `idle_in_transaction_session_timeout`. Settings count when made for the role (in this database or all), for the database, or in the server configuration where pg_sage's session can see it | info | `ALTER ROLE … SET statement_timeout = '30s'` and `idle_in_transaction_session_timeout = '60s'` |
| AP-09 | Remote access or host code an exposed role (or `PUBLIC`) can reach: `dblink` functions it can execute, the `postgres_fdw`/`dblink_fdw` wrappers and their servers it can use, a user mapping (stored credentials) for it on such a server, and an untrusted language (`plperlu`, `plpython3u`, `pltclu`, …) marked trusted | critical | `REVOKE EXECUTE ON FUNCTION … FROM …`, `REVOKE USAGE ON FOREIGN DATA WRAPPER/SERVER/LANGUAGE …`, `DROP USER MAPPING FOR … SERVER …` |
| AP-10 | pgvector below 0.8.4 while HNSW indexes exist (vacuum fixes), or below 0.8.7 while IVFFlat indexes exist (index build overflow); the server's major at, past or within 90 days of its end of life | critical (pgvector), warning (end of life) | `ALTER EXTENSION vector UPDATE` after installing the fixed release; the upgrade plan |
| AP-11 | No point-in-time recovery: on a self-managed server `archive_mode = off`, or on with nothing archiving; on RDS, Aurora and Cloud SQL, automated backups or PITR off or retention 0, as cloud telemetry last read the instance. Deletion protection off (RDS, Cloud SQL). No pgaudit (neither preloaded nor installed) while agent roles use the database | warning | `ALTER SYSTEM SET archive_mode …`; the provider's CLI command; `CREATE EXTENSION pgaudit` after preloading it |
| AP-12 | A schema whose LangGraph checkpoint tables (`checkpoints`, `checkpoint_blobs`, `checkpoint_writes`) grew more than `agents.posture.memory_growth_gb_day` GB (GiB) a day with no rows deleted | info | A query for the largest threads, to delete past your retention |
| AP-13 | A login role used at once by 3 or more distinct clients (`application_name` and client address), at least one of them agent-like | info; warning once any registered agent role exists | `CREATE ROLE <login>_agent LOGIN NOINHERIT`, then move the agent to it |
| AP-14 | pg_sage's own role is a superuser, holds `BYPASSRLS`, or has the privileges of an agent role | warning | `ALTER ROLE … NOBYPASSRLS`; `REVOKE <agent> FROM …` (PostgreSQL 14/15) or `REVOKE INHERIT OPTION FOR <agent> FROM …` (16+); a dedicated non-superuser role |
| AP-15 | The `PUBLIC` baseline: tables, views and foreign tables granted to `PUBLIC` (one finding per schema), and default privileges that grant `PUBLIC` on new tables or sequences. Extension-owned objects are skipped | warning | `REVOKE ALL ON TABLE … FROM PUBLIC`; `ALTER DEFAULT PRIVILEGES FOR ROLE … REVOKE ALL ON TABLES FROM PUBLIC` |
| AP-16 | Once any registered agent role exists, a login role seen with an agent-like `application_name` that isn't one (an agent outside agent governance) | warning | `ALTER ROLE … NOLOGIN` after the agent moves to its own credentials |

AP-03 and AP-06 skip tables and views an extension owns (pg_hint_plan's `hint_plan.hints`,
for example): those grants come from the extension, not from you.
AP-08 skips pg_sage's own role, reserved `pg_` roles and managed-service admin logins
(`rdsadmin`, `cloudsqladmin`, `azure_superuser`, `alloydbadmin` and similar).

Notes on AP-09 to AP-16:
- **AP-10.** The pgvector fix releases come from pg_sage's research of pgvector's changelog;
  no CVE id is cited because none is verified. End-of-life dates are from the PostgreSQL
  versioning policy (PostgreSQL 14: 2026-11-12). A major older than 13 counts as past end of
  life; a major newer than 18 isn't reported until its date is known.
- **AP-11.** The provider check uses what cloud telemetry already reads with the instance
  (no extra API call); without telemetry nothing is reported, never "off". pg_sage can't see
  whether backups are immutable or kept after the instance is deleted, so check your
  provider's backup vault or retained-backup settings yourself. A tool that streams WAL
  (`pg_receivewal`, Barman streaming) can give PITR with `archive_mode` off; the caveat says so.
  On PostgreSQL 14 the `archive_library` arm is skipped (the setting is 15+).
- **AP-12** needs two observations at least an hour apart, kept in memory by the analyzer's
  posture run: the first run records, a later run (usually the next daily one) compares.
  The first look has none and reports nothing. After a restart, the next finding waits for
  two new observations, so it can come a day later.
- **AP-13** needs `pg_read_all_stats` (part of `pg_monitor`) to see other roles' client
  addresses. Without it, clients are counted by `application_name` only, and the finding's
  caveat says so.
- **AP-14.** pg_sage also warns once at startup when it connects as a superuser: agent governance
  features need a non-superuser role. Posture still runs. Inheritance is read with
  `pg_has_role` on every version; the arms decide the fix (PostgreSQL 14/15 inherit per role,
  16+ per grant).

**Version arms.** A detector part that applies only to some PostgreSQL versions is an *arm*.
On other versions the arm is skipped, and the check's note records why. For example, on
PostgreSQL 14, AP-06's note reads "arm security_invoker skipped: security_invoker views need
PostgreSQL 15+".

## Configuration

```yaml
agents:
  exposed_roles: []        # roles untrusted clients reach; PUBLIC always counts,
                           # anon + authenticated are added when both exist
  client_patterns: ["^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"]
                           # anchored, case-insensitive; [] disables hints
  posture:
    memory_growth_gb_day: 5  # agent memory-store growth reported (AP-12)
    daily_at: "03:00"        # local HH:MM of the daily posture run
```

| Key | Class | Notes |
|---|---|---|
| `agents.exposed_roles` | safety_critical | `PUBLIC` is implicit and can't be listed |
| `agents.client_patterns` | operator_preference | Each pattern must start with `^` |
| `agents.posture.memory_growth_gb_day` | operator_preference | Greater than 0 |
| `agents.posture.daily_at` | operator_preference | `H:MM` or `HH:MM`, 24-hour |

The keys take effect on restart. The analyzer's posture run reads them each time it runs.

## Kill switch, freeze and unfreeze

The kill switch stops agents at once. It needs no approval: it works under the emergency stop
and at every `trust.level`, because it only takes access away.

| Request | Who | What it does |
|---|---|---|
| `POST /api/v1/agents/kill` `{"scope": "all", "reason": "..."}` | admin | Every agent, every database and configured replica; sets the fleet freeze |
| `POST /api/v1/agents/kill` `{"scope": "principal", "id": "agp_...", "reason": "..."}` | admin | One agent, on every cluster where it has roles |
| `POST /api/v1/agents/kill` `{"scope": "database", "id": "orders", "reason": "..."}` | admin | Every agent session in one database; sets that database's freeze |
| `POST /api/v1/agents/{id}/freeze` `{"reason": "..."}` | operator | Like a one-agent kill, but the agent's MCP tokens stay (they get `agent_frozen`) |
| `POST /api/v1/agents/{id}/unfreeze` `{"reason": "..."}` | admin | Restores the roles' previous attributes and rotates the broker password |
| `POST /api/v1/agents/kill/release` `{"scope": "all"}` or `{"scope": "database", "database": "orders"}` | admin | Lifts a fleet or database freeze |

A kill, step by step:

1. The agents are marked frozen. The kill waits up to 2 s for an agent change that is
   committing, then cancels it.
2. Their MCP tokens are revoked.
3. Their pending approvals become `cancelled_kill`.
4. Their roles get `NOLOGIN CONNECTION LIMIT 0`. pg_sage records the previous attributes.
5. Their sessions end on the primary and on each replica listed under `databases[].replicas`.
   Sessions of look-alike roles (`sage_agent_...` names that are not agent roles) end too, and
   the report names them.
6. pg_sage checks for up to `agents.kill_verify_timeout_seconds` (10) that no agent session is
   left. The report says, per database and replica, whether that held.

Standbys you did not list still stop new agent logins, because `NOLOGIN` replicates. Open
sessions there end within the agent roles' `statement_timeout`, `idle_session_timeout` and
(PostgreSQL 17+) `transaction_timeout`. The report names each such standby with that bound. Set
each standby's `cluster_name` to its `replicas[].name`, so a listed replica is not also
reported as unlisted.

```yaml
databases:
  - name: orders
    host: db1
    replicas:
      - name: orders-replica1         # = the standby's cluster_name
        dsn_env: ORDERS_REPLICA1_DSN  # the DSN is read from this variable
agents:
  kill_verify_timeout_seconds: 10
  kill_fallback_log: agent-kill-fallback.log
```

pg_sage's role needs `pg_signal_backend` to end agent sessions (it holds `SET`, not `INHERIT`,
on agent roles), and `pg_monitor` to name standbys. If the control database or the gate is
unreachable, the kill still runs against each database. It records each step in
`agents.kill_fallback_log`, and copies those entries into the action log when the control
database is back.

Lifting a freeze widens access, so it goes through the gate like any change. The emergency stop
and `trust.level: observation` hold it back. After a kill it needs two admins: the first
request waits up to 15 minutes for a second admin who is not the agent's sponsor. Lifting an
operator freeze needs one admin. With `agents.single_operator_mode`, one admin can lift a kill
freeze, but must give a reason. Every unfreeze rotates the broker password, so the old one
fails. Tokens revoked by a kill stay revoked: mint new ones.

## Manual runbook

These are the SQL statements you run yourself when pg_sage is down or you want to check by
hand. Run them as a role that can read the catalog (`pg_monitor` is enough to read). The
containment steps need `CREATEROLE` or superuser.

**Contain registered agent roles** (stop new logins, end live sessions):

```sql
DO $$ DECLARE r record; BEGIN
  FOR r IN SELECT rolname FROM pg_roles WHERE rolname ~ '^sage_agentb?_[a-z2-7]{10}$' LOOP
    EXECUTE format('ALTER ROLE %I NOLOGIN CONNECTION LIMIT 0', r.rolname);
  END LOOP; END $$;
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE usename ~ '^sage_agentb?_[a-z2-7]{10}$';
```

For an agent that logs in with a role of your own, run the same two statements for that role
name. Do this on the primary and every replica the agent can reach.

**Find agent-like sessions** (the AP-01 hint):

```sql
SELECT DISTINCT usename, application_name, client_addr
FROM pg_stat_activity
WHERE backend_type = 'client backend'
  AND application_name ~* '^(mcp|claude|cursor|codex|langgraph|crewai)';
```

**Dangerous agent role attributes** (AP-01):

```sql
SELECT rolname, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb, rolreplication
FROM pg_roles WHERE rolname = 'my_agent_role';
SELECT g.rolname FROM pg_roles g
WHERE g.rolname IN ('pg_execute_server_program', 'pg_write_server_files',
                    'pg_read_server_files', 'pg_write_all_data')
  AND pg_has_role('my_agent_role', g.oid, 'MEMBER');
```

**Tables exposed without row-level security** (AP-03; replace `anon` with your exposed role):

```sql
SELECT n.nspname, c.relname, a.privilege_type
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
WHERE c.relkind IN ('r', 'p') AND NOT c.relrowsecurity
  AND a.grantee IN (0, 'anon'::regrole)
  AND n.nspname NOT IN ('pg_catalog', 'information_schema');
```

**Definer functions without a pinned search_path** (AP-05):

```sql
SELECT p.oid::regprocedure FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prosecdef AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) s WHERE s LIKE 'search_path=%');
```

**Schemas where anyone can create** (AP-07):

```sql
SELECT n.nspname FROM pg_namespace n
CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a
WHERE a.grantee = 0 AND a.privilege_type = 'CREATE';
```

**dblink callable by anyone** (AP-09):

```sql
SELECT p.oid::regprocedure FROM pg_proc p
JOIN pg_depend d ON d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'dblink'
WHERE has_function_privilege('public', p.oid, 'EXECUTE');
```

**WAL archiving and pgaudit** (AP-11):

```sql
SELECT name, setting FROM pg_settings
WHERE name IN ('archive_mode', 'archive_command', 'archive_library', 'shared_preload_libraries');
```

**Relations granted to PUBLIC** (AP-15):

```sql
SELECT n.nspname, c.relname, a.privilege_type
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) a
WHERE a.grantee = 0 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema');
```

Review each fix script before you run it. Enabling row-level security without a policy hides
every row from roles other than the owner. Pinning a function's `search_path` requires its
body to schema-qualify what it uses. A role timeout must fit the role's longest legitimate
statement.
