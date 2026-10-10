# Agent Database Deployment Module Spec

Date: 2026-05-07

## Thesis

Agents are becoming database deployers, not just database users. They will ask
for temporary schemas, sandbox databases, vector stores, spatial workspaces,
JSON-heavy stores, and preview environments. pg_sage should become the DBA layer
that provisions, tunes, tracks, budgets, backs up, and cleans up those
agent-created databases.

This should not turn pg_sage into a full AgentDB product. The AgentDB drafts are
strong, but pg_sage should pull in only the parts that reinforce its autonomous
DBA mission:

- agent identity and workload attribution
- deployment requests
- workspace lifecycle
- capability and approval gates
- object and DDL audit
- cost tracking
- TTL/heartbeat cleanup
- promotion/deploy-request workflow
- machine-readable query tuning recommendations

Leave broad agent memory, full framework SDKs, general tool-call tracing,
general MCP gateway behavior, HNSW experiment/autotuning orchestration, and
complete agent control-plane ownership to a separate AgentDB product unless they
directly improve database safety.

## Product Name

Working name: **Agent Database Deployments**.

Short UI label: **Agent DBs**.

## Mission Fit

pg_sage's loop remains:

Observe -> Diagnose -> Decide -> Act -> Verify -> Remember

For agent-created databases:

- Observe: deployment request, workload, storage, query patterns, heartbeats.
- Diagnose: vector/PostGIS/JSON/config/index/query issues.
- Decide: tune, warn, archive, back up, clean up, promote, or block.
- Act: provision, configure, enqueue safe tuning, expire leases, run backup.
- Verify: health checks, query latency, recall, cost, successful restore point.
- Remember: deployment ledger, audit events, recommendations, cost history.

Provisioning is creation work and must be policy-gated. It should not become
autonomous merely because an existing database has moved up the trust ramp for
tuning actions.

## Explicit Scope Decisions

- **In pg_sage:** database/schema deployment, lifecycle, tuning hints, query
  recommendations, cost, backup, cleanup, provider capability checks, DDL safety,
  audit, and promotion workflow.
- **Out of pg_sage:** full agent memory product, full run trace product, generic
  tool-call ledger, broad MCP server, agent framework SDKs, HNSW experiment
  planner/autotuner product, and hosted branch provider.
- **Shared boundary:** pg_sage can accept `agent_id`, `run_id`, trace links, and
  external cost samples from agent systems, but it should not be the
  authoritative source for every agent event unless the event directly touches a
  database deployment.

## Why Now

The market already validates adjacent pieces:

- Neon branches support isolated development/testing workflows, API/CLI/GitHub
  creation, branch TTLs, and restore windows.
- Supabase branches create separate preview environments with their own
  credentials, automatic pause/delete behavior for preview branches, and a merge
  workflow.
- LangGraph and OpenAI Agents SDK show that agents increasingly need persistent
  state, tracing, resumability, and external trace processors.
- Agentic security guidance emphasizes least privilege, strong observability,
  approval gates, provenance, and scoped tools.

pg_sage's angle is different: it is not selling branches or traces. It is the
operator that keeps agent-deployed databases safe, tuned, paid for, and cleaned
up.

## Agent Identity And Guard

Every deployment request must identify:

- tenant/org id
- agent id
- owner id/team
- purpose
- run id or task id
- requested isolation type
- data sensitivity
- budget policy
- TTL/heartbeat policy

Authoritative identity comes from an agent API token, signed request, or human
session. `application_name` and SQL comments are correlation hints, not
authorization primitives.

The existing Agent Database Guard is a read-only classifier that answers: "is
this database workload attributable to a known agent or is it anonymous or
ephemeral?" It is advisory-only.

Current behavior:

- `agent:<id>` in `application_name` is treated as stable attribution evidence.
- `agent_id=<id>` in a SQL comment is treated as stable attribution evidence.
- empty `application_name` or no stable identifier becomes `unattributed`.
- `tmp`, `sandbox`, or temporary-table creation signals become `ephemeral`.
- attributed workloads stay quiet.
- unattributed/ephemeral workloads create `agent_workload` findings with no SQL
  and no action risk.

