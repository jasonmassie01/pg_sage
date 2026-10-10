# AgentDB code audit: domain model, agent-facing surface, security (2026-10-05)

Auditor: research agent (AgentDB code audit), for the AgentDB first-principles review and new build spec.
Worktree `C:/Users/jmass/pg_sage-agentdb-spec` at `72646ab1` (= origin/master). Read-only apart from this
file. No database or cloud was contacted. Two `go test` runs were made in Docker without
`SAGE_TEST_DATABASE_URL` (§5).

Conventions:
- Code paths are relative to `sidecar/`. Paths starting `docs/`, `reviews/` or `.github/` are relative to the
  repo root.
- **Severity:** P0 means data loss, a cross-tenant breach or a spend bypass. P1 means an enterprise control
  is missing or materially broken. P2 means a functional bug or a weak control. P3 is hygiene.
- **Confidence:** CONFIRMED means traced in code; two items were also checked by executing the copied
  helpers (§4.1). PLAUSIBLE means the path exists but the impact depends on timing, configuration or
  external behaviour.
- **Scope:** provider runners, Terraform, live-execution internals and teardown mechanics belong to the
  parallel deployment-path audit. They appear here only where the domain model or the agent surface
  depends on them.

---

## 0. Executive summary

1. **What it is.** AgentDB is a human-operated control plane for per-agent-run Postgres. It keeps a
   registry, request approvals, TTL leases and admin-authorized cloud create/destroy. It is not agent
   self-service.
   - An agent with a token can call four REST routes: create or list its requests, and list or read its
     tenant's deployments. With a separate ping token it can also send a heartbeat
     (`internal/api/agent_db_agent_api.go:56-70`, `internal/api/agent_db_handlers.go:25-28`).
   - Agents get no credentials, branching, lease extension, teardown, recommendations or MCP tools. There
     are 0 AgentDB MCP tools.
2. **It is a separate product inside the binary.** `internal/agentdb` imports no other pg_sage package
   (`grep` of imports: only `internal/testdb`, in tests).
   - It runs parallel machinery for policy, execution, approvals, tokens, audit, emergency stop and schema
     migration (§6).
   - The real integrations are these:
     - fleet sync, which attaches the full pg_sage runtime to active agent DBs that have an env secret ref
       (`cmd/pg_sage_sidecar/agentdb_fleet.go:89-117,176-206`);
     - retention;
     - RBAC;
     - the LLM manager (for blueprints).
3. **The 2026-09-26 P0s are fixed** (G8-B01..B06; re-verified in §4.6). The live-authority core (plan hash,
   estimate, single-use authorization, receipts, ownership-verified destroy, durable teardown claims) is
   careful work and is load-bearing.
4. **Remaining P1s are enterprise controls.**
   - **Audit cannot be attributed or completed** (A-03..A-05): most events carry no actor, several
     mutations are not audited, token ids are redacted out of the audit, and request decisions and token
     mints are written where no API can read them.
   - **Agent tokens cannot be listed or revoked** (A-01).
   - **The restore-verified destroy gate is a free-text admin attestation**; the UI invents the backup id
     and the "checks" (A-02).
   - **No credential handoff has least privilege** (A-06). The only real credential is the RDS master-user
     Secrets Manager ARN, which every agent token in the tenant can read.
5. **Main P2 bugs:**
   - recommendation and deploy-request upserts can overwrite another deployment's rows (A-07, A-08);
   - approvals are not bound to the content reviewed (A-09);
   - Neon/Supabase blueprints can never be approved (A-10);
   - the UI's region, account, project and source fields are silently dropped (A-11);
   - provider policy expires 30 days after the last save (A-12);
   - unauthenticated pings can amplify audit writes (A-13);
   - request policy auto-approves any positive budget (A-14);
   - lease extension is unbounded in total (A-15);
   - the budget "pause" is only a label (A-16);
   - local schemas and databases are never dropped (A-17);
   - the UI has no approve/deny control for requests (A-18).
6. **Dead or unwired weight.** These include:
   - the durable monitoring queue (545 LOC and 3 tables);
   - Terraform templates (review-only) and deploy requests (never executed);
   - `agent_db_pings` (write-only), `auto_within_policy` (nothing auto-issues) and static tuning hints;
   - inline-credential fleet sync and several dead columns and states.

   The footprint is about 13.7k domain LOC, 2.6k API LOC, 3.7k UI LOC and 0.4k cmd LOC (about 20.4k
   non-test LOC), 27 `sage.*` tables and about 60 REST endpoints.
7. **Tests.** Without a database: 104 pass, 4 fail (hard `t.Fatal` instead of skip), 123 skip, 36.7%
   coverage. CI runs them against Postgres (`.github/workflows/ci.yml:18-33`). The gaps sit exactly where
   the new bugs are (§5).

---

## 1. What AgentDB does today, end to end

### 1.1 Actors and principals

| Actor | How authenticated | What it can do | Evidence |
|---|---|---|---|
| Human **admin** | session cookie, `RequireRole("admin")` | Everything an operator can, plus: upsert provider policy, mint agent tokens, `authorize-live`, restore-drill attestation | `internal/api/agent_db_handlers.go:95-99,142-147,241-245,254-257` |
| Human **operator** | session, `RequireRole("admin","operator")` | Every other management route for **any tenant**. This includes requests on behalf of any tenant/agent, approve/deny, provision, dry-run, ping tokens, lease, archive/restore/delete, backups, costs, recommendations, blueprints, templates, deploy requests and reconcile | `internal/api/agent_db_handlers.go:24-38,51-278`; single-team model `docs/agent-db-deployments.md:71-85` |
| **Agent** | `Authorization: Bearer agt_…` (agent token, tenant-bound) | Create/list its tenant's requests; list/read its **tenant's** deployments (all agents in the tenant) | `internal/api/agent_db_agent_api.go:36-133` |
| **Agent runtime heartbeat** | `Bearer <ping token>` on an unauthenticated route | Record `last_ping_at` + `agent_status` for one deployment | `internal/api/agent_db_handlers.go:25-28`, `internal/agentdb/identity.go:271-281`, `internal/agentdb/store.go:118-146` |
| **pg_sage reconciler** | in-process, every 300 s | Archive expired leases; TTL live destroy; reconcile in-flight provider ops; fleet sync | `cmd/pg_sage_sidecar/agentdb_reconciler.go:13-68` |
| **pg_sage fleet runtime** | env-ref DSN | Full AI-DBA runtime on attached agent DBs | `cmd/pg_sage_sidecar/agentdb_fleet.go:176-206`, `cmd/pg_sage_sidecar/database_runtime.go:119-147` |
| **LLM** | n/a | Turns operator "intent" into a blueprint spec | `internal/api/agent_db_blueprint_handlers.go:116-163` |

### 1.2 User stories as implemented

**Implemented:**
- **Platform engineer.** "Configure provider policy and size profiles. Review agents' database requests.
  Provision a schema, a database or a cloud instance per agent run, with a TTL and a budget. Run dry-run
  plans, then an admin-authorized live create. pg_sage destroys the cloud resource when the lease expires,
  after someone attests a restore drill."
  - This is the only complete story. Every step is human-driven except TTL teardown.
- **Agent.** "I can ask for a database and later see whether it was approved and which deployment row it
  became (endpoint plus secret reference)."
- **Agent runtime.** "I can heartbeat. It changes nothing about the lifecycle."
  - A ping never extends a lease (`internal/agentdb/store.go:133-141`). Abandonment means TTL expiry only,
    even though `docs/agent-db-deployments.md:30` says pings drive cleanup.

**Not implemented:**
- a coding agent provisioning its own database on demand (no self-serve provision; humans consume
  requests);
- copy-on-write branching per run (`branch` isolation is "deferred", `internal/agentdb/policy.go:39-40`;
  hosted branch parents come only from size profiles, §4 A-11);
- an app agent connecting with least-privilege credentials (§3.6);
- an agent reading recommendations or tuning hints, extending its lease or releasing its database (no
  agent routes).

### 1.3 Flow, request to teardown

```
 admin: provider-configs ─┐        operator: identities ── admin: mint agent token (agt_…)
                          │                                             │
 ┌──────────────── REQUEST ─────────────────────────────────────────────┘
 │ agent:  POST /api/v1/agent-api/agent-db-requests   (tenant/agent forced from token)
 │ human:  POST /api/v1/agent-dbs/requests            (tenant/agent from body)
 │   └─ Store.CreateRequest → DecideRequest           store.go:17-44, policy.go:12-44
 │        approved(policy) | requested(review) | denied | deferred
 ▼
 DECISION (human, operator): POST …/requests/{id}/approve|deny   request_decision.go:26-68
 │   decided_by = session actor; policy deny is terminal; consumed requests are final
 ▼
 PROVISION (operator): POST …/requests/{id}/provision  or  POST /agent-dbs {request_id}
 │   claimForProvision (single use) → Store.Provision → Register     request_provision.go:14-58
 │   local_postgres: CREATE SCHEMA/DATABASE (opt-in env)              local_provisioning.go:34-80
 │   cloud: plan from SIZE PROFILE params only → provisioning_status=planned   schema.go:94-125
 │   (alt: blueprint/template provision also consumes a request)      provision_links.go:47-150
 ▼
 DRY-RUN (operator): /provision/preflight → /provision/execute → dry_run_ready  execution.go:28-117
 ▼
 LIVE CREATE (admin): /provision/authorize-live {operation:create}   agent_db_live_authority.go:37-144
 │   plan+estimate+authorization (10 min) → same admin: /provision/execute {tuple}
 │   ExecuteProvisionLive → runner.Create → receipt → available | create_uncertain   live_create.go:14-163
 ▼
 REGISTER INTO MONITORING (reconciler, 300 s): syncAgentDBsToFleet      agentdb_fleet.go:89-153
 │   only status=active with env:PG_SAGE_AGENTDB_* DSN whose host matches connection_info
 │   → full pg_sage runtime + sage schema bootstrapped *inside the agent DB*  database_runtime.go:119-147
 ▼
 MONITOR: agent heartbeat (ping token) | operator cost samples/backups/recommendations | static hints
 ▼
 LEASE: operator /extend-lease (per-extension cap only)              store.go:165-201, cost_guard.go:36-56
 ▼
 TEARDOWN (reconciler): claimExpired → archived(claim) → [cloud live] owned? + restore_verified?
 │   → destroy_pending → destroying → destroyed      teardown_reconcile.go:39-158, lifecycle_claim.go
 │   [cloud dry-run] destroy_dry_run_ready (never deletable)   [local] nothing is dropped
 │   restore_verified only via admin POST /{id}/backups/restore-drill  restore_drill.go:24-59
 ▼
 DELETE (operator): DELETE /agent-dbs/{id} → status=deleted (tombstone)   store.go:211-248
```

Supporting detail for the stages:
- **Teardown blocking.** Teardown is blocked by default until a `restore_verified` backup exists.
  - `backup_required` is forced on by `agentdb.require_backup_before_destroy=true`
    (`internal/agentdb/register.go:116`, `internal/config/config.go:1035-1039`).
  - The UI always sends `backup_required: true` (`web/src/pages/AgentDBsPage.jsx:266`).
  - So every expired live database waits for an admin attestation (A-02).
