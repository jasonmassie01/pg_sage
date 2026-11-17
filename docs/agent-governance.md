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

Brokered sessions set `application_name = 'pg_sage agent:<id>'`, the principal's id and not its
name. pgaudit attribution counts that claim only from one of the two roles the id derives, so
one agent cannot sign its statements as another; see
[Attribution](security.md#attribution-trusts-the-role-not-application_name). An agent that
connects on its direct lane can name itself the same way: `pg_sage agent:<id>[:<action>]`.

| Field | Meaning |
|---|---|
| `id` | `agp_` and 20 lower-case base32 characters, assigned at creation |
| `name` | a unique slug: `^[a-z][a-z0-9-]{1,62}$` |
| `sponsor_user_id` | the accountable person, a pg_sage user id; required at creation |
| `sponsor_active` | true while the sponsor is an existing, enabled operator or admin |
| `profile` | the capability profile, one of those in [Profiles](#profiles); required |
| `env_ceiling` | `branch`, `dev`, `stage` or `prod` (default `dev`): the most production-like environment the agent may reach |
| `tenant` | optional free text, up to 200 characters |
| `status` | `active`, `frozen` or `retired` |
| `frozen_reason` | why it is frozen, while it is |
| `tainted` | true while an uncleared taint row exists (see [Taint](#taint)) |
| `created_by`, `created_at`, `updated_at` | who created it, and when it changed |

| Request | Who | What it does |
|---|---|---|
| `GET /api/v1/agents` `?status=&limit=&cursor=` | operator | Lists principals, 1 to 200 a page, with `profiles`: the profiles pg_sage ships (`name`, `classes`, `env_ceiling`, `direct_lane`), and `retire_grace_days` |
| `POST /api/v1/agents` `{"name", "sponsor_user_id", "profile", "env_ceiling", "tenant"}` | admin | Creates one (201) |
| `GET /api/v1/agents/{id}` | operator | Reads one, with `sponsor_active` and `tainted` |
| `PATCH /api/v1/agents/{id}` `{"sponsor_user_id", "profile", "env_ceiling", "status", "reason"}` | admin | Changes the sponsor, profile or ceiling, or retires it (`"status": "retired"`). A widening change answers 202 and waits for a second admin (see [Two-person rules](#two-person-rules)) |
| `GET /api/v1/agents/{id}/changes` | operator | The agent's pending widening change: `change_id`, `patch`, `requested_by`, `reason`, `expires_at` |
| `POST /api/v1/agents/{id}/changes/{cid}/confirm` `{"sponsor_user_id", "profile", "env_ceiling", "reason"}` | admin | A second admin applies the pending change (200 with the agent) |

Unknown body fields are refused. A missing or bad field answers 422, and so does a profile
pg_sage doesn't ship, on create and on `PATCH`. An unknown agent answers 404,
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
| `agent_identity_bound` | `POST /api/v1/agents/{id}/identities` | `principal_id`, `name`, `issuer`, `subject` |
| `agent_identity_unbound` | `DELETE /api/v1/agents/{id}/identities` | as `agent_identity_bound` |
| `agent_change_requested` | a widening `PATCH` outside single-operator mode | as `agent_updated` (`after` is what the change would make), with `change_id`, `expires_at` and `superseded_change_id` (0 when none) |
| `agent_change_confirmed` | `POST /api/v1/agents/{id}/changes/{cid}/confirm` | as `agent_updated`, with `change_id` and `requested_by_user`; `reason` is the confirming admin's |

`before` and `after` are `{"sponsor_user_id", "profile", "env_ceiling"}`. No row carries a token
secret, its hash or its prefix. An empty `PATCH` changes nothing and writes nothing. Kills and
unfreezes are recorded in the action log (see [the kill switch](#kill-switch-freeze-and-unfreeze)).

**Levels.** Read tools work up to L3. Propose tools are capped at L2: a person approves each
proposal. `agent_*` tools reach L3 at most, and L2 while the agent is tainted. The trust level
and the gate can only lower these.

**Production writes (D7).** A write or DDL in `prod` needs point-in-time recovery: the
provider's PITR flag from cloud telemetry, else WAL archiving that is on and has a command or
library. Without it the request is refused (`agent_no_pitr`). L3 is earned only with a passed
restore drill in the last 14 days (a pass exactly 14 days old still counts; one second older
caps at L2). The drill time D7 reads is the database's newest passed row of
`sage.restore_drills`, on both PITR paths; a failed drill after a pass does not reset it, and
with no passed drill (or none readable) production writes and DDL stay at L2, where a person
approves each one, even with PITR on.

pg_sage earns that drill itself: the fleet leader runs a restore drill on `clone.provider`
([restore drills](managed-clouds.md#restore-drills)). Branch and copy-on-write kinds
(`lakebase_branch`, `neon_branch`, `aurora_clone`) and `dle` drill every 30 days by default;
when the agent gate is on for a production database the cadence of a cheap kind is shortened
to 12 days (`require_restore_drill_days` - 2), so the monthly default does not keep D7 at L2.
Full copies drill only when both `clone.allow_full_copy` and the policy document's
`drill.kinds` include them. A failed drill opens the critical finding `restore_drill_failed`;
without a provider the database gets `recoverability_unknown` and stays at L2. Whether PITR
actually works is also reported by `wal_archive_failing`, `wal_archive_stale` and
`backup_stale` (see [Backups and WAL archiving](findings.md#backups-and-wal-archiving)).

**Environment ceiling.** The effective ceiling is the narrower of the agent's `env_ceiling`
and its profile's ceiling. A database's environment comes from its verified label (see
[Environments](#environments)), never from the agent, and an unverified database counts as
`prod`. A request to a database above the effective ceiling is refused with
`agent_env_ceiling`.

### Profiles

pg_sage ships these profiles, fixed in this release. Creating an agent or changing its
profile to a name outside this table answers 422, and the Agents page offers only these. An
agent created before this check with another profile still loads, with a "profile not
recognised" warning on the Agents page; its profile allows no class, so every `agent_*` request
under it is refused until an admin sets one of these.

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
[Decommissioning the removed database provisioner](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/decommission/README.md).

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
  where the subject is the actor. An admin binds it on the agent's **OAuth identities**
  section of the **Agents** page, or through the API. An identity with no binding, or bound
  to a retired agent, is refused (403 `identity_unbound`).

### Binding an OAuth identity

A binding grants the identity exactly what the agent may do: its profile and ceiling. So,
like minting a token, it takes one admin and is audited. All four routes are admin only.

| Route | What it does |
|---|---|
| `GET /api/v1/mcp/oauth/issuers` | `{"enabled", "issuers"}`: the issuer URLs from `mcp.oauth.issuers` as the server started (no JWKS or algorithms); empty while OAuth is off |
| `GET /api/v1/agents/{id}/identities` | `{"items", "truncated"}`: the agent's bindings (`issuer`, `subject`, `principal_id`, `created_by`, `created_at`), oldest first, at most 500 |
| `POST /api/v1/agents/{id}/identities` | `{"issuer", "subject"}` binds one: 201 with the binding |
| `DELETE /api/v1/agents/{id}/identities?issuer=&subject=` | removes one at once: 200 with the removed binding |

- The issuer must be exactly one of `mcp.oauth.issuers` (the same string, with no trailing
  slash added or dropped), and https, or http only on a loopback host as `mcp.oauth`
  allows. While `mcp.oauth.enabled` is off nothing can be bound.
- The subject is the token's actor (`act.sub` under delegation, else `sub`): 1 to 255 bytes,
  with no control characters.
- An unknown issuer, a non-https issuer, an empty subject, one over 255 bytes, or OAuth off
  answers 422 `invalid_identity`. A body that is not one JSON object, or has an unknown
  field, answers 400 `invalid_body`.
- An identity bound already, to this agent or another, answers 409 `identity_bound`: unbind
  it there first. A retired agent answers 409, and a frozen one 409 `agent_frozen` (unfreeze
  it first). An unbind of an identity the agent doesn't hold answers 404 `identity_not_bound`.
- Unbinding narrows, so it works in any status and for an issuer no longer configured. The
  next request with that identity's tokens is refused.
- Bindings live in pg_sage's store, so fleet mode has one set for every database. Managed
  providers are not affected.

```bash
# admin session cookie in $SAGE_COOKIE
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents/<agent id>/identities \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"issuer":"https://idp.example.com","subject":"agent-client-id"}'
curl -s -X DELETE -b "sage_session=$SAGE_COOKIE" \
  'https://pg-sage.example.com:8080/api/v1/agents/<agent id>/identities?issuer=https%3A%2F%2Fidp.example.com&subject=agent-client-id'
```

A bad token, audience or issuer answers 401 `invalid_token`, a missing scope 403
`insufficient_scope`, and an issuer or binding lookup that can't be reached 503. pg_sage can't
revoke an access token your issuer signed: a kill freezes the agent, which limits its OAuth
tokens to read tools. Retire the agent, or unbind the identity, to refuse them outright.

### Databricks Apps

Inside a Databricks App none of this is reachable yet: MCP, agent-token issuance and the
specialist API answer 501 `databricks_app_protocol_unsupported`, and no forwarded header or
bearer token establishes an agent identity. Service-principal access waits on the header
spike in [Databricks Apps: service principals and MCP](databricks-apps.md#service-principals-and-mcp);
when it ships, an admin will bind each service principal to an agent here, under its own
issuer `<workspace>/databricks-apps-sp`, the same way as an OAuth identity.

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
| Widen a principal: a higher `env_ceiling`, or a profile that grants more | `PATCH /api/v1/agents/{id}`, then `POST /api/v1/agents/{id}/changes/{cid}/confirm` | another admin within 15 minutes, with the same fields; not the requester, the agent's sponsor or the sponsor the change names | one admin, with a reason |
| Widen a database's environment label (toward `branch`) | `PUT /api/v1/agent-environments/{database}` | another admin repeats the same label within 24 hours | does not apply |
| Unfreeze after a kill | `POST /api/v1/agents/{id}/unfreeze` | another admin within 15 minutes, not the agent's sponsor | one admin, with a reason |
| Unfreeze after an operator freeze | `POST /api/v1/agents/{id}/unfreeze` | none: one admin | does not apply |
| Release a fleet or database kill | `POST /api/v1/agents/kill/release` | another admin within 15 minutes | one admin, with a reason |
| Ratify an agent's policy proposal that widens the policy | `POST /api/v1/policy/proposals/{id}/ratify` | a second admin; neither may be the agent's sponsor | one admin, with a reason |

Every request in the table is an admin's. The first admin's request waits as pending (202 for
a principal change, an unfreeze, a release or a label), and the same admin repeating it doesn't
count. Narrowing changes (a lower ceiling, a narrower label, a freeze, a kill) need one person
and apply at once.

**Widening a principal.** A profile change widens when the new profile adds a capability
class, reaches a higher effective ceiling (the narrower of the agent's `env_ceiling` and the
profile's) or adds the direct lane; a move to or from a profile pg_sage doesn't ship always
widens. So `coding-agent` to `readonly-analyst` on a `stage` agent applies at once, but on a
`prod` agent it widens, because `readonly-analyst` reads `prod`. Raising `env_ceiling` always
widens. A sponsor change never does. Outside single-operator mode a widening `PATCH` answers
202:

```json
{"status": "pending", "change_id": 7, "expires_at": "2026-11-11T15:15:00Z",
 "requested_by": "alice@example.com", "patch": {"env_ceiling": "prod"},
 "superseded_change_id": 0}
```

The whole patch waits, including any sponsor or narrowing field in it, and nothing applies
until it is confirmed. Another admin reads it with `GET /api/v1/agents/{id}/changes` and
confirms it with exactly the same fields:

```sh
curl -s -X POST https://pg-sage.example.com:8080/api/v1/agents/<agent id>/changes/7/confirm \
  -H 'Content-Type: application/json' -b "sage_session=$SAGE_COOKIE" \
  -d '{"env_ceiling": "prod"}'
```

The confirm answers 403 `second_admin_required` for the requester and 403
`sponsor_cannot_approve` for the agent's sponsor or the sponsor the change names, 409
`change_mismatch` for other fields, 409 `change_expired` after 15 minutes (the change is
closed and must be requested again) and 409 `change_not_pending` once it is applied, expired or
superseded. An agent has at most one pending change: a newer widening `PATCH` supersedes it.
Retiring the agent ends it too (409 on confirm). A `PATCH` that retires the agent and widens it
at once answers 422. pg_sage judges a narrowing `PATCH` again under the agent's row lock: if
another change landed in between and the patch now widens the agent, it answers 409
`agent_changed` and applies nothing; read the agent again and resend.

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

Posture says how exposed a database is; it does not decide whether a change runs. Every
change, an agent's included, passes the policy gate: see [Policy gate](policy-gate.md) for
its evaluation order and every verdict and reason it records.

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
  `ANALYZE` and `VACUUM` don't trigger a run, and neither do temporary tables, temporary
  views or the `pg_temp_N` schemas: posture doesn't read them.
  - Each problem is a finding in `sage.findings` with the category `agent_posture:AP-NN`.
    It resolves on its own once a completed check no longer sees it. A check that hit its
    row limit isn't complete: see [Large catalogs](#large-catalogs).
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
read with `aclexplode` on the object's ACL, or its default ACL. A grant counts when it is
made to the exposed role, to `PUBLIC`, or to a group role the exposed role inherits,
directly or through other groups (up to 16 levels). The same goes for `USAGE` on the
object's schema, which a grant also needs. Inheritance is read from `pg_auth_members`:
before PostgreSQL 16 by each member's `INHERIT` attribute (`rolinherit`), from 16 by each
membership's `INHERIT` option. AP-03, AP-04, AP-05, AP-06 and AP-09 count it.

- Findings name the path, for example `anon → api_reader`, in the detail and as
  `pg_auth_members` evidence.
- A fix that revokes from a group warns that every member of the group loses the
  privilege, and offers ending the membership instead (`REVOKE api_reader FROM anon`; on
  PostgreSQL 16+ add `GRANTED BY` when another role granted it). Only the shortest path is
  named, so any other membership that leads to the same group has to end too.
- On PostgreSQL 14, AP-06's `REVOKE` names every group the exposed roles inherit as well,
  since a grant of the view to any of them still reaches the exposed role.
- A membership that only allows `SET ROLE` (no inheritance) gives nothing until the client
  switches roles, so it is not reported.
- User mappings never inherit, so AP-09 matches a mapping to the exposed role itself.
- AP-15 still reports `PUBLIC` grants only.
- When more memberships than the read's limit exist, the detectors that use them report a
  truncated run, so their open findings stay open.

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
| AP-03 | A table with row-level security off that grants `SELECT`, `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE` to an exposed role or `PUBLIC`. Also a view granted to one that runs as its owner over such a table. One finding per schema past 20 | critical | `ENABLE ROW LEVEL SECURITY` (write the policies first), or `REVOKE ALL … FROM …`; per schema `REVOKE ALL ON ALL TABLES IN SCHEMA … FROM …` |
| AP-04 | A permissive policy for an exposed role or `PUBLIC` whose `USING` or `WITH CHECK` is plain `true` | warning | `ALTER POLICY … USING (<predicate>)`, or `DROP POLICY` |
| AP-05 | A `SECURITY DEFINER` function or procedure that an exposed role (or `PUBLIC`, the default) can execute, with no `search_path` in its settings. One finding per schema past 20 | warning | `ALTER FUNCTION … SET search_path = pg_catalog, pg_temp`; optionally `REVOKE EXECUTE`; per schema a `DO` block that pins them all |
| AP-06 | A view in an exposed schema (one where an exposed role has `USAGE`) over a table with row-level security, without `security_invoker`. On PostgreSQL 14 it's reported as "no security_invoker available". One finding per schema past 20 | warning | PostgreSQL 15+: `ALTER VIEW … SET (security_invoker = true)`. PostgreSQL 14: revoke the view from exposed roles |
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

### Large catalogs

**Row limits.** Each catalog read is bounded: AP-01 to AP-08 read at most 50,000 rows, AP-09
to AP-16 at most 5,000. A read that reaches its limit is *truncated*:
- the check's note says so, for example "relations: first 50,000 rows read, later rows not
  checked";
- the analyzer logs a warning naming the detector;
- the findings that were read are reported, but the detector counts as not evaluated, so its
  open findings stay open (including those on objects past the limit) until a run that reads
  everything.

**Schema findings.** In one schema, AP-03, AP-05 and AP-06 report at most 20 findings on
their own. With more than 20, the schema gets one finding instead, with the same severity
and the object type `schema`:
- its evidence holds the count and the first 50 objects;
- its fix script covers the whole schema. AP-03 revokes the schema's tables from the roles
  that hold them, with `REVOKE ALL ON ALL TABLES IN SCHEMA` (the RLS alternative is listed
  for the first tables). AP-05 pins the `search_path` of every definer routine in the
  schema with a `DO` block. AP-06 makes every such view `security_invoker`, or on
  PostgreSQL 14 revokes them.

When a schema crosses 20, its per-object findings resolve once and the schema finding
replaces them (and the reverse when it drops back to 20 or fewer).

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
| `GET /api/v1/agents/freezes` | operator | Lists the open fleet, database and agent freezes, fleet first: `scope`, `target`, `reason`, `set_by`, `set_at`, `after_kill` and the live `pending_release` request. At most 200 rows; `truncated: true` says more are open |

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

### From the Agents page

The Agents page does all of this without curl:

- **Edit.** An admin opens an agent and changes its sponsor (an operator or admin), profile
  or ceiling under **Edit**. Only the changed fields are sent. A widening change shows
  "awaiting a second admin" with its change id: another admin confirms it (see
  [Two-person rules](#two-person-rules)). A sponsor change while the agent is frozen shows the
  server's `sponsor_change_frozen` refusal. An upgraded install's legacy agents start without a
  sponsor: assign one here.
- **Retire.** **Retire agent** asks for a reason and the agent's name, typed exactly. The dialog
  shows `agents.roles.retire_grace_days`, the days before the roles are dropped. Once the agent
  is retired, the drawer no longer offers Edit, Retire, Freeze or new tokens.
- **Tokens.** Each active token in the drawer has **Revoke**. The MCP Tokens page still lists
  and revokes every token.
- **Kill.** **Kill agents** takes a scope: every agent, one agent or one database. Each needs
  the reason and the typed `KILL AGENTS` phrase; a one-agent or one-database kill also needs
  its target chosen.
- **Release.** The banner above the list shows each open fleet and database freeze, with who
  set it and why. An admin types a reason and presses **Release**. After a kill, the first admin's
  release shows "pending second admin" and the banner names the request until another admin
  releases it too. Operators see the banner without **Release**. Unfreeze each agent from its
  drawer.

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