Future behavior:

- Correlate pg_sage findings, query stats, actions, audit, and cost samples by
  `tenant_id`, `agent_id`, `run_id`, and `deployment_id`.
- Exclude pg_sage-owned workspaces from generic ephemeral warnings by matching
  pg_sage-issued deployment tags.
- Detect agent workload anti-patterns: missing identity, generated object
  sprawl, unbounded JSONB/event tables, vector tables without embedding model
  metadata, mixed-SRID PostGIS data, high regeneration churn, and abandoned but
  writable workspaces.
- Escalate from info to warning/critical only when there is cost, safety, or
  production-blast-radius evidence.

Non-goal: this guard is not an agent memory vault or full tracing product. It is
the attribution layer pg_sage needs before it can safely tune, archive, clean up,
or promote agent-created database environments.

## Deployment Request API

Agents and humans request databases through the same policy path.

Minimum endpoints:

- `POST /api/v1/agent-dbs/requests`
- `GET /api/v1/agent-dbs/requests`
- `GET /api/v1/agent-dbs/requests/{request_id}`
- `POST /api/v1/agent-dbs/requests/{request_id}/approve`
- `POST /api/v1/agent-dbs/requests/{request_id}/deny`
- `GET /api/v1/agent-dbs`
- `GET /api/v1/agent-dbs/{deployment_id}`
- `POST /api/v1/agent-dbs/{deployment_id}/ping`
- `POST /api/v1/agent-dbs/{deployment_id}/extend-lease`
- `GET /api/v1/agent-dbs/{deployment_id}/recommendations`
- `POST /api/v1/agent-dbs/{deployment_id}/recommendations/{recommendation_id}/feedback`
- `POST /api/v1/agent-dbs/{deployment_id}/cost-samples`
- `POST /api/v1/agent-dbs/{deployment_id}/archive`
- `POST /api/v1/agent-dbs/{deployment_id}/restore`
- `DELETE /api/v1/agent-dbs/{deployment_id}`

Request decisions:

- `allow`: safe schema registration/provisioning request, within policy.
- `review`: requires human approval before provisioning.
- `deny`: impossible or violates hard policy.
- `defer`: provider capability, budget, backup, or dependency is not ready.

Idempotency rules:

- scope keys by `(tenant_id, idempotency_key)`
- retain keys for 24 hours by default
- same key and same body returns the original response
- same key with different body returns `409 conflict`
- missing idempotency key on mutating agent endpoints returns `400`

All mutating endpoints require an agent token or human session with a role that
can act on the relevant tenant and database. Agents can request, ping, extend
lease when policy allows it, submit cost samples, and report recommendation
feedback. Humans or higher-trust service accounts approve, archive, restore, and
delete.

Ping authentication is deployment-bound. A ping token can refresh only its own
deployment lease and cannot list, approve, mutate, archive, restore, or delete
anything else. Ping endpoints are rate-limited per token.

## Provisioning Modes

Start with the safest mode and design for adapters:

1. `schema`: schema-per-agent-workspace inside a managed database.
2. `external`: already-created database registered for tracking.
3. `database`: separate database on the same Postgres cluster.
4. `branch`: provider branch/clone, where available.

v1 implements `schema` and `external`. `database` and `branch` are adapter
interfaces that return `defer` until the adapter proves it can create, tag, back
up, and clean up that resource.

Guardrail matrix:

| Mode | v1 behavior | Credentials | Cleanup authority | Backup authority | Notes |
|---|---|---|---|---|---|
| `schema` | can provision when policy allows | pg_sage issues scoped role | pg_sage can quarantine/archive/drop only tagged objects | pg_sage can run schema export | default v1 path |
| `external` | registration/tracking only | supplied by operator | no destructive cleanup unless explicitly delegated | observe or trigger configured backup only | low-trust mode |
| `database` | adapter stub returns `defer` | adapter required | adapter required | adapter required | v1.x after schema mode |
| `branch` | adapter stub returns `defer` | provider adapter required | provider adapter required | provider-native snapshot preferred | v1.x/provider-specific |