- **Fleet attachment.** The attached runtime runs:
  - monitoring, execution, facts, SRE actions, ask and self-config, all through the normal
    `policy.Gate`;
  - under the *global default* execution mode (falling back to `auto`;
    `cmd/pg_sage_sidecar/mode_init.go:189-200`, `internal/config/fleet.go:160-165`).
  - The deployment's own `execution_mode` column (default `manual`,
    `internal/agentdb/monitoring_schema.go:10-11`) is ignored.

### 1.4 Agent capability matrix

| Capability | Agent token | Ping token | Needs a human |
|---|---|---|---|
| Ask for a DB (request) | yes | no | approve if `review` |
| Provision the approved request | **no** | no | operator |
| Read own tenant's deployments (endpoint, `secret_ref`, plan, metadata) | yes (whole tenant) | no | – |
| Get usable credentials | **no** (only RDS master-secret ARN; §3.6) | no | operator sets env DSN |
| Heartbeat | no | yes | operator mints ping token |
| Extend lease / release / delete | **no** | no | operator |
| Read recommendations / tuning hints / cost / backups | **no** | no | operator session only |
| Submit schema change (deploy request) | **no** | no | operator; never executed |
| Branch / clone / reset | **no** | no | not built |

---

## 2. Data model

### 2.1 Tables (27 in `sage.*`, created lazily by `Store.Ensure`)

All tables are created by `internal/agentdb/schema.go:30-85`. The base DDL is in
`internal/agentdb/schema_statements.go:3-355`. Further statements are appended by `init()` from
`internal/agentdb/live_execution_schema.go`, `internal/agentdb/monitoring_schema.go` and
`internal/agentdb/safety_schema.go`. Migrations are memoized per pool and recorded in
`agent_db_schema_version` (`internal/agentdb/schema.go:20-53`).

**Registry and approvals**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_identities` | `agent_id` PK, `tenant_id`, `owner_id`, `status`, `metadata` | none beyond PK | `agent_id` is a global namespace; upsert can move an agent to another tenant (A-31). Not referenced by any FK |
| `agent_db_requests` | `request_id` PK, `tenant_id`, `agent_id`, `provider`, `requested_isolation_type`, `budget_usd`, `backup_required`, `policy_decision`, `status`, `idempotency_key`, `body_hash`, `policy_reasons`, `decided_by/at`, `consumed_deployment_id/by/at` | unique `(tenant_id, idempotency_key) WHERE key<>''` (`schema_statements.go:43-45`) | **No region, data classification, masking policy or SLA columns.** They live only in the body hash and reason strings, so the approver cannot see them (A-14). No unique on `consumed_deployment_id` (A-25) |
| `agent_db_deployments` | 40 columns (§2.2) | PK only; indexes `(tenant_id,status)`, `(lease_expires_at,status)`, non-unique `(provider, provider_resource_id)` | No CHECK on any enum. No FK to requests: the link is `metadata->>'request_id'`. Never hard-deleted (tombstone) |

**Policy and designs**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_db_provider_configs` | `provider` PK, `enabled`, `settings`, `last_validated_at` | – | `last_validated_at` is never written (A-12) |
| `agent_db_size_profiles` | `profile_id` PK, provider, level, sizing, `provider_params` | – | The **only** trusted source of live provider params (A-11). No secret filter on `provider_params` |
| `agent_db_blueprints` | `blueprint_id` PK, status, intent, spec json, findings, `llm_used`, `raw_response`, created/approved by | – | Upsert by id resets status (A-09) |
| `agent_db_terraform_templates` | `template_id` PK, status, sha256, files, manifest, findings | – | Review-only; never executed |

**Agent heartbeat and tokens**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_db_pings` | `ping_id`, `deployment_id` FK, status, metrics | FK cascade | **Write-only**; retention keeps the newest one |
| `agent_db_ping_tokens` | `token_id` PK, `deployment_id` FK, `agent_id`, `token_hash` UNIQUE, scope, status, `expires_at`, `revoked_at`, `rotated_from_token_id` | FK cascade | SHA-256 hash only |
| `agent_db_ping_token_failures` | `failure_id`, `deployment_id` (**no FK**), `token_hash`, reason | – | Unauthenticated writer (A-13) |
| `agent_db_agent_tokens` | `token_id` PK, `tenant_id`, `agent_id`, `token_hash` UNIQUE, status, `created_by`, `expires_at`, `revoked_at` | – | No code ever revokes (A-01) |

**Operations and evidence**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_db_recommendations` | `recommendation_id` **PK (global)**, `deployment_id` FK, kind, title, `agent_instructions`, payload, feedback | FK cascade | Global PK plus an unscoped upsert gives cross-deployment overwrite (A-07) |
| `agent_db_cost_samples` | serial, `deployment_id` FK, `cost_usd`, metric | FK cascade | Operator-posted only; no idempotency key |
| `agent_db_backups` | `backup_id` PK, `deployment_id` FK, status, `archive_uri`, `verified_at`, `restore_verified_at`, detail | FK cascade; upsert scoped to the owner (`operations.go:451-466`) | `restore_verified` only via the drill (A-02) |
| `agent_db_tuning_hints` | `(hint_id, deployment_id)` PK | FK cascade | Static text |
| `agent_db_provision_attempts` | serial, `deployment_id` FK, kind, status, runner, command, stdout/stderr, detail | FK cascade | Attempt log |
| `agent_db_audit` | serial, `deployment_id` (**no FK, may be ''**), event, detail | – | Retention-exempt (`internal/retention/exemptions.go:115`) |
| `agent_db_deploy_requests` | `deploy_request_id` **PK (global)**, `deployment_id` FK, tenant, agent, SQL texts, status, reviewed_by | FK cascade | Unscoped upsert (A-08) |
| `agent_db_creation_receipts` | `deployment_id` PK/FK, provider, `provider_resource_id`, `operation_mode` | FK cascade | Ownership evidence for destroy |

**Live-execution authority**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_db_live_plans` | `plan_hash` PK, `deployment_id` FK, payload | FK cascade | Live authority |
| `agent_db_live_estimates` | `estimate_id` PK, `plan_hash` FK, `deployment_id` FK, payload, `expires_at`, superseded/consumed | FK cascade | 15 min TTL |
| `agent_db_live_authorizations` | `authorization_id` PK, estimate/plan/deployment FKs, operation, `requester_id`, `idempotency_key`, `expires_at`, consumed/revoked | `UNIQUE(deployment_id, operation, idempotency_key)` | 10 min TTL; requester is also the reviewer (A-29) |
| `agent_db_live_receipts` | `authorization_id` PK/FK, `idempotency_key`, `plan_hash`, payload | FK cascade | Replay receipts |

**Monitoring queue and housekeeping**

| Table | Key columns | Constraints | Notes |
|---|---|---|---|
| `agent_db_monitoring_policies` | `(scope_type, scope_id)` PK, `max_concurrency` | – | **Unwired** |
| `agent_db_monitoring_state` | `physical_target_key` PK, due/scheduled times | – | **Unwired** |
| `agent_db_monitoring_work` | `work_id` PK, target, tier, status, claim fields | – | **Unwired**. Only `Delete` touches it (`store.go:230-243`) |
| `agent_db_schema_version` | singleton PK `CHECK(singleton)`, version | the only CHECK in the schema | – |

The retention comment says "the 18 tables" (`internal/retention/agent_rules.go:5-9`). There are 27.

### 2.2 The deployment row (40 columns)

The row is defined in `internal/agentdb/deployment_types.go:5-47` and `internal/agentdb/queries.go:26-34`.
The three monitoring columns come from `internal/agentdb/monitoring_schema.go:8-13`.

| Group | Columns | Notes |
|---|---|---|
| Ownership | `deployment_id`, `tenant_id`, `agent_id`, `run_id`, `database_name`, `schema_name` | Free text. Not validated against `agent_identities` on the human paths |
| Shape | `provider`, `provisioning_level`, `isolation_type`, `size_profile_id` | `isolation_type` is always forced equal to `provisioning_level` (`internal/agentdb/providers.go:33-39`) |
| Lifecycle | `status`, `lease_expires_at`, `last_ping_at`, `agent_status`, `lifecycle_version`, `cleanup_claim_id/at`, `teardown_operation_id`, `teardown_blocked_reason/at` | – |
| Provider state | `provisioning_status`, `provisioning_plan`, `provider_resource_id`, `create_operation_id`, `provider_mutation_id/expires_at`, `live_mode`, `connection_info` | – |
| Credentials | `secret_ref`, `secret_ref_provider`, `secret_ref_expires_at` | `secret_ref_expires_at` has no writer in any API; it is read only by fleet sync |
| Money and safety | `budget_usd`, `backup_required`, `safety_mode` | `safety_mode` is free text with no semantics; it only takes part in the teardown CAS, `internal/agentdb/lifecycle_claim.go:38` |
| Monitoring queue | `monitoring_mode`, `execution_mode`, `wake_idle_allowed` | Read only by the unwired scheduler; `execution_mode` is ignored by fleet attach |
| Free-form | `metadata` | Unredacted; returned to agents |

### 2.3 State machines

**Lifecycle `status`.** Values are `active | archived | budget_exceeded | deleted`. Nothing enforces them;
they are plain text.

```
            Register (insert, or same-owner pre-live re-plan → status='active')   queries.go:40-90
 (none) ─────────────────────────────────────────────────► active
                                                            │  ▲   ▲
     AddCostSample: sum(samples) ≥ budget (status='active') │  │   │ ExtendLease: archived+claim → active
                                                            ▼  │   │      register.go:122-141
                                                   budget_exceeded │
                                                            │      │ Restore: ANY → active (no guard) store.go:207-209
   claimExpired (active|budget_exceeded, lease<now, SKIP LOCKED) lifecycle_claim.go:14-28
                                                            ▼      │
   Archive (operator): ANY → archived, no claim ──────────► archived ─────────────┐
                                                            │                     │ Delete: archived ∧
                                                            │  (TTL teardown, §2.3 b) │ (¬backup_required ∨
                                                            ▼                     │  restore_verified) ∧
                                                         deleted ◄────────────────┘ (local ∨ prov=destroyed)
                                    (tombstone; Restore/Archive can resurrect, A-19)   policy.go:59-92
```

**`provisioning_status`.** The transition table is `internal/agentdb/provider_runner.go:171-203`. Raw-SQL
writers bypass it (A-26).

```
registered ─► planned ─► preflight_passed ─► provisioning ─► dry_run_ready            (dry-run)
   │            │              ▲                 │
   │            │              └─ failed ◄───────┤ definitive reject (live_create.go:152-163)
   │            ▼                                ├─► available ─► status_checked
   │   status_checked / destroy_dry_run_ready    ├─► create_uncertain ─(reconcile)► available|failed
   ▼                                             └─► status_unknown
provisioned (local)
available|status_checked ─(TTL claim or admin destroy)► destroy_pending ─► destroying ─► destroyed
                                                                     └─► status_unknown ─► destroying