## Bootstrap And Guardrails

On schema provisioning, pg_sage should:

- create scoped role/credential with deterministic deployment-bound naming
- set `application_name` requirements
- create schema/database tags in pg_sage metadata
- set TTL and heartbeat lease
- apply DDL allowlist
- register backup/archive policy
- run provider capability checks
- run extension tuning preflight
- run baseline query/statistics collection

Default v1 limits:

- schema workspaces only for autonomous provisioning
- no production data copy unless a masking policy is attached
- no extension install/update from an agent request
- no unsafe DDL outside the workspace schema
- no permanent credential without human approval
- max TTL defaults to 72 hours unless policy overrides it
- missing ping marks stale but does not drop data immediately
- provisioning failures create `provisioning_failed` deployments with cleanup
  tasks for any half-created schemas, roles, or grants

Role cleanup must handle cluster-global Postgres roles deliberately: revoke
login, revoke grants, terminate or drain active sessions according to policy,
`REASSIGN OWNED` / `DROP OWNED` where appropriate, and drop the role only after
owned objects are gone.

## Tuning Packs To Keep

The following tuning/hint packs are core to agent DB deployments:

- **Vector/pgvector:** vector table/index inventory, query-time `ef_search`
  hints, filtered ANN diagnosis, partial indexes, quantization/halfvec hints,
  index build cost estimates, and recall-measurement hooks. HNSW experiment
  planning/autotuning is a separate product and should not live in pg_sage.
- **PostGIS:** GiST/SP-GiST index hints, `ST_DWithin` rewrite hints,
  `ST_Transform` expression index hints, KNN query checks, SRID/mixed-type
  diagnostics, geography-vs-geometry guidance.
- **JSON/JSONB:** containment/key-existence/scalar extraction classifiers,
  `jsonb_path_ops` vs `jsonb_ops`, expression indexes, generated columns, and
  schema-promotion hints when JSON becomes stable.
- **Extension config:** `pg_stat_statements`, `pg_trgm`, `pgvector`, PostGIS,
  `auto_explain`, `pgaudit`, and `pg_hint_plan` configuration/preload/status
  checks.

This replaces the idea of an extension dashboard. Extensions should be tuned and
explained in context, not merely inventoried.

## Machine-Readable Query Tuning For Agents

Agents need tuning recommendations they can act on without scraping UI text.

Recommendations include:

- recommendation id
- tenant, agent, run, and deployment scope
- affected query fingerprint
- severity and confidence
- finding category
- human explanation
- parse-aware patch suggestion when available
- suggested SQL rewrite when available
- suggested index/config/extension hint when available
- validation guidance
- risk tier
- whether human approval is required
- expiration and supersession state
- safe application mode: `read`, `patch_suggested`, `ddl_request`, or
  `config_request`

`query_fingerprint` is pg_sage's normalized SQL hash by default. When
`pg_stat_statements.queryid` is available, it is included as a separate field,
not used as the cross-agent identity key.

Example response shape:

```json
{
  "id": "rec_123",
  "type": "query_rewrite",
  "scope": {
    "tenant_id": "tenant_acme",
    "agent_id": "agent_support_refunds",
    "deployment_id": "adb_456"
  },
  "query_fingerprint": "sha256:...",
  "pg_stat_statements_queryid": "123456789",
  "risk_tier": "safe",
  "confidence": 0.82,
  "summary": "Use ST_DWithin so the spatial index can be used.",
  "application_mode": "patch_suggested",
  "patch_hint": {
    "language": "sql",
    "kind": "parse_aware_suggestion",
    "before_pattern": "ST_Distance(geom, $1) < $2",
    "after_pattern": "ST_DWithin(geom, $1, $2)"
  },
  "validation": {
    "method": "manual_explain_compare",
    "notes": "Run on a staging or agent workspace dataset before changing production code."
  }
}
```

Recommendation semantics:

- `read`: information only, no agent action expected.
- `patch_suggested`: agent may update application SQL, but pg_sage does not
  mutate source code itself.
- `ddl_request`: agent may request an index or schema change; pg_sage routes it
  through trust, maintenance-window, and approval policy.
- `config_request`: agent may request extension/config changes; pg_sage treats
  these as reviewed operations because they may require restart or raise
  observability/security costs. Restart-required GUCs are denied at the API
  layer unless a human creates a reviewed maintenance request.

Patch hints are parse-aware suggestions, not blind string replacement recipes.
The agent must verify the target query fingerprint and parse tree before
applying a code change.

Supersession is automatic when a newer active recommendation is published for
the same `(deployment_id, query_fingerprint, category)`. Agent feedback can also
mark a recommendation superseded explicitly.

Agent feedback states:

- `accepted`
- `applied`
- `failed`
- `rejected`
- `superseded`

## Heartbeat, Lease, Cleanup

Agent DBs must ping pg_sage. No ping means the database is abandoned until
proved otherwise.

Lease states:

- `active`: ping is fresh
- `stale`: missed grace window
- `quarantined`: credentials disabled or writes blocked
- `archiving`: backup/export in progress
- `archived`: data retained, compute/schema removed or disabled
- `restore_verified`: archive was restored and checked
- `dropping`: destructive cleanup in progress
- `dropped`: deleted after retention

Default timing:

- `heartbeat_interval`: 15 minutes
- `stale_after`: 2 missed heartbeats
- `quarantine_after`: 24 hours after entering `stale`
- `archive_after`: 48 hours after entering `stale`, or immediately when a
  non-renewable TTL expires
- `archive_retention`: 14 days by default
- `drop_after`: archive retention expires and restore artifact is verified

Ping refreshes `last_ping_at`; it does not automatically extend `expires_at`.
Long-running workloads must request a lease extension, capped by policy max TTL.
Lease extension is a separate reviewed or policy-allowed transition.

Cleanup must never directly drop an agent DB on first missed ping. It should:

1. mark stale
2. notify owner/agent
3. disable credentials or set read-only when possible
4. create backup/archive
5. verify restore artifact
6. drop or detach after retention

Auto-drop is opt-in per deployment. The default cleanup mode is dry-run through
archive verification. Drop operations require a hard ownership match:

- deployment exists in `sage.agent_db_deployments`
- object carries pg_sage-issued ownership tag
- deployment id and tenant id match
- emergency stop is not active
- fleet drop-rate limit is not exceeded

For `external` mode, pg_sage marks stale and notifies by default. It cannot
disable credentials, archive, or drop unless the operator explicitly delegated
those capabilities.

## Backups And Restore

pg_sage should own backup readiness and restore verification policy, not become
a general backup engine. The module should prove that a deployment can be
recovered before it allows destructive cleanup.

Required capabilities:

- backup policy per deployment
- backup mode: `provider_managed`, `self_managed_external`, `pg_sage_schema_archive`, or `none_dev_only`
- daily backup readiness check
- backup before destructive cleanup
- restore-point metadata
- restore verification status
- archive size and cost
- last successful backup
- restore API for archived deployments

Managed Postgres model:

- pg_sage checks provider backup/PITR readiness daily.
- Readiness includes retention window, latest restorable time, encryption,
  region/residency, deletion protection when supported, and restore permissions.
- Restore drills are scheduled or risk-triggered, not daily by default.
- Provider restores usually create a new instance, server, or branch. pg_sage
  should model that explicitly instead of pretending it can restore a single
  schema in place.
- For agent schema cleanup on managed Postgres, pg_sage still creates a
  workspace-level archive because full-instance restore is too coarse for
  recovering one abandoned agent workspace.

Self-managed Postgres model:

- pg_sage should integrate with pgBackRest, WAL-G, Barman, or an operator-supplied
  backup command rather than reinvent physical backup/WAL archival.
- Readiness includes last successful base backup, WAL archive health, archive
  lag, retention, encryption/storage target, restore command availability, and
  failed backup jobs.