dead entries: queued, cancel_requested, cancelling (provider_runner.go:197-199),
              "archived" as a from-state (194), "ready" (execution.go:71) — nothing sets them
```

**Request.** Writers: `internal/agentdb/store.go:17-44`, `internal/agentdb/request_decision.go:26-68`,
`internal/agentdb/request_provision.go:163-196`.

```
create ─DecideRequest─► approved(policy:allow, decided_by=policy) | requested(review) | denied(policy) | deferred
requested|deferred|approved ─human approve─► approved/allow   (refused if policy_decision='deny')
any unconsumed ─human deny─► denied/deny (terminal: a later approve is refused)
approved/allow ─claim─► consumed_deployment_id set (status STAYS 'approved')
                └─ provision error ─► claim released (approval reusable)
```

**Other entities:**
- **Ping token:** `active → revoked`. Expiry is time-based only.
- **Agent token:** `active` only. No code path changes it (A-01).
- **Backup:** `planned | ready | verified | unverified | restore_verified`. Only `RecordRestoreDrill` can
  write `restore_verified`.
- **Blueprint:** `generated | rejected → approved`. Regenerating with the same id resets it to
  `generated` or `rejected`.
- **Template:** `draft | rejected → approved`, with the same reset.
- **Deploy request:** `draft → review_requested → approved | denied`. An upsert can push it back to
  `draft` or `review_requested` while keeping the old `reviewed_by` (A-08).
- **Live records:**
  - estimate: `issued → consumed | superseded | expired (15 min)`;
  - authorization: `issued → consumed | revoked | expired (10 min)`;
  - receipt: replay evidence.
- **Monitoring work:** `queued → claimed → (completed | failed) | revoked`. No worker exists.

### 2.4 Data-model defects

- **Enums are not enforced.** There is no CHECK on any status, provider, level, scope or decision column
  (§2.1). Several writers bypass Go validation with raw SQL:
  - `authorizeTeardownSQL` sets `destroy_pending` from any state the claim captured
    (`internal/agentdb/lifecycle_claim.go:30-46`);
  - `clearFailedLiveCreate` (`internal/agentdb/live_create.go:201-208`).
- **`agent_identities` is not referential.** Requests, deployments and ping tokens carry `agent_id` as free
  text. Disabling an identity therefore does not touch its ping tokens or running databases (A-01, A-31).
- **Request ↔ deployment link is one-sided:** `consumed_deployment_id` on the request, JSON metadata on the
  deployment. There is no FK and no uniqueness (A-25).
- **The decision inputs are not persisted.** Region, data classification, masking policy and approval SLA
  are not stored, so approvals are blind (A-14).
- **`provider_resource_id` is not unique per provider** (non-unique index,
  `internal/agentdb/schema_statements.go:111-113`). Ownership now relies on receipts and provider tags.
- **The schema is big and partly dead.** `agent_db_deployments` has 40 columns; at least 7 are dead or
  semantically empty (`safety_mode`, `isolation_type` duplicate, `secret_ref_expires_at`,
  `monitoring_mode`, `execution_mode`, `wake_idle_allowed`, plus `agent_db_provider_configs.last_validated_at`).

---

## 3. Security review

### 3.1 Authn/authz

- **Ordinary routes** have session auth applied by middleware, except the skip list.
- **The skip list** (`internal/api/auth_middleware.go:97-135`) includes:
  - the exact `POST /api/v1/agent-dbs/{id}/agent-ping` shape (5 segments);
  - the whole `/api/v1/agent-api/` prefix.
- **Routing:**
  - only four mux patterns exist under `/api/v1/agent-api/`, and each is wrapped in
    `requireAgentPrincipal` (`internal/api/agent_db_agent_api.go:56-70`);
  - unknown paths return 404;
  - a trailing-slash variant of agent-ping falls into the operator subrouter, where `RequireRole` sees no
    user and returns 401. Correct.
- **Role checks** are coarse: operators are global. Four sub-routes are admin-only (§1.1). Execute and
  destroy-live are operator-routed but bound to the authorizing admin through `requester_id`
  (`internal/api/agent_db_live_authority.go:326-331`).
- **There is no two-person rule anywhere** (A-29):
  - an operator can create a request for any tenant or agent and approve it;
  - an admin both authorizes and executes live work;
  - `ReviewerID = requesterID` (`internal/api/agent_db_live_authority.go:124-129`), which also cancels the
    "low-confidence estimate needs review" flag (`internal/agentdb/provider_policy.go:85-89`).

### 3.2 Agent tokens

The token is `agt_` plus 32 random bytes in base64url (`internal/agentdb/identity.go:347-353`,
`internal/agentdb/agent_tokens.go:49-54`).

| Aspect | Today | Evidence |
|---|---|---|
| Mint | Admin only. Plaintext returned once. Default 7 days, max 90 days. The tenant comes from the identity row | `internal/api/agent_db_agent_api.go:137-156`, `internal/agentdb/agent_tokens.go:36-73` |
| Storage | Unsalted SHA-256, base64url. Adequate for 256-bit random secrets | `internal/agentdb/identity.go:355-358` |
| Scope | **None.** Every token can do all 4 agent routes over its whole tenant. No per-agent or per-database scope (contrast `mcptoken` scopes and databases, `internal/mcptoken/mcptoken.go:44-90`) | `internal/api/agent_db_agent_api.go:72-133` |
| Validation | One `UPDATE … FROM agent_identities` on every call (sets `last_used_at`). Requires an active identity with a matching tenant. No failure throttle | `internal/agentdb/agent_tokens.go:77-100` |
| Expiry | `expires_at > now()` | same |
| Revocation / list | **None.** No store function or route. The only lever is to upsert the identity with a non-`active` status, which is unaudited and leaves ping tokens and databases alive (**A-01, P1**) | `internal/agentdb/agent_tokens.go` (whole file), `internal/api/agent_db_handlers.go:140-155` |
| Token id | `agt_id_` plus 64 bits of SHA-256(agent‖token). Derived from the secret; not exploitable, but unusual (A-28) | `internal/agentdb/agent_tokens.go:54` |

### 3.3 Ping tokens

- **Mint, rotate, revoke** are operator-only and per deployment (`internal/agentdb/identity.go:88-234`).
- **Lifetime has no maximum.** The only check is `ExpiresSeconds <= 0` (`internal/agentdb/identity.go:103`)
  (A-28).
- **Validation** binds token, deployment and the deployment's current `agent_id`
  (`internal/agentdb/identity.go:236-269`). It ignores identity status and deployment status, so a
  tombstoned deployment still accepts pings.
- **Lockout:**
  - 5 failures per (deployment, hash) in 5 minutes;
  - 50 failures per deployment;
  - rows pruned after 24 hours (`internal/agentdb/identity.go:15-22,305-345,405-421`).
  - Valid tokens still succeed while the budget is exhausted, because the `UPDATE` runs first. There is
    no lockout DoS.
- **The ping cannot touch the lifecycle.** It writes only `last_ping_at` and `agent_status`; lifecycle
  words are rejected (`internal/agentdb/store.go:118-163`). G8-B01 is fixed.

### 3.4 Identities

- `UpsertAgentIdentity` is open to operators and is **unaudited**.
- `ON CONFLICT (agent_id) DO UPDATE SET tenant_id=…` silently moves an agent to another tenant
  (`internal/agentdb/identity.go:40-60`).
  - The effect: old agent tokens die (tenant mismatch), new tokens see the new tenant, and the agent's old
    deployments and ping tokens stay under the old tenant.
- Human request creation and local registration accept any `tenant_id` and `agent_id` without checking
  them against identities (`internal/agentdb/store.go:17-44`, `internal/agentdb/register.go:78-97`). The
  identity table only matters to agent tokens. This is **A-31 (P2)**.

### 3.5 Tenancy and the D4 status

**Agents are isolated:**
- the tenant is forced from the token on create (`internal/api/agent_db_agent_api.go:113`), list (`:80`)
  and get (`GetForTenant`, `internal/agentdb/agent_tokens.go:104-113`; `:93`);
- other tenants and tombstones return 404.

**Defense in depth is fail-open (A-24, P3).**
- The list handler ignores the `ok` from `agentPrincipalFromContext`, and `deploymentPageQuery` skips the
  tenant predicate when the value is empty (`internal/api/agent_db_agent_api.go:74-81`,
  `internal/agentdb/list_page.go:91-98`).
- This is unreachable today, because tenant ids are non-empty by construction.

**Within a tenant there is no agent-level isolation.** Any agent token reads every deployment in the
tenant, including endpoints and `secret_ref` (§3.6).

**Request ids are global primary keys.**
- An agent choosing an existing `request_id` gets a 500, which is a cross-tenant existence oracle.
- A repeated body without an idempotency key also gets a 500 (A-21).

**D4 status:**

| D4 item | Status | Evidence |
|---|---|---|
| D4a-A: global humans, attributed decisions | Built | `request_types.go:62-68`, `request_decision.go:26-68`, `safety_schema.go:22-30`, `docs/agent-db-deployments.md:71-85` |
| D4a-A audit row | Written but **unreadable**: `deployment_id=''` (A-05) | – |
| D4a-B: per-user tenant grants | Not built (by decision) | – |
| D4b-B: cloud register requires an approved, single-use request | Built | `internal/api/agent_db_register_handler.go:11-42`, `request_types.go:70-88` |
| D4b-B UI | Provisions through the request (`AgentDBsPage.jsx:274-288`) | – |
| D4b-B tests | Present | `internal/api/agent_db_request_d4_test.go:154-316` |
| Open question: is one install shared by several customer teams? | Unresolved; the docs forbid sharing | `docs/agent-db-deployments.md:73-78` |

### 3.6 Credentials: storage, encryption, who can read them

AgentDB stores **no database passwords**. `internal/crypto` is not used. What exists per provider:

| Provider | What the deployment gets | Who can read it | Least privilege? |
|---|---|---|---|
| `aws_rds` | `secret_ref` = Secrets Manager ARN of the **RDS-managed master user `postgres`** (`aws_rds_runner.go:226-227,294-295`; `aws_rds_ownership.go:26-31`; stored by `execution.go:496-514`) | every operator and admin, and **every agent token in the tenant** (`agent_db_agent_api.go:90-100`); reading the value needs AWS IAM | **No.** Master user; IAM is the only barrier (**A-06, P1**) |
| `gcp_cloudsql` | instance connection name and private IP; `secret_ref_provider=gcp_secret_manager` but no secret or user is created (`gcp_cloudsql_runner.go:196-206`) | – | Nothing usable |
| `databricks_lakebase` | endpoint and project (`lakebase_runner.go:196-206`) | – | Nothing usable |
| `neon` / `supabase` | `secret_ref` echoes an operator-set `env:PG_SAGE_AGENTDB_*` (`hosted_runner.go:185-195`) | Supabase DB password = HMAC(master env, project name), recoverable by whoever holds the master (`credentials.go:32-39`) | The env ref is a sidecar variable; agents cannot use it |
| `local_postgres` | `connection_info = {provider, schema_name\|database_name}`; `metadata.credential_scope` is only a label (`local_provisioning.go:52-55,73-78`) | – | No role is created |

- **Where `secret_ref` points.** It is restricted to `env:PG_SAGE_AGENTDB_[A-Z0-9_]+`
  (`internal/agentdb/credentials.go:19-30`) and is resolved in memory, only by fleet sync, with a
  host/database match (`cmd/pg_sage_sidecar/agentdb_secret_ref.go:22-62`).
- **The effect.** Every agent database needs its own pre-set environment variable on the sidecar, so
  dynamic provisioning requires a restart to become monitorable.
- **RDS ARNs** fail the env pattern, so RDS deployments are never fleet-monitored
  (`cmd/pg_sage_sidecar/agentdb_secret_ref.go:25`).
- **TLS.** For non-hosted providers the default `sslmode` is `prefer`, which allows a downgrade
  (`cmd/pg_sage_sidecar/agentdb_secret_ref.go:53-55`).

### 3.7 Secret redaction (logs and API)

**What is redacted:**
- provider details, connection info, audit details and attempt details pass through
  `RedactProviderDetail`;
- keys are matched by substring against `password|token|secret|credential|…|session`
  (`internal/agentdb/provider_redaction.go:11-35,79-87`);
- URL credentials are masked;
- provider-config writes reject secret-named keys (`internal/agentdb/provider_policy.go:119-131`);
- ping tokens are redacted on list and validate;
- unknown errors map to a generic 500 (`internal/api/agent_db_handlers.go:308-350`).

**Over-redaction removes evidence (A-04, A-30).**
- Executing the copied helper confirms that `token_id`, `old_token_id`, `new_token_id`, `secret_ref` and
  `session_count` are all redacted.
- So the token audit events lose the token id, and `connection_info.secret_ref` is stored as
  `[redacted]` while the top-level column holds the ARN.

**What is not redacted:**
- `metadata` and size-profile `provider_params`, which come from operators and are returned to operators
  and agents;
- dry-run `stdout` in `ExecuteProvision` (`internal/agentdb/execution.go:94-104`; only planned arguments,
  low risk).

`docs/agent-db-deployments.md:348-351` embeds a local fixture admin credential. It is a test fixture, not
a product secret; the value is not reproduced here.

### 3.8 SQL injection

None found. Specifically:
- **DDL** uses `quoteIdent` on names sanitized to letters, digits and underscore
  (`internal/agentdb/schema.go:149-184`, `internal/agentdb/local_provisioning.go:49,70`).
- **The list query** uses constant column names and `$n` arguments (`internal/agentdb/list_page.go:84-111`).
- **The only concatenated fragment** is `setStatusFields(…, extra)`, and every caller passes `""`
  (`internal/agentdb/store.go:276-299`). It is a latent footgun and dead code.
- **Deploy-request `migration_sql` and `verification_sql`** are stored text that nothing executes.

### 3.9 SSRF

None from request inputs. Specifically:
- provider base URLs are hard-coded or come from the environment (`internal/agentdb/hosted_http.go:43-48`,
  `internal/agentdb/runtime_registry.go:59-79`, `internal/agentdb/credentials.go:84-90`);
- path segments are escaped with `url.PathEscape` (`internal/agentdb/lakebase_runner.go:227-248`,
  `internal/agentdb/gcp_cloudsql_runner.go:262-306`, `internal/agentdb/hosted_api.go:22-165`);
- `evidence_uri` and `archive_uri` are never fetched;
- Terraform zip import never touches the filesystem (`internal/agentdb/terraform_policy.go:105-139`).

### 3.10 Privileges of roles that AgentDB creates in target databases

**AgentDB creates no roles and no grants** (`grep` finds no `CREATE ROLE` or `GRANT`).
- **Local DDL** runs as the sidecar's control-pool role, which needs CREATEDB for the `database` level.
- **Without a meta-DB, that pool is the first monitored database** (`cmd/pg_sage_sidecar/wire.go:55-65`).
  The opt-in env var does not check for a meta-DB (`internal/agentdb/local_provisioning.go:16-18,85-110`)
  (A-17, PLAUSIBLE when misconfigured).
- **Fleet attach bootstraps pg_sage's whole `sage` schema inside the agent's database.** It uses the env
  DSN's role, which must be privileged (`cmd/pg_sage_sidecar/database_runtime.go:171-179`,
  `cmd/pg_sage_sidecar/metadb.go:30`).

### 3.11 Audit completeness (A-03, P1)

**Audited, with an actor:**
- request decisions (`request_decision.go:64-66`);
- request consumption (`request_provision.go:70-74`);
- agent-token mint (`agent_tokens.go:69-71`);
- deploy-request create and review (actor inside the row);
- restore drill (actor inside the attempt detail, `restore_drill.go:40-48`).

**Audited, with no actor:**
- register (`register.go:50`);
- extend_lease (`store.go:196-199`);
- archive, restore and every `setStatus` (`store.go:308`);
- delete (`store.go:247`);
- ping-token create, revoke and rotate (`identity.go:127,196,229`);
- `backup_*` (`operations.go:214-217`);
- recommendation_feedback (`operations.go:106-109`);
- provision preflight, execute and status (`execution.go:58,112,301`);
- destroy_live (`execution.go:228`).

**Not audited at all:**
- request creation (`store.go:17-44`);
- identity upsert (`identity.go:24-62`);
- size-profile upsert and delete (`profiles.go:26-130`);
- provider-config upsert (`provider_config.go:10-40`);
- blueprint and template create and approve (`blueprint.go:76-165`, `provision_links.go:21-45`,
  `terraform_templates.go:11-104`);
- recommendation upsert (`operations.go:12-44`);
- cost samples (`operations.go:112-138`);
- agent-API reads.

**Write-only events (A-05).** `agent_token_created` and `request_approved|denied` are written with
`deployment_id=''`. `AuditEvents` filters by deployment (`internal/agentdb/audit.go:9-33`) and the route
cannot address `''` (`internal/api/agent_db_handlers.go:51-57`).

**Errors are dropped.** More than 20 `_ = s.audit(...)` sites discard the error. Only
`auditOrWarn` (`internal/agentdb/request_decision.go:83-87`) logs it.

**The actor format is inconsistent.** Approvals use the email; live authorizations use `user:<id>`
(`internal/api/agent_db_agent_api.go:160-169`, `internal/api/agent_db_live_authority.go:435-441`).

### 3.12 Prompt injection

- **Blueprint LLM.**
  - Operator intent goes in; a typed `BlueprintSpec` comes out (`internal/api/agent_db_blueprint_handlers.go:116-163`).
  - The blast radius is bounded by several layers: policy findings, human approval, single-use request
    consumption, admin `authorize-live`, and the effective-policy allowlists and cost caps.
  - HCL escaping covers AWS, GCP and Lakebase, but hosted renderers still use `%q` (A-23). This is low
    impact because templates are never executed.
  - `raw_response` is stored and returned, but the UI does not render it, and there is no
    `dangerouslySetInnerHTML`.
- **New exposure: fleet attach** (PLAUSIBLE, P2 threat-model item).
  - Agent-controlled schema, query text and data in attached agent databases become inputs to pg_sage's
    LLM features (ask, RCA, analyzer narratives) and to an executor under global trust.
  - So untrusted agents can plant indirect prompt injection into the AI DBA's context.
  - The core's `policy.Gate` still bounds actions, but AgentDB widens the set of untrusted inputs without
    stating it.
- **`agent_instructions`** on recommendations is an operator-to-agent instruction channel.
  - Agents cannot read it today (no agent route).
  - Any operator can rewrite another deployment's instructions (A-07).

### 3.13 Abuse and DoS

**Unauthenticated agent-ping with random deployment ids (A-13).**
- Each request writes one failure row with no FK and one audit row
  (`internal/agentdb/identity.go:305-345`, `internal/agentdb/schema_statements.go:222-228,317-323`).
- The per-deployment cap is keyed by the caller-chosen id, so it does not bound a spray.
- The audit table is retention-exempt (`internal/retention/exemptions.go:115`).
- There is no per-IP limiter; the only limiter in the API is for login (`internal/api/login_limiter.go:9-20`).

**Agent-token validation** is one `UPDATE` per request with no failure throttle. Brute force is
infeasible, but it is DB load (P3).

---

## 4. Correctness review

### 4.1 Findings

| ID | Sev | Conf | path:line | Summary |
|---|---|---|---|---|
| A-01 | P1 | CONFIRMED | `internal/agentdb/agent_tokens.go:36-100`; `internal/api/agent_db_handlers.go:140-155` | Agent tokens cannot be listed or revoked. The kill switch is "disable identity": unaudited, and it leaves ping tokens and databases alive |
| A-02 | P1 | CONFIRMED | `internal/agentdb/restore_drill.go:24-59`; `web/src/pages/agentdb/agentDBLiveActions.js:51-58`; `web/src/pages/AgentDBsPage.jsx:592-606` | The restore-verified destroy gate is an admin's free-text URI. The backup need not exist; the UI fabricates `backup_id` and a fixed "operator verified restored data" check |
| A-03 | P1 | CONFIRMED | §3.11 | The audit trail cannot be attributed and is incomplete; write errors are discarded |
| A-04 | P2 | CONFIRMED (ran helper) | `internal/agentdb/identity.go:127-130,196-199,229-232`; `internal/agentdb/agent_tokens.go:69-71`; `internal/agentdb/operations.go:281-288`; `internal/agentdb/provider_redaction.go:79-87` | `token_id`, `old_token_id` and `new_token_id` are redacted in audit details, so the audit cannot say which token was minted, revoked or rotated |
| A-05 | P2 | CONFIRMED | `internal/agentdb/request_decision.go:64-66`; `internal/agentdb/agent_tokens.go:69`; `internal/agentdb/audit.go:9-33` | Request decisions and token mints are audited under `deployment_id=''`, which no endpoint can read |
| A-06 | P1 | CONFIRMED (code) / PLAUSIBLE (IAM) | §3.6 | No least-privilege credentials. The RDS master-secret ARN is exposed to every tenant agent; no roles are created anywhere |
| A-07 | P2 | CONFIRMED | `internal/agentdb/operations.go:12-44,427-447`; `internal/api/agent_db_handlers.go:479-500` | Recommendation upsert has no deployment scope. A POST to X with Y's `recommendation_id` rewrites Y's title, detail and `agent_instructions`, and returns Y's row. Not audited |
| A-08 | P2 | CONFIRMED | `internal/agentdb/deploy_requests.go:11-65,286-313`; test gap `internal/agentdb/deploy_requests_test.go:109-154` | Deploy-request upsert has no deployment or status scope: cross-deployment overwrite, content swap under a pending review, and the stale `reviewed_by` is kept |
| A-09 | P2 | CONFIRMED | `internal/agentdb/provision_links.go:21-45`; `internal/agentdb/terraform_templates.go:75-104`; `internal/agentdb/blueprint.go:132-163` | Blueprint and template approval is by id only. A regenerate between view and approve means approving unseen content (no hash or version in the approval) |
| A-10 | P2 | CONFIRMED | `internal/agentdb/blueprint.go:53-74,224-226`; `internal/api/agent_db_blueprint_handlers.go:35-44`; `internal/agentdb/provision_links.go:33-35`; `internal/agentdb/hosted_blueprint_test.go:59-61` | Neon and Supabase blueprints are always `rejected`: defaults set `PublicIP=true` against a zero `BlueprintPolicy` the handler never sets, so they can never be approved. The test codifies this |
| A-11 | P2 | CONFIRMED (ran helper) | `web/src/pages/agentdb/agentDBFormHelpers.js:20-50`; `internal/agentdb/request_provision.go:132-158`; `internal/agentdb/provision_links.go:213-224`; `internal/agentdb/schema.go:109-123`; `internal/agentdb/profiles.go:188-199`; `internal/agentdb/live_execution_issue.go:82-99`; `internal/agentdb/provider_policy.go:65-67,102-117` | The UI's region, account, project, workspace, Lakebase source and hosted parent/branch fields are discarded (see the scenario below) |
| A-12 | P2 | CONFIRMED | `internal/api/agent_db_live_authority.go:16,184-187`; `internal/agentdb/effective_policy.go:120-123`; `internal/agentdb/provider_config.go:28-38` | The provider policy layer goes "stale" 30 days after the last save, and all live authorizations fail. `last_validated_at` is never written. The TTL destroy path ignores policy, which is inconsistent |
| A-13 | P2 | CONFIRMED (path) / PLAUSIBLE (impact) | §3.13 | Unauthenticated write amplification into the retention-exempt audit table |
| A-14 | P2 | CONFIRMED | `internal/agentdb/policy.go:12-44`; `internal/api/agent_db_request_handlers.go:12-38`; `internal/agentdb/schema_statements.go:17-36` | Intake policy is advisory: any `budget_usd>0` auto-approves a cloud instance; classification and masking id are self-declared and unvalidated; region is checked only against the YAML list and not persisted |
| A-15 | P2 | CONFIRMED | `internal/agentdb/cost_guard.go:36-56`; `internal/agentdb/store.go:165-201`; `internal/api/agent_db_request_handlers.go:40-45` | Each extension is capped (YAML TTL only, not the persisted provider policy) and costed for the new lease only. Repeated extensions give unbounded lifetime and spend |
| A-16 | P2 | CONFIRMED | `internal/agentdb/policy.go:46-57`; `internal/agentdb/operations.go:112-138,290-305` | `hard_limit` means "pause" but only sets `status=budget_exceeded`: no provider action, no notification. Spend is operator-posted samples (no idempotency, no metering) |
| A-17 | P2 | CONFIRMED | `internal/agentdb/store.go:211-248`; `internal/agentdb/teardown_reconcile.go:89-91`; `internal/agentdb/local_provisioning.go:16-18,85-110`; `cmd/pg_sage_sidecar/wire.go:55-65` | Local schemas and databases are never dropped (delete is a tombstone; the reconciler skips local). With no meta-DB, local DDL lands on the first monitored database |
| A-18 | P2 | CONFIRMED | `web/src/pages/agentdb/AgentDBSections.jsx:455-498`; `web/src/pages/AgentDBsPage.jsx:230-240` | No approve/deny control for requests in the UI, and no UI for identities, agent tokens or ping tokens. A consumed request still shows "Provision" (status stays `approved`) |
| A-19 | P3 | CONFIRMED | `internal/agentdb/store.go:203-209,276-310` | Archive and Restore have no state guard: they resurrect `deleted`; Restore does not extend the lease, so the row re-archives within one pass; Restore clears teardown and mutation ids mid-destroy (converges later) |
| A-20 | P3 | CONFIRMED | `internal/agentdb/policy.go:79-86`; `internal/agentdb/execution.go:312-335`; `internal/agentdb/teardown_reconcile.go:133-158` | Planned or dry-run cloud deployments can never be deleted: delete needs `destroyed`, dry-run teardown ends at `destroy_dry_run_ready`, and it first demands a restore attestation for a database that never existed |
| A-21 | P3 | CONFIRMED | `internal/agentdb/store.go:29-43`; `internal/agentdb/queries.go:11-24` | A repeated body without an idempotency key (same derived `request_id`), or concurrent idempotent retries, return 500 (no `ON CONFLICT`) |
| A-22 | P3 | CONFIRMED | `internal/api/agent_db_handlers.go:369-376` (24 call sites); `internal/api/agent_db_provider_config_handlers.go:20-37`; `internal/api/agent_db_execution_handlers.go:31-37` | Lenient `readMap` turns malformed JSON into `{}`: an admin's malformed provider-config POST disables the provider and wipes its settings; a malformed execute runs a dry-run |
| A-23 | P3 | CONFIRMED | `internal/agentdb/hosted_blueprint.go:32-54` vs `internal/agentdb/blueprint.go:427-434` | Hosted Terraform uses `%q` for the LLM-derived region, so `${…}` is not escaped (the G8-B28 fix missed hosted). Templates are never executed by pg_sage |
| A-24 | P3 | PLAUSIBLE | `internal/api/agent_db_agent_api.go:74-81`; `internal/agentdb/list_page.go:91-98` | Fail-open tenant filter if a principal ever has an empty tenant |
| A-25 | P3 | PLAUSIBLE | `internal/agentdb/request_provision.go:163-182` | Deployment-id exclusivity across requests is a racy `NOT EXISTS` with no unique index: two approvals can bind one deployment (the second re-plans it) |
| A-26 | P3 | PLAUSIBLE | `internal/agentdb/execution.go:447-516`; `internal/agentdb/lifecycle_claim.go:30-46`; `internal/agentdb/teardown_mutation.go:32-48` | `updateProvisioningStatus` and `applyProvisionResult` read then write with no CAS and no mutation-lease fencing (lost updates; e.g. an operator status check writes `status_unknown` over `destroying`; reconcile converges). Raw-SQL writes bypass the transition table |
| A-27 | P3 | PLAUSIBLE (defense in depth) | `internal/agentdb/live_create.go:36-40`; `internal/agentdb/live_execution_contract.go:194-235`; `internal/agentdb/live_execution_store.go:211-243` | Live execute does not re-derive the plan from the current row; the runner reads current metadata. Today single-use request consumption prevents a re-plan of a request-created deployment |
| A-28 | P3 | CONFIRMED | `internal/agentdb/identity.go:100-110,236-269`; `internal/agentdb/agent_tokens.go:54` | Ping tokens have no maximum TTL and survive identity disable and tombstoning. Token ids are derived from a hash of the secret |
| A-29 | P3 (P1 for regulated buyers) | CONFIRMED | `internal/api/agent_db_live_authority.go:124-129`; `internal/agentdb/provider_policy.go:85-89` | No two-person rule; self-review cancels the low-confidence review flag |
| A-30 | P3 | CONFIRMED (ran helper) | `internal/agentdb/provider_redaction.go:11-35`; `internal/agentdb/execution.go:512` | Over-redaction: `connection_info.secret_ref` becomes `[redacted]`; detail or metric keys containing token, session or secret are erased |
| A-31 | P2 | CONFIRMED | `internal/agentdb/identity.go:40-60`; `internal/agentdb/store.go:17-44`; `internal/agentdb/register.go:78-97` | `agent_id` is global; identity upsert silently moves tenants, unaudited; human paths accept any tenant or agent pair |
| A-32 | P3 | CONFIRMED | `cmd/pg_sage_sidecar/agentdb_fleet.go:194-198`; `cmd/pg_sage_sidecar/mode_init.go:189-200`; `internal/agentdb/monitoring_schema.go:10-11` | Fleet-attached agent DBs run under the global default execution mode (fallback `auto`); the deployment's `execution_mode='manual'` column is ignored |

**Scenarios for the main findings**

**A-02: the restore gate.**
1. A live RDS deployment's lease expires.
2. The reconciler blocks it with "verified restore required" (`internal/agentdb/teardown_reconcile.go:119-123`).
3. To stop the spend, an admin clicks "Attest restore drill" and types any string, e.g. `n/a`.
4. The UI posts `backup_id: restore_drill_<ts>` and `checks: ['operator verified restored data']`.
5. `RecordRestoreDrill` creates a brand-new `restore_verified` backup row with no existing backup and no
   drill.
6. The next pass destroys the instance. No restore was ever attempted.

The control therefore either blocks all TTL teardown, which is a cost leak, or is rubber-stamped, which
is a data-loss path.

**A-07: recommendation overwrite.**
1. An operator POSTs `/api/v1/agent-dbs/X/recommendations` with
   `{"recommendation_id":"rec_<Y's>","kind":"k","title":"t","agent_instructions":{…}}`.
2. `ON CONFLICT (recommendation_id) DO UPDATE` rewrites Y's row, keeping `deployment_id=Y`, and returns
   it to the caller.
3. No audit row is written.

This is the same bug class as G8-B11 (backups), which was fixed only for backups.

**A-08: deploy-request swap.**
1. Reviewer R opens `dr_1` (`review_requested`, SQL v1).
2. Operator O re-POSTs `dr_1` with `status:"review_requested"` and SQL v2. This works through any
   deployment's route.
3. R clicks approve. `ReviewDeployRequest` checks only `status='review_requested'`
   (`internal/agentdb/deploy_requests.go:144-152`).
4. R has approved SQL v2, which R never saw.

Separately, re-posting an approved request resets it to draft while keeping `reviewed_by=R` and
`reviewed_at`.

**A-10: hosted blueprints.**
1. `NormalizeBlueprintSpec` sets `PublicIP=true` for neon and supabase (`internal/agentdb/blueprint.go:224-226`).
2. `BlueprintPolicyFindings` with the zero policy adds "public ip is denied by blueprint policy"
   (`internal/agentdb/blueprint.go:64-66`).
3. `CreateBlueprint` therefore marks the blueprint `rejected` (`:113-118`).
4. `ApproveBlueprint` refuses it because it has findings (`internal/agentdb/provision_links.go:33-35`).

No configuration path sets `BlueprintPolicy`.

**A-11: dropped UI fields.**
1. The UI puts its fields in `metadata.provider_params` (`web/src/pages/agentdb/agentDBFormHelpers.js:31-48`).
2. `registerFromRequest` merges `{"provider_params": req.ProviderParams}`, where the value is a typed nil
   map. `mergeMap` treats a typed nil as present, so the UI value is replaced
   (`internal/agentdb/request_provision.go:152-156`, `internal/agentdb/provision_links.go:213-224`). A
   copied-helper run confirmed this: `ui region survives mergeMap: false`.
3. `Store.Provision` then overwrites `provider_params` with the size profile's params anyway
   (`internal/agentdb/schema.go:121-123`). That precedence is deliberate: a trusted profile beats client
   metadata (`internal/agentdb/size_profile_readiness_wave3_test.go:56-71`).
4. The default `rds_instance_s` and `cloudsql_instance_s` profiles have no region
   (`internal/agentdb/profiles.go:188-197`).
5. So the live plan's region is `""`, and `allowedValue(regions, "")` is false unless the list contains
   `*`, giving "region is not allowlisted".

Net effect: the UI's region, account and project inputs do nothing, and the request API's
`provider_params` field (`internal/agentdb/request_types.go:78`) is dead.

**A-12: provider-policy staleness.**
1. An admin saves the provider config on day 0.
2. On day 31, `authorize-live` (create or destroy) returns 400, with the reason "provider policy
   validation is stale" in the server-side decision.
3. The only cure is to re-save the config. Nothing writes `last_validated_at`.
4. Meanwhile the autonomous TTL destroy still works, because `reconcileLiveTeardown` evaluates no live
   policy (`internal/agentdb/teardown_reconcile.go:99-131`).

### 4.2 Concurrency (claims and leases)

**Sound:**
- the expiry claim uses `FOR UPDATE SKIP LOCKED` plus CAS on `cleanup_claim_id` and `lifecycle_version`
  (`internal/agentdb/lifecycle_claim.go:14-46,130-141`);
- the archived-live revisit is bounded and rotates by `teardown_blocked_at`
  (`internal/agentdb/teardown_reconcile.go:14-33`);
- the provider mutation lease lasts 15 minutes (`internal/agentdb/teardown_mutation.go:55-99`);
- live reconcile is guarded by a session advisory lock (`internal/agentdb/lifecycle.go:30-43,164-188`);
- schema init uses an advisory transaction lock plus a version row (`internal/agentdb/schema.go:55-85`);
- request consumption is an atomic `UPDATE … WHERE consumed_deployment_id=''`
  (`internal/agentdb/request_provision.go:163-182`), with a race test at
  `internal/api/agent_db_request_d4_test.go:282`.

**Weak:**
- **A-25:** cross-request deployment-id exclusivity is racy.
- **A-26:** status writes are not fenced by the mutation lease.
- **A-09:** design approvals race with regeneration.
- **Monitoring claim code** (`internal/agentdb/monitoring_claims.go:46-86`) is careful (transaction
  advisory lock plus `SKIP LOCKED`) but has no caller.

### 4.3 Idempotency

| Operation | Behaviour | Evidence |
|---|---|---|
| Request create | Idempotency-Key scoped by tenant gives a replay, or `ErrConflict` on a different body. Without a key: 500 on repeat. Concurrent retries: 500 | `internal/agentdb/store.go:29-43` |
| Request provision | Single-use. A retry after success returns 409, not the created deployment; the client must GET the request to find `consumed_deployment_id` | `internal/agentdb/request_provision.go:43-48` |
| Register | Same-owner pre-live re-plan replaces; live or in-flight rows replay; another owner gets 409; tombstones get 409 forever | `internal/agentdb/queries.go:52-90`, `internal/agentdb/register.go:58-73` |
| `authorize-live` / execute | Idempotent on (deployment, operation, requester, key); an exact replay returns the receipt | `internal/api/agent_db_live_authority.go:67-96`, `internal/agentdb/live_execution_contract.go:194-220` |
| Cost samples | Not idempotent; retries double-count | `internal/agentdb/operations.go:112-138` |
| Agent and ping token mint | Each call mints a new token; agent tokens accumulate and cannot be enumerated (A-01) | – |
| Blueprint, template, deploy request, recommendation | Upsert by id overwrites (A-07..A-09) | – |

### 4.4 Error handling

- **Status codes:**
  - `ErrNotFound` from a missing provider config surfaces as "agent db deployment not found" on
    `authorize-live` (`internal/agentdb/provider_config.go:52-54` →
    `internal/api/agent_db_live_authority.go:168-171`);
  - FK violations on POSTs to unknown deployment ids (recommendations, cost samples, backups, operator
    ping) surface as 500, not 404 (`internal/agentdb/store.go:126-131`,
    `internal/agentdb/operations.go:126-133`).
- **`IsCloudProvider("foo")` is true** (`internal/agentdb/providers.go:85-87,456-458`). An invalid
  provider on `POST /agent-dbs` therefore returns 409 "approved request required" instead of 400.
- **Audit-write errors are swallowed** (§3.11). The claim-release failure is only logged
  (`internal/agentdb/request_provision.go:186-196`).
- **The cleanup reason is misleading.** "lease expired without a fresh ping"
  (`internal/agentdb/policy.go:66-69`) is stale, because pings never extend leases.

### 4.5 State-transition bugs (summary)

- A-19: Restore and Archive have no guard.
- A-20: dry-run deployments are undeletable.
- A-26: there are raw-SQL bypasses.
- The `budget_exceeded` state has no exit except Restore.
- The `status` after consumption stays `approved` (A-18).

### 4.6 Re-check of prior findings (G8, D4)

| Prior ID | Status on `72646ab1` | Evidence / note |
|---|---|---|
| G8-B01 (ping rewrites lifecycle) | FIXED | `internal/agentdb/store.go:118-163` |
| G8-B02 (archived never revisited) | FIXED | `internal/agentdb/teardown_reconcile.go:14-62,181-198` |
| G8-B03 (register upsert orphans) | FIXED | `internal/agentdb/queries.go:83-89`, `internal/agentdb/register.go:44-73` |
| G8-B04 (derived-name destroy) | FIXED (domain side) | `internal/agentdb/teardown_ownership.go:9-28`, `internal/agentdb/execution.go:176-199`; runner tags are for the deployment audit |
| G8-B05 (no tenant isolation) | FIXED for agents; humans global by D4a | §3.5 |
| G8-B06 (ambiguous create) | FIXED | `internal/agentdb/live_create.go:100-163`, `internal/agentdb/lifecycle.go:147-162` |
| G8-B08 (cost bypass) | PARTIAL | Unknown class fails closed, doubling applied, per-extension cap. Self-review and cumulative extension remain (A-15, A-29) |
| G8-B10 (policy bypasses) | FIXED / PARTIAL | Deny terminal, single use, spec wins, cloud register requires a request. The approval is still not bound to region or class (A-14) |
| G8-B11 (self-attested restore) | PARTIAL | Generic endpoint closed and upsert scoped. The attestation is still unverifiable (A-02) |
| G8-B12 (backup flag ineffective) | PARTIAL | Direct destroy follows policy. The TTL path uses the row flag, and the UI always sends `true` (`web/src/pages/AgentDBsPage.jsx:266`) |
| G8-B13 / B14 / B15 / B16 / B23 / B25 | FIXED | `web/src/pages/agentdb/agentDBLiveActions.js:26-47`; `internal/agentdb/credentials.go:19-30`; `internal/api/agent_db_provider_handlers.go:15-74`; `internal/agentdb/schema.go:20-85`; `internal/agentdb/lifecycle.go:147-162`; `internal/agentdb/list_page.go` |
| G8-B17 (local mutates monitored DB) | PARTIAL | Opt-in and no adoption. No roles, no DROP, no meta-DB check (A-17) |
| G8-B18 (no emergency stop) | PARTIAL | Store gate exists. The API store lacks the in-memory fleet gate (`internal/api/agent_db_agent_api.go:174-184` vs `cmd/pg_sage_sidecar/agentdb_reconciler.go:21-23`). Trust ramp never consulted |
| G8-B21 (fleet one-way) | PARTIAL | Desired-state sync and env refs resolved, full runtime attached. RDS ARN unresolved; docs stale |
| G8-B24 (ping row amplification) | PARTIAL | Per-deployment cap only (A-13) |
| G8-B26 (spoofable audit identity) | PARTIAL | Session actors on approvals and designs. Many events actorless; errors discarded (A-03) |
| G8-B28 (HCL injection) | PARTIAL | Hosted renderers missed (A-23) |
| G8-B29 (budget pause) | PARTIAL | Negative samples rejected. Still a label only (A-16) |
| G8-B30 (hygiene) | PARTIAL | 24 lenient bodies (A-22); duplicate create gives 500 (A-21); Restore resurrects (A-19); misleading reason; `TestEnsure*` now 4 hard-fail without a DB |
| G8-D03 (monitoring queue) | STILL UNWIRED | No caller of `ScheduleMonitoring` or `ClaimMonitoringWork` outside the package |
| G8-D06 / D08 / D09 | STILL PRESENT | `internal/agentdb/provider_runner.go:197-199`; pings write-only (retention added); `cmd/pg_sage_sidecar/agentdb_fleet.go:27-37,52-71` |
| G8-D11 (deploy requests) | STILL REVIEW-ONLY | `internal/agentdb/deploy_requests.go:216-228` (`review_only: true`) |
| G8-D13 (reconfigure owner) | No owner registered (restart required) | `internal/config/lifecycle.go:125`; no `RegisterOwner` for agentdb |
| D4a-A / D4b-B | BUILT | §3.5; audit-row readability gap (A-05) |

---

## 5. Test quality

### Test results

**Command 1:**
```
MSYS_NO_PATHCONV=1 docker run --rm --cpus=2 --label owner=agentdb-audit -v <repo>:/repo \
  ... golang:1.25 go test -count=1 -cover -v ./internal/agentdb