- pg_sage may run or schedule restore verification into a scratch instance or
  namespace.
- For schema-mode agent DBs, pg_sage can directly run and verify schema exports
  before cleanup.

Schema archive model:

- v1 can use `pg_dump --schema` or logical export for schema workspaces.
- Archive metadata must include manifest, checksum, source database, tenant,
  deployment id, created-at, encryption/storage details, and restore status.

Restore is required before destructive cleanup is trusted. For schema mode,
"verified backup" means restore into a throwaway schema plus manifest/checksum
comparison. For provider snapshot/branch mode, it means provider restore status
plus a pg_sage health query against the restored target. "File exists" is not a
valid backup verification.

Backups must be encrypted at rest. Customer-managed encryption keys can wait for
v1.x, but the storage location, encryption mechanism, and retention period must
be recorded for every backup.

Default cadence:

- readiness check: daily
- restore drill: scheduled, risk-triggered, or required before destructive
  cleanup
- destructive cleanup: blocked unless a verified restore artifact exists

## Cost Tracking

Track costs even when exact cloud billing is unavailable.

Database-observable signals:

- database/schema size
- index size
- dead tuples/bloat
- WAL generated estimate
- query total time and calls
- connection count
- vector index memory estimate
- backup/archive size
- age since last ping
- provider/project/database labels

Optional external cost ingestion:

- model tokens
- embedding calls
- external tool spend
- storage class cost estimate

These optional signals are not inferred by pg_sage. They require explicit
submission through `POST /api/v1/agent-dbs/{deployment_id}/cost-samples`.

Enterprise cost features:

- hard and soft budget limits per agent/team
- cost anomaly findings
- automatic read-only quarantine when a deployment exceeds hard spend or storage
  limits
- soft limit breaches create review findings
- chargeback labels for org/team/product/environment
- forecasted archive cost before cleanup
- top-N expensive agent DBs and queries

## Promotion Flow

Agents will create useful schemas. pg_sage should eventually let humans promote
them safely. This is not required for the first shippable module if provisioning,
cleanup, backup, and recommendation APIs are not yet boring and well-tested.

Promotion output:

- schema/object diff
- generated migration SQL
- rollback or forward-fix plan
- data movement plan
- index/tuning recommendations
- security/RLS review
- sample query validation
- cost estimate
- deploy-request record

Promotion gates:

- owner approval
- data sensitivity review
- schema lint pass
- migration safety preflight
- production object name conflict check
- query plan/cost comparison where representative queries exist
- backup/restore point before apply
- post-apply verification and rollback/forward-fix decision

Promotion should be a reviewed deploy request, never silent production mutation.
HIGH-risk production mutation inherits the existing manual confirmation gate.

## UI

Add an **Agent DBs** surface:

- provision button for humans
- request queue
- active deployments table
- stale/archived/deleted lifecycle views
- cost by agent/team
- last ping and lease state
- backup/restore status
- recommendations tab
- objects and DDL audit tab
- promotion/deploy-request tab when promotion ships

Do not create a separate extension dashboard. Extension state appears inside:

- deployment readiness
- tuning recommendations
- provider capability details
- blocked-action explanations

## Enterprise Requirements Beyond MVP

Enterprises deploying agents will also need:

- **Identity federation:** SSO/SAML/OIDC for human users and workload identity
  federation for agents/service accounts.
- **Secrets integration:** Vault, cloud secrets managers, short-lived
  credentials, rotation, revocation, and no credential return after initial
  creation.
- **Data classification and masking:** block production-data seeding unless
  masking/tokenization policy is attached and audited.
- **Egress controls:** deny or review exports, external connections, foreign
  tables, FDWs, `COPY PROGRAM`, and unapproved network paths.
- **Policy-as-code:** versioned policies for allowed DDL, TTLs, budgets, data
  sources, providers, and approval thresholds.
- **Evidence exports:** JSONL v1 export for audit events; richer SOC2/HIPAA/PCI
  packages can come later.
- **Tenant isolation:** hard boundaries by org/team/customer, including
  per-tenant encryption and retention where required.
- **Incident response:** kill switch by agent/team/deployment, read-only
  quarantine, credential revocation, and forensic timeline.
- **Supply-chain visibility:** record agent client version, prompt/tool pack
  version, migration generator version, and source repository SHA when supplied.
- **Human escalation:** owner paging, approval SLAs, stale-owner fallback, and
  emergency override with audit trail.
- **Provider governance:** quotas, region/data residency constraints, branch
  limits, maintenance windows, and restore guarantees per provider.
- **Compliance-friendly retention:** separate retention for audit events,
  backups, recommendations, query samples, and PII-bearing payloads.
- **Safety evaluation hooks:** replay or dry-run checks for generated SQL,
  prompt-injection test cases for query-generating agents, and regression tests
  for applied recommendations.
- **Operational metrics:** allow/review/deny/defer ratios, provisioning failures,
  quarantine causes, restore verification failures, cleanup dry-run results,
  stale deployments, and drop-rate limit hits.

## Data Model

Suggested pg_sage schema additions:

- `sage.agent_identities`
- `sage.agent_api_tokens`
- `sage.agent_db_idempotency_keys`
- `sage.agent_db_requests`
- `sage.agent_db_deployments`
- `sage.agent_db_leases`
- `sage.agent_db_objects`
- `sage.agent_db_audit_events`
- `sage.agent_db_cost_samples`
- `sage.agent_db_backups`
- `sage.agent_db_recommendations`
- `sage.agent_db_recommendation_feedback`

Use `database_name` consistently for fleet isolation. Add `tenant_id` or
`org_id` to every `agent_db_*` table; `database_name` is a fleet scoping column,
not a tenant boundary.

Critical fields:

- `tenant_id`
- `requested_isolation_type`, `actual_isolation_type`
- `provider`, `provider_resource_id`, `region`, `data_residency`
- `policy_decision`, `policy_reasons`
- `lease_state`, `last_ping_at`, `expires_at`
- `archive_state`, `last_backup_id`, `restore_verified_at`
- `budget_limit_usd`, `estimated_cost_usd`, `actual_cost_usd`
- `data_classification`, `masking_policy_id`
- `credential_state`, `credential_expires_at`
- `created_by_agent_id`, `created_by_run_id`, `approved_by_user_id`

Deployment states:

- `requested`
- `approved`
- `provisioning`
- `active`
- `stale`
- `quarantined`
- `archiving`
- `archived`
- `restore_verified`
- `dropping`
- `dropped`
- `failed`
- `provisioning_failed`

Audit events are append-only. v1 should at minimum deny UPDATE/DELETE through
application roles and provide JSONL export. v1.x can add hash chaining or an
external immutable sink.

## What To Pull From AgentDB

Pull in:

- Agent, run, capability, approval, workspace, and workspace object concepts.
- Append-only event taxonomy for deployment lifecycle only.
- Forge workspace creation flow.
- DDL policy and workspace query rules.
- TTL cleanup and promotion flow.
- Dashboard concepts for workspace inventory, cost, audit, and approvals.

Do not pull in yet:

- full memory vault
- full agent trace product
- framework SDK roadmap
- broad MCP gateway
- HNSW experiment/autotuning planner
- hosted AgentDB positioning
- pricing model
- complete org/owner IAM system

Those may belong in a separate AgentDB project. pg_sage should stay narrow:
database deployment, tuning, lifecycle, cost, and safety for agents.

## First Implementation Slices

### Slice 1: Agent DB Domain Model

- Add request/deployment/lease/backup/recommendation structs.
- Add lifecycle state machine including `provisioning_failed`.
- Unit tests for all state transitions and expiration boundaries.

### Slice 1.5: Identity, Token, Tenant, And Idempotency Model

- Add agent identity and deployment-bound ping token records.
- Add token scope rules for request, ping, recommendation feedback, cost samples,
  and admin actions.
- Add idempotency-key storage and conflict behavior.
- Add tenant/org scoping.