```
- No `SAGE_TEST_DATABASE_URL` was set; `testdb.Run` points the DSN at an unreachable port.
- **Total (top level):** 104 passed, 4 failed, 123 skipped. Subtests: 85 passed, 2 skipped.
- **Coverage:** `internal/agentdb` 36.7%. DB-backed tests skipped.

**Command 2:**
```
same image, go test -count=1 -v -run 'AgentDB|AgentAPI|AgentPrincipal|AgentPing|PreflightSurfaceAgentDB|Wave3AgentDB|D4|AgentToken|RequestAllowedRegions|Register' ./internal/api
```
- **Total:** 13 passed, 0 failed, 47 skipped. Coverage was not measured; the run was filtered.

**Skipped tests (justified by environment, not product):**
- 123 + 47 tests need Postgres: `requireAgentDB` skips at `internal/agentdb/store_test.go:22-41`.
- Live cloud tests are gated by `PG_SAGE_LIVE_AWS_RDS`, `PG_SAGE_LIVE_GCP_CLOUDSQL`,
  `PG_SAGE_LIVE_DATABRICKS_LAKEBASE`, `PG_SAGE_LIVE_AGENTDB_GAUNTLET` and `PG_SAGE_HOSTED_TEST_TOKEN`.
  CI never sets them (`.github/workflows/ci.yml`), so real-cloud paths are never exercised automatically.
- `TestWave3SizeProfilesReachDryRunAndNativeInputsIdentically` **passes with every subtest skipped**.
  This is a silent skip.

**Failures:** `TestEnsureIsMemoizedPerPool`, `TestEnsureConcurrentColdStores`,
`TestEnsureDatabaseLockCancellationAndRecovery` and `TestEnsureSeedFailureRollsBackSchemaAndReleasesLock`.
- The helper `freshEnsurePool` calls `t.Fatal` instead of skipping when the DB is missing
  (`internal/agentdb/schema_concurrency_test.go:16-38`).
- This is a harness bug, not a product bug. G8-B30 reported 3 such tests; there are now 4.

**Coverage gaps:** `internal/agentdb` is at 36.7% in this DB-less run, below the 70% business-logic
floor. The 2026-09-26 run with a DB recorded 74.0% (`reviews/2026-09-26/fixes-agentdb.md:103`). It was
not re-measured with a DB here, by rule.

**Bugs found this session:** A-01..A-32 (§4.1). None was found by a failing test.

**Manual checks remaining:** the UI was not exercised.

### What is tested and what is not

**Strong:**
- live-authority contracts (`internal/agentdb/live_execution_contract_wave3_test.go`, 11 tests);
- teardown and claim concurrency (`internal/agentdb/lifecycle_concurrency_test.go`, 13 tests);
- the D4 approval chain (`internal/agentdb/request_consumption_test.go`, 14;
  `internal/api/agent_db_request_d4_test.go`, 12);
- tenant isolation for agent tokens (`internal/api/agent_db_tenant_fix_test.go:77-198`);
- effective-policy merging (`internal/agentdb/effective_policy_wave3_test.go`).

**Missing, exactly where the bugs are:**
- cross-deployment recommendation upsert (A-07);
- deploy-request create on another deployment's id (A-08). The scope test covers only get and review
  (`internal/agentdb/deploy_requests_test.go:109-154`);
- approval content binding (A-09);
- hosted blueprint approval (A-10). The test asserts rejection (`internal/agentdb/hosted_blueprint_test.go:59-61`);
- UI provider-params end to end (A-11);
- policy staleness after 30 days (A-12);
- ping failures for nonexistent deployments (A-13);
- cumulative lease extension (A-15);
- local teardown (A-17);
- Restore of a tombstone (A-19);
- deleting a dry-run deployment (A-20);
- duplicate request create (A-21);
- audit content (token ids, actors, `''` events) (A-03..A-05). No test asserts on the
  `ping_token_created`, `agent_token_created` or `ping_token_rotated` details;
- identity tenant move (A-31).

### Fakes and helpers that hide the real wiring

- **API handler tests bypass production wiring.** The 54 handler tests in
  `internal/api/agent_db_handlers_test.go` use `agentDBSubrouter` or `registerAgentDBRoutes`.
  - These inject a fixed operator user and skip `RequireRole`.
  - They use `authority=nil` (no YAML policy, so no TTL cap or backup policy from config).
  - They use the dry-run runner registry (`internal/api/agent_db_router_helpers_test.go:14-44`).
  - Only 4 API tests use `registerAgentDBRoutesWithAuthority`.
- **The test generator is not the production generator.** Blueprint tests use
  `NewHeuristicBlueprintGenerator` (test-only, `internal/agentdb/heuristic_blueprint_generator_test.go`),
  while production requires the LLM generator.
- **Provider runners are tested against fake SDK and HTTP clients.** This is appropriate, but the live
  gauntlet tests pass a hand-built `LiveProvisionPolicy` and bypass `agentDBLiveAuthority`
  (`internal/agentdb/live_product_chain_test.go:13-39`).
- **`agentDBTestDSN` prefers `SAGE_DATABASE_URL`** (the production variable name) over
  `SAGE_TEST_DATABASE_URL` (`internal/agentdb/store_test.go:15-20`). The harness overwrites both, so it
  is safe under `go test`, but it is a footgun outside it.

---

## 6. Fit with the pg_sage core: reuse vs parallel machinery

| Core capability | Core implementation | AgentDB equivalent | Reused? |
|---|---|---|---|
| Action authorization | `policy.Gate` (`internal/policy/gate.go:20-80`) | `DecideRequest` (`policy.go:12-44`); `EvaluateLiveProvisionPolicy` (`provider_policy.go:45-94`); effective-policy resolver (`effective_policy.go`, 383 LOC); blueprint policy (`blueprint.go:53-74`); Terraform policy (`terraform_policy.go`) | **No** |
| Execution pipeline | `Executor.Apply` (authorize, lease, slot, re-authorize, execute, verify; `internal/executor/apply.go:88-130`) | live plan, estimate, authorization, claim and receipt (`live_execution_*.go`, about 1.3k LOC) plus the provider mutation lease (`teardown_mutation.go:55-99`) and teardown claims | **No** |
| Approvals | Operator approvals via the Gate; `internal/approvalcard`; MCP `approve` scope | request approve/deny; blueprint and template approve; deploy-request review; admin `authorize-live`; restore attestation (5 separate approval objects) | **No** |
| Trust ramp / earned autonomy | `internal/earned`, `internal/ledger`, Gate trust levels | `LiveModeAutoWithinPolicy` plus `AutoIssued`, which nothing sets (`live_execution_issue.go:22,73`) | **No** |
| Verification | `internal/verify` | None: status polls; restore is an attestation | **No** |
| Facts / evidence | `internal/facts` | None | **No** |
| Audit | `action_log`, `config_audit` | `agent_db_audit`, `agent_db_provision_attempts`, live receipts | **No** |
| Principals / tokens | `mcptoken` (`sage.mcp_tokens`: kinds, scopes, database scoping, revoke, list, prefix, lifetime bounds) | `agent_db_agent_tokens`, `agent_db_ping_tokens`, `agent_identities` (3 systems) | **No** |
| Emergency stop | `executor.ReadEmergencyStop` / `CheckEmergencyStop` (`internal/executor/emergency_stop.go:26-62`) | Own SQL with `bool_or`, where `42P01` means "not stopped" (`store_options.go:50-72`), plus the fleet gate | Partial |
| Schema migrations | `internal/schema` bootstrap | `Store.Ensure` plus `agent_db_schema_version` plus `init()`-appended statement lists | **No** |
| Secrets | `internal/crypto`; notify secret keys | env-only `secret_ref`; own redaction list | **No** |
| Schema-change review | MCP `apply_migration`, `lint_migration`, `request_change` → `ProductionIntentExecutor` with `policy.Gate` (`internal/mcp/production_intent_executor.go:15-60`); `internal/migration` risk and rehearsal | `deploy_requests.go` (review-only text, never executed) | **No** |
| Recommendations | analyzer, `internal/recommendation` | `agent_db_recommendations` (manual POST) plus static `tuning.go` hints | **No** |
| Notifications | notify, alerting, chatops, approval cards | None (no import; `grep` finds no agentdb references) | **No** |
| Retention | `internal/retention` | `internal/retention/agent_rules.go` | **Yes** |
| RBAC | `RequireRole` | used | **Yes** |
| LLM | `llm.Manager`, `llm.ParseJSON` | blueprint generator | **Yes** |
| Fleet runtime | fleet manager, `buildDatabaseRuntime` | `syncAgentDBsToFleet`; agent DBs get the full core runtime | **Yes** (the real integration) |

Consequences for the product principle ("every action goes through `policy.Gate` and `Executor.Apply`,
evidence-backed, verified, reversible"):
- No AgentDB mutation meets it. Cloud create and destroy, local DDL, lease changes and TTL teardown all
  run on AgentDB's own rails, with their own audit and their own (weaker) approval semantics.
- None of these mutations appears in the core ledger or in earned-autonomy accounting.

---

## 7. Dead code, over-engineering, LOC vs value, what is load-bearing

### 7.1 LOC by feature (non-test; `internal/agentdb` 13,689 LOC in 69 files; tests 10,753)

| Area | Files | LOC | Value today |
|---|---|---|---|
| Provider runners and readiness | aws, gcp, lakebase and hosted runners, `providers`, `provider_*`, `credentials`, `runtime_registry` | 3,719 | Real but never exercised against clouds in CI |
| Live authority (plan, estimate, authorization, receipts, effective policy, cost) | `live_*`, `effective_policy`, `provider_policy`, `cost_guard`, `provider_cost` | 2,192 | **Load-bearing**, good quality |
| Core registry, store and schema | `types`, `deployment_types`, `store*`, `register`, `queries`, `schema*`, `list_page`, `audit`, `profiles` | 2,343 | Load-bearing |
| Lifecycle and teardown | `lifecycle*`, `teardown_*`, `execution` | 1,374 | **Load-bearing** (TTL destroy) |
| Blueprints and Terraform | `blueprint`, `hosted_blueprint`, `terraform_*`, `provision_links` | 1,117 | Low: templates are review-only; hosted blueprints are unapprovable |
| Requests and policy | `request_*`, `policy` | 569 | Load-bearing (D4 single use); intake policy is weak |
| Identity and tokens | `identity`, `agent_tokens` | 562 | Load-bearing but incomplete (no revoke) |
| Monitoring queue | `monitoring_*` | 545 | **Dead** (no caller), plus about 576 test LOC |
| Ops: recommendations, cost, backups | `operations` | 466 | Low: manual inputs only |
| Deploy requests | `deploy_requests` | 313 | Low: review-only; duplicates the MCP migration flow |
| Backup and restore drill | `backup_assurance`, `restore_drill` | 268 | Gate exists but is attestation-only |
| Local provisioning | `local_provisioning` | 139 | Half-built (no roles, no drop) |
| Tuning hints | `tuning` | 82 | Static strings |
| API | `internal/api/agent_db_*.go` | 2,638 (+4,009 test) | – |
| cmd wiring | `agentdb_fleet.go`, `agentdb_secret_ref.go`, `agentdb_reconciler.go` | ~375 non-test | Fleet sync is the only core integration |
| UI | `AgentDBsPage.jsx` (809) plus `agentdb/*` | 3,660 | Missing approve/deny, identities, tokens |

### 7.2 Dead, unwired or semantically empty

**Unwired subsystems:**
- the monitoring queue: `internal/agentdb/monitoring_claims.go`, `monitoring_schedule.go`,
  `monitoring_schema.go` and three tables;
- `auto_within_policy` and `AutoIssued`: nothing auto-issues, so choosing `auto_within_policy` is
  *stricter* than `approval` while granting no autonomy (`internal/agentdb/live_execution_contract.go:283-290`).

**Dead code paths:**
- `queued`, `cancel_requested` and `cancelling` transitions, plus `ready` and `archived` as provisioning
  states (`internal/agentdb/provider_runner.go:194-199`, `internal/agentdb/execution.go:71`);
- the inline-credential fleet path (`cmd/pg_sage_sidecar/agentdb_fleet.go:27-37,52-71`). Runners never
  emit `host` or `password`, and redaction would strip a password;
- the duplicate agent-ping case in the operator subrouter (`internal/api/agent_db_handlers.go:231-232`).
  The specific mux pattern always wins;
- `setStatusFields(…, extra)` (`internal/agentdb/store.go:280-299`). `extra` is always empty.

**Dead or empty fields and columns:**
- `RequestProvisionRequest.ProviderParams` (`internal/agentdb/request_types.go:78`; overwritten, A-11);
- `Deployment.SecretRefExpiresAt` (no writer in the API);
- `provider_configs.last_validated_at` (never written);
- `safety_mode` (no semantics);
- deployment columns `execution_mode`, `monitoring_mode` and `wake_idle_allowed`.

**Write-only, review-only or static data:**
- `agent_db_pings` (write-only);
- deploy requests (never executed);
- Terraform templates (never executed; relabeled `review_only`,
  `internal/agentdb/provision_links.go:265-275`);
- tuning hints (static text).

**Unreachable or unused in practice:**
- hosted blueprint approval (A-10);
- `external` isolation: it can be approved but never provisioned (`internal/agentdb/policy.go:37-38`;
  `validLevel` rejects it);
- the UI's `approved_by`, `reviewed_by` and `created_by: 'operator'` fields, which the server ignores
  (`web/src/pages/AgentDBsPage.jsx:375,423,631`, `web/src/pages/agentdb/PromotionPanel.jsx:37`).

### 7.3 Over-engineering and wrong abstractions

- **The policy layers are not really three.** The "three-layer" effective policy has the runtime and
  global layers built from the same YAML object (`internal/api/agent_db_live_authority.go:172-183`), and
  the docs describe four layers (`docs/agent-db-deployments.md:245-251`).
- **Gates do not form a coherent model.** A cloud database needs up to 8 human steps:
  1. request approval;
  2. consumption;
  3. preflight;
  4. admin authorize;
  5. same-admin execute;
  6. restore attestation;
  7. destroy authorize or wait for TTL;
  8. tombstone delete.

  Yet none of them is bound to the actual spec (region and class come from a size profile chosen at
  provision time), and none is two-person.
- **The abstraction is wrong for agents.** The model is "deployment row plus provider instance". The
  agent primitives (copy-on-write branch per run, reset, scoped role with an expiring password, a
  per-run MCP connection) are absent or only provider parameters.
- **Two parallel schema-change review systems** exist: AgentDB deploy requests and MCP
  `apply_migration`. Only the latter goes through the Gate.

### 7.4 Load-bearing parts (keep candidates)

- the live authority (plan hash, estimate, single-use authorization, receipts) and ownership-verified
  destroy (`internal/agentdb/teardown_ownership.go`);
- the durable TTL teardown with claims and blocked reasons;
- single-use attributed requests (D4);
- the tenant-bound agent principal (needs revoke and scopes);
- fleet sync into the core runtime (the one place AgentDB is an AI DBA feature);
- the cost estimator with fail-closed unknown pricing.

---

## 8. API, MCP and UI surface

### 8.1 REST endpoints

Defined in `internal/api/agent_db_handlers.go:16-278` and `internal/api/agent_db_agent_api.go:56-70`.

| Method & path | Auth | Purpose |
|---|---|---|
| `POST /api/v1/agent-dbs/{id}/agent-ping` | **none (ping token)** | heartbeat (`agent_db_identity_handlers.go:101-120`) |
| `GET /api/v1/agent-api/agent-dbs` | agent token | list tenant deployments |
| `GET /api/v1/agent-api/agent-dbs/{id}` | agent token | read tenant deployment |
| `POST /api/v1/agent-api/agent-db-requests` | agent token | create request (tenant/agent from token) |
| `GET /api/v1/agent-api/agent-db-requests` | agent token | list tenant requests |
| `GET /api/v1/agent-dbs` | operator+ | paginated list (any tenant) |
| `POST /api/v1/agent-dbs` | operator+ (cloud requires `request_id`) | register/provision |
| `POST /api/v1/agent-dbs/cleanup` | operator+ | archive expired (no teardown) |
| `POST /api/v1/agent-dbs/reconcile` | operator+ | run TTL pass now (**can live-destroy**) |
| `POST/GET /api/v1/agent-dbs/requests`, `GET …/requests/{id}` | operator+ | requests |
| `POST …/requests/{id}/approve`, `…/deny`, `…/provision` | operator+ (actor required) | decide / consume |
| `GET /api/v1/agent-dbs/providers`, `GET …/provider-configs` | operator+ | readiness / policy read |
| `POST …/provider-configs/{provider}` | **admin** | provider policy upsert |
| `GET/POST …/terraform-templates`, `POST …/{id}/approve`, `…/{id}/provision` | operator+ | templates (review-only) |
| `GET/POST …/blueprints`, `POST …/{id}/approve`, `…/{id}/provision` | operator+ (LLM required) | blueprints |
| `GET/POST …/identities` | operator+ | identity upsert/list (unaudited) |
| `POST …/identities/{agent_id}/tokens` | **admin** | mint agent token |
| `GET/POST …/size-profiles`, `DELETE …/size-profiles/{id}` | operator+ | profiles (trusted live params) |
| `GET /api/v1/agent-dbs/{id}` | operator+ | read |
| `POST …/{id}/ping`, `…/{id}/extend-lease` | operator+ | operator heartbeat; lease |
| `GET/POST …/{id}/recommendations`, `POST …/{id}/recommendations/{rec}/feedback` | operator+ | recommendations (A-07) |
| `GET …/{id}/audit`, `GET …/{id}/audit/export` | operator+ | audit (per deployment only) |
| `GET/POST …/{id}/deploy-requests`, `GET …/{dr}`, `POST …/{dr}/request-review`, `…/approve`, `…/deny` | operator+ | deploy requests (review-only) |
| `GET/POST …/{id}/ping-tokens`, `POST …/{t}/rotate`, `…/{t}/revoke` | operator+ | ping tokens |
| `POST …/{id}/cost-samples`, `GET …/{id}/cost` | operator+ | spend |
| `GET/POST …/{id}/backups`, `POST …/{id}/backups/check`, `…/restore-drill-dry-run` | operator+ | backups |
| `POST …/{id}/backups/restore-drill` | **admin** | restore attestation (A-02) |
| `GET …/{id}/tuning-hints` | operator+ | static hints |
| `POST …/{id}/provision/preflight`, `…/execute`, `…/status`, `…/destroy-dry-run`, `…/destroy-live`, `GET …/attempts` | operator+ (live execute/destroy bound to the authorizing admin) | execution |
| `POST …/{id}/provision/authorize-live` | **admin** | live authorization |
| `GET …/{id}/cleanup`, `POST …/{id}/archive`, `…/restore`, `DELETE …/{id}` | operator+ | lifecycle |

That is about 60 endpoints. There is no `agentdb.enabled` switch: the routes are always registered when
a control pool exists (`internal/api/router.go:194-223`). The reconciler always runs every 300 s
(`internal/config/config.go:1035-1041`, `cmd/pg_sage_sidecar/api_server.go:120-128`).

### 8.2 MCP

**There are no AgentDB tools.** `grep -i agentdb` over `internal/mcp`, `internal/mcptoken`,
`internal/ask`, `internal/agentloop` and `internal/agenttools` returns nothing.

MCP's agent surface (`list_databases`, `request_change`, `apply_migration`, `lint_migration`,
`explain_query`, `optimize_query` and others) authenticates with `mcptoken`, a different principal from
AgentDB agent tokens.

### 8.3 UI

There is one page, `#/agent-dbs` (`web/src/App.jsx:203-204`, `web/src/components/Layout.jsx:40-41`).
- **Tabs:** Deployments, Provision, Profiles, Provider Settings, Terraform, Blueprints, Activity
  (`web/src/pages/agentdb/AgentDBWorkspaceTabs.jsx:5-13`).
- **Deployment detail:** cost, backups, attestation, promotion (deploy requests), recommendations, audit.
- **Missing:**
  - request approve/deny;
  - identities, agent tokens and ping tokens;
  - tenant scoping;
  - blocked-teardown, orphan and spend views (A-18).

### 8.4 Configuration and environment

- **YAML:** `agentdb.live_provisioning_enabled`, `allow_public_ip`, `require_backup_before_destroy`,
  `reconcile_interval_seconds`, and `providers.<p>.{enabled, allowed_regions|accounts|projects|workspaces,
  max_ttl_seconds, max_estimated_cost_usd}` (`internal/config/config.go:144-160`).
- **Environment:**
  - runner enablement: `PG_SAGE_LIVE_PROVISIONING`, `PG_SAGE_ENABLE_{AWS_RDS,GCP_CLOUDSQL,LAKEBASE,NEON,SUPABASE}_RUNNER`;
  - provider credentials and regions: `PG_SAGE_AWS_REGION`, `PG_SAGE_GCP_*`, `PG_SAGE_DATABRICKS_*`,
    `PG_SAGE_NEON_API_KEY`, `PG_SAGE_SUPABASE_ACCESS_TOKEN`, `PG_SAGE_SUPABASE_DATABASE_PASSWORD` (HMAC
    master);
  - local DDL: `PG_SAGE_AGENTDB_LOCAL_PROVISIONING`;
  - per-database DSNs: `PG_SAGE_AGENTDB_<NAME>`.

  Sources: `internal/agentdb/runtime_registry.go:10-85`, `internal/agentdb/hosted_plans.go:38-54`,
  `internal/agentdb/local_provisioning.go:16-18`.

---

## Appendix A: documentation drift

| Doc | Claim | Code |
|---|---|---|
| `docs/agent-db-deployments.md:314-319` | Fleet path "registers … inline connection credentials"; `secret_ref` deployments are skipped; analyzer and executor not attached | Env `secret_ref`s are resolved, and the full runtime (monitoring, execution, facts, SRE, ask) is attached (`cmd/pg_sage_sidecar/agentdb_fleet.go:176-206`, `cmd/pg_sage_sidecar/database_runtime.go:119-147`) |
| `docs/agent-db-deployments.md:30` | Pings drive abandoned-database cleanup | Pings never touch leases (`internal/agentdb/store.go:133-141`) |
| `docs/agent-db-deployments.md:131-132` | Request example sends `allowed_regions` | Ignored (`internal/api/agent_db_request_handlers.go:9-12`) |
| `docs/agent-db-deployments.md:245-251` | Four-layer effective policy | Runtime and global layers are the same object |
| `docs/agent-db-deployments.md:291-293` | Recommendations "an agent can consume" | No agent route; operator session only |
| `docs/reverse_spec/05-agentdb.md:169-185` | Heuristic generator is LIVE, with an LLM fallback to heuristics | Heuristic is test-only; the LLM is required (`internal/agentdb/blueprint.go:88-90`) |
| `docs/reverse_spec/05-agentdb.md:296-315` | The ping updates `status`; `secret_ref` deployments are skipped; analyzer and executor not attached | `agent_status` only; env refs resolved; full runtime attached |
| `docs/reverse_spec/05-agentdb.md:349-351` | "full append-only `agent_db_audit` trail on every state change" | Not every change is audited, and most events lack an actor (§3.11) |
| `internal/retention/agent_rules.go:5-9` | "the 18 tables" | 27 tables |

## Appendix B: method

- Read in full:
  - `internal/agentdb` core files: `types`, `deployment_types`, `request_types`, `store*`, `register`,
    `queries`, `schema*`, `safety_schema`, `policy`, `provider_policy`, `effective_policy`, `blueprint`,
    `hosted_blueprint`, `profiles`, `identity`, `agent_tokens`, `credentials`, `audit`, `cost_guard`,
    `backup_assurance`, `restore_drill`, `monitoring_*`, `tuning`, `operations`, `runtime_registry`,
    `list_page`, `request_*`, `provision_links`, `local_provisioning`, `deploy_requests`;
  - the lifecycle, teardown and live-create files;
  - every `internal/api/agent_db_*.go` non-test file;
  - the cmd wiring;
  - the UI page and helpers;
  - `docs/agent-db-deployments.md`;
  - the relevant reverse-spec sections;
  - D4, G8 and the fix report.
- Prior findings were re-verified against `72646ab1`.
- Two semantic checks were executed by copying `mergeMap` and `sensitiveKey` verbatim into a scratch
  program run in `golang:1.25`. The output is cited in A-04, A-11 and A-30.
- No repository file other than this report was modified. No database or cloud was contacted.