### Slice 2: API Request Path

- `POST /api/v1/agent-dbs/requests`
- idempotency support
- owner/agent identity validation
- pure policy classifier for allow/review/deny/defer
- advisory-only decision output
- UI request list

### Slice 3: Lease And Cleanup Skeleton

- ping endpoint
- lease extension endpoint
- stale/quarantine/archive/drop lifecycle
- dry-run cleanup report
- backup-before-drop guardrail
- no destructive action until backup verification contract exists

### Slice 4: Schema Provisioning

- create schema-per-workspace
- create scoped role
- record deployment
- register heartbeat lease
- handle mid-flight failure and cleanup tasks
- role cleanup plan: revoke login/grants, drain sessions, reassign/drop owned
  objects, then drop role if safe

### Slice 5: Query Recommendations API

- publish existing query/index/rewrite/vector/PostGIS/JSON recommendations as
  stable machine-readable records
- add feedback endpoint: accepted, rejected, applied, failed, superseded
- can run in parallel with slices 1-4 because it is mostly a read/API shape

### Slice 6: Cost And Budget Enforcement

- collect database-observable cost samples
- add optional external cost ingestion
- enforce hard and soft budgets

### Slice 7: Audit Events

- append-only lifecycle and action events
- JSONL export
- audit metrics

### Slice 8: Tuning Packs

- vector, PostGIS, JSONB, extension config hint packs
- attach recommendations to agent deployment ids
- surface in UI and API

### Slice 9: Promotion Deploy Requests

- schema diff
- migration script
- verification SQL
- rollback/forward-fix plan
- approval workflow

Promotion can be delayed until v1.x if the provisioning/cleanup/recommendation
loop is not yet boring and well-tested.

## Resolved Decisions For v1

1. v1 autonomous provisioning is `schema` plus `external` registration only.
   True database creation and provider branches are adapter-gated.
2. pg_sage may issue scoped credentials for schema workspaces. Secrets-manager
   integration is preferred for enterprises but not required for local v1.
3. Default stale policy is notify -> quarantine/read-only -> archive -> verified
   restore -> optional drop. Never immediate drop.
4. Query recommendations expose patch hints and request payloads. pg_sage does
   not open repository PRs in the first module.
5. Production-data seeding is blocked unless a masking policy or explicit human
   approval is attached.
6. MCP remains out of pg_sage scope for this module.
7. HNSW experiment/autotuning is a separate product. pg_sage keeps query-time
   pgvector hints and readiness checks.
8. Archive retention defaults to 14 days, policy-overridable.
9. Cost-limit overrun creates automatic read-only quarantine for hard limits;
   drop requires human approval.
10. Workspaces read production data through masked/materialized copies by
    default. Direct grants require policy-as-code opt-in.
11. Customer-managed encryption keys are v1.x. v1 still requires encrypted
    backups and recorded storage location.
12. JSONL is the minimum audit export format for v1.

## Still Open

1. Which provider adapter should be first after schema/external: self-managed
   database creation, Neon branches, Supabase branches, RDS/Aurora, Cloud SQL,
   or AlloyDB?
2. What exact role taxonomy should ship first: viewer/requester/approver/admin,
   plus break-glass?
3. Should schema backup verification always perform a test restore, or can local
   development mode accept checksum-only verification?
4. What sample SQL/query payload redaction is sufficient for v1 recommendation
   APIs?
5. What approval SLA behavior should be default: escalate, auto-deny, or leave
   pending?

## Source Notes

- Neon validates branch-like workflows, TTLs, API/CLI automation, and restore
  windows for temporary environments.
- Supabase validates separate preview environments with independent credentials,
  auto pause/delete behavior, and merge workflows.
- LangGraph validates persistent agent state, human-in-the-loop resumability,
  memory, time travel, and Postgres-backed checkpointers.
- OpenAI Agents SDK validates trace processors and external tracing ecosystem.
- Agentic security guidance supports least privilege, scoped tools, strong
  observability, provenance, and approval gates.
