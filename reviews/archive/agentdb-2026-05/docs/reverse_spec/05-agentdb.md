# 05 — AgentDB Subsystem (Agent-Deployed Database Provisioning)

Reverse-engineered from `internal/agentdb/*`, `internal/api/agent_db_*.go`, and
`web/src/pages/agentdb*`. This documents what is *actually implemented* as of the
current tree, distinguishing **live**, **scaffolding**, and **absent**.

The package lets an external "agent" request, provision, monitor, and tear down a
Postgres it owns — across local Postgres (schema/database isolation) and three cloud
providers at the instance level (AWS RDS, GCP Cloud SQL, Databricks Lakebase). Almost
everything is persisted in `sage.agent_db_*` tables (DDL in
`internal/agentdb/schema.go:180-520`) and exposed over REST under `/api/v1/agent-dbs`.

---

## 1. Execution Contract / Provisioning State Machine

### 1.1 Two orthogonal status fields

Every deployment row (`Deployment`, `types.go:91-119`) carries two independent
lifecycle fields:

- **`status`** — the *ownership/monitoring* lifecycle:
  `active` → `archived` → `deleted`, plus `budget_exceeded`. Driven by lease expiry,
  pings, budget, and cleanup (`store.go`, `policy.go`).
- **`provisioning_status`** — the *provider resource* state machine (below).

### 1.2 Provisioning state machine

The transition table is the single source of truth: `provider_runner.go:170-205`
(`validProvisionTransition`). It is enforced on **every** status write through
`requireProvisionTransition`, called by `updateProvisioningStatus`
(`execution.go:472-498`) and `applyProvisionResult` (`execution.go:500-539`). An illegal
transition returns `ErrInvalid` — the state machine is real, not advisory.

States and key edges:

```
registered → planned → preflight_passed → provisioning → {available | dry_run_ready | failed | status_unknown}
planned    → {preflight_passed | preflight_failed | status_checked | destroy_dry_run_ready}
dry_run_ready    → {preflight_passed | status_checked | provisioning | destroy_pending}
available        → {status_checked | destroy_pending | status_unknown}
status_checked   → {available | preflight_passed | provisioning | destroy_pending | destroy_dry_run_ready}
status_unknown   → {status_checked | available | failed | provisioning}
destroy_pending  → {destroying | destroy_dry_run_ready}
destroying       → {destroyed | failed | status_unknown}
failed           → {preflight_passed | provisioning}
provisioned      → status_checked   (local schema/db terminal-ish)
queued → cancel_requested → cancelling → {destroyed | failed}   # declared, see note
archived         → {destroy_pending | destroy_dry_run_ready}
```

Operation methods (in `execution.go` unless noted):

| Method | Kind recorded | Gating |
|---|---|---|
| `PreflightProvision` | `preflight` | requires cloud instance + valid plan commands; sets `preflight_passed` |
| `ExecuteProvision` (dry-run) | `execute` | only from `preflight_passed`/`failed`; conflicts if already `dry_run_ready`/`ready`; runs `commands[0]` through `DryRunProvisionRunner` |
| `ExecuteProvisionLive` | `execute_live` | rejects nil/`dry_run` runner; requires the exact persisted plan, estimate, policy generation, authorization, requester, and idempotency tuple; records a creation receipt |
| `CheckProvisionStatus` / `...Live` | `status_check[_live]` | live status maps provider state; if provider returns NotFound while `destroying`, transitions to `destroyed` |
| `DestroyProvisionDryRun` | `destroy_dry_run` | requires verified-restore backup if `BackupRequired` |
| `DestroyProvisionLive` | `destroy_live` | requires a separate exact-operation authorization and verified-restore backup; only from `available`/`status_checked`/`dry_run_ready` |
| `CheckBackupAssurance[Live]` (`backup_assurance.go`) | `backup_check[_live]` | records a `verified` backup row |
| `PlanRestoreDrillDryRun` | `restore_drill_dry_run` | dry-run only; explicitly does **not** grant restore verification |

**Note (partial scaffolding):** the `queued → cancel_requested → cancelling`
sub-chain exists in the transition table but no method emits those states; there is no
async job queue. Provisioning is synchronous within the HTTP request.

### 1.3 Receipts

`CreationReceipt` (`types.go:442-453`) is persisted to
`sage.agent_db_creation_receipts` (one row per deployment, `ON CONFLICT DO UPDATE`,
`execution.go:586-619`) only on a **live** create that returns a `ProviderResourceID`
(`execution.go:180-191`). It captures provider, resource id, `operation_mode="live"`,
and a `request_hash` (set to the cost-estimate id). Detail is redacted before write.

### 1.4 Redaction

`RedactProviderDetail` / `redactString` (`provider_redaction.go`) recursively scrub maps,
slices, and URL credentials against a sensitive-key list (password, token, secret,
access_key, private_key, client_email, x-amz, signature, dsn, connection_string, …).
Redaction is applied at every persistence boundary: provision attempts
(`recordProvisionAttempt`, `execution.go:562`), connection info (`applyProvisionResult`),
audit detail (`operations.go:272-279`), creation receipts, and provider-config reads
(`provider_config.go:55,91`). Secret *settings* are rejected on write
(`rejectSecretSettings`, `provider_policy.go:110-122`) so credentials never land in
`agent_db_provider_configs`.

### 1.5 Naming

`resourceName` (`providers.go:426-448`) lowercases, replaces non-alnum with `-`, trims,
falls back to `agentdb`. `ProviderResourceName` (in `provider_naming.go`) adds
provider-specific length/charset rules. Local schema/db names go through
`sanitizeSchemaName` (`schema.go:143-170`, 63-char cap, underscore separators, numeric
prefix guard). Identifiers are always quoted via `quoteIdent` for local DDL.

---

## 2. Provider Runners

Interface `ProviderRunner` (`provider_runner.go:38-46`): `Name`, `Provider`,
`Preflight`, `Create`, `Status`, `Destroy`, `BackupCheck`. Registry
(`RunnerRegistry`) maps provider → runner with a `DryRunProvisionRunner` fallback.
`ForProvider` refuses local Postgres and unknown providers (`ErrInvalid`).

### 2.1 Gating — env flags + provider config

Runners are only wired live when **`PG_SAGE_LIVE_PROVISIONING=1`**
(`runtime_registry.go:10-21`). Each provider has a second flag *and* required creds:

| Provider | Enable flag | Required env |
|---|---|---|
| AWS RDS | `PG_SAGE_ENABLE_AWS_RDS_RUNNER=1` | region via `PG_SAGE_AWS_REGION`/`AWS_REGION`/`AWS_DEFAULT_REGION` (uses AWS SDK default credential chain) |
| GCP Cloud SQL | `PG_SAGE_ENABLE_GCP_CLOUDSQL_RUNNER=1` | `PG_SAGE_GCP_ACCESS_TOKEN` + project (`PG_SAGE_GCP_PROJECT`/`GOOGLE_CLOUD_PROJECT`); region defaults `us-central1` |
| Databricks Lakebase | `PG_SAGE_ENABLE_LAKEBASE_RUNNER=1` | host + token (`PG_SAGE_DATABRICKS_HOST/TOKEN` or `DATABRICKS_*`); instance mode hard-disabled (`NewLakebaseRunner(..., false)`) |

If a flag is set but creds are missing, the runner is silently **not** registered, so
the registry returns the dry-run fallback. The default (no env) registry is pure dry-run.

Live execution is reached only after the admin-only `POST
/{id}/provision/authorize-live` endpoint persists a server-owned plan, estimate,
effective policy generation, operation authorization, requester, nonce, and idempotency
tuple. The subsequent create or destroy request must repeat the exact returned identity.
Client-supplied approval, actor, cost, policy, and override claims do not grant authority.

### 2.2 AWS RDS (`aws_rds_runner.go`) — LIVE (real SDK)

Uses `aws-sdk-go-v2` `rds` client. `Create` calls `CreateDBInstance` with
`ManageMasterUserPassword=true` (so the master secret is an AWS Secrets Manager ARN,
surfaced as `secret_ref`/`secret_ref_provider=aws_secrets_manager`), `StorageEncrypted`,
`AutoMinorVersionUpgrade`, tags including `pg_sage_deployment_id` and a `ttl` tag from the
lease. Defaults: `db.t4g.micro`, 20 GB, 7-day backup retention. `Status` maps RDS states
to the provisioning state machine; `Destroy` skips the final snapshot only for
`disposable` deployments and treats NotFound as `destroyed`. Public instances are denied
unless `Policy.AllowPublicIP`. Errors are normalized by `mapAWSError` to typed
`ProviderError` (Conflict/Throttle/Quota/NotFound/Invalid/Unavailable). `BackupCheck`
== `Status`.

### 2.3 GCP Cloud SQL (`gcp_cloudsql_runner.go`) — LIVE (real REST)

`CloudSQLHTTPClient` calls the Cloud SQL Admin API (`sqladmin.googleapis.com`,
`/sql/v1beta4/...`) with a bearer token. `Create` POSTs an instance with backups +
PITR enabled, `requireSsl`, encryption implicit, `deletionProtection` on unless
disposable. Policy guards: rejects public IPv4 unless allowed, rejects `0.0.0.0/0`
authorized networks, enforces an edition rule for `db-f1-micro`. `Status` maps
RUNNABLE/PENDING_*/MAINTENANCE; `secret_ref_provider=gcp_secret_manager` is advertised
but **no secret ARN is actually retrieved** — credential handoff for GCP is incomplete
vs AWS. NotFound on delete → `destroyed`.

### 2.4 Databricks Lakebase (`lakebase_runner.go`) — LIVE (real REST), branch-only

`LakebaseHTTPClient` calls `/api/2.0/postgres/projects/{project}/branches`. Default mode
is `autoscaling_branch` (cheap, copy-on-write branch off a `source_branch`, default
`main`), TTL derived from the lease. **Full-instance mode is hard-disabled**
(`allowInstanceMode=false` from env wiring); requesting it returns `ErrInvalid`. Requires
`provider_params.project`. No secret ref is produced.

### 2.5 Dry-run runner (`provider_runner.go:106-168`)

The default. `Preflight`/`Create`/`Status`/`Destroy`/`BackupCheck` echo the planned
command, set `execution_mode=dry_run`, and advance to `*_ready` states. This is the path
exercised when live provisioning is off — useful for plan validation without touching a
cloud account.

---

## 3. Blueprints, Terraform, and the Store Persistence Model

### 3.1 Deterministic blueprint generator (LIVE)

`HeuristicBlueprintGenerator` (`blueprint.go:15-53`) parses a free-text `intent` with
regexes to infer provider, region, storage, backup days, PITR/Multi-AZ, private/public
networking, extensions (pgvector/postgis/…), and budget (`$NNN`). `NormalizeBlueprintSpec`
fills provider defaults. It then renders Terraform via
`RenderTerraformFromBlueprint` → `renderAWSRDS`/`renderCloudSQL`/`renderLakebase`
(`blueprint.go:367-482`) producing a single `main.tf` with encryption, deletion
protection, and backup config baked in.

### 3.2 Optional LLM blueprint generation (LIVE wiring, heuristic fallback)

`BlueprintGenerator` is an interface (`types.go:541-543`). The API wires a generator from
the LLM manager (`newAgentDBBlueprintGenerator(llmMgr)` at `router.go:134`). When the LLM
returns no files, the store falls back to deterministic Terraform rendering
(`blueprint.go:127-133`). `CreateBlueprint` requires a non-nil generator
(`ErrBlueprintLLMRequired`) and records `llm_used` + `raw_response`.

### 3.3 Blueprint → template → deployment flow

`CreateBlueprint` (`blueprint.go:108-197`) also persists a derived Terraform template
(`{id}_tf`). Policy findings from both blueprint and template are merged; **any finding
sets status `rejected`** and blocks approval (`ApproveBlueprint`, `provision_links.go:21-45`).
`ProvisionFromBlueprint` (`provision_links.go:47-73`) only works on an `approved`
blueprint: it builds a `RegisterRequest` + `SizeProfile` from the spec, computes the
provisioning plan, sets `provisioning_status=planned`, tags metadata with
`blueprint_id`/`terraform_template_id`, and calls `Register`.

### 3.4 Terraform template upload / import / policy (`terraform_policy.go`)

Templates can be uploaded inline or from a zip (`TerraformFilesFromZip`, 256 KB cap).
`TerraformPolicyFindings` rejects: `.tfvars`/`.tfstate`/`.terraform/`/secret-named files,
non-`.tf` files, dangerous constructs (`provisioner`, `local-exec`, `remote-exec`,
`null_resource`, `local_file`, `data "external"`), and unpinned `git::` module sources.
`TerraformManifest` extracts providers/resources/variables/outputs via regex. File bodies
are redacted on store. There is **no `terraform apply`/`plan` execution from templates** —
the plan command strings reference `terraform apply` but only the SDK/REST runners ever
run; Terraform itself is never invoked. Template rendering/policy/import is real; Terraform
*execution* is not.

### 3.5 Store model

`Store` (`schema.go:13`) wraps a `pgxpool.Pool`. `Ensure` (idempotent CREATE/ALTER) runs
the full DDL set on first use and seeds default size profiles. Tables:
`agent_identities`, `agent_db_requests`, `agent_db_deployments`,
`agent_db_provider_configs`, `agent_db_creation_receipts`, `agent_db_terraform_templates`,
`agent_db_blueprints`, `agent_db_size_profiles`, `agent_db_pings`,
`agent_db_ping_tokens`, `agent_db_ping_token_failures`, `agent_db_recommendations`,
`agent_db_cost_samples`, `agent_db_backups`, `agent_db_tuning_hints`,
`agent_db_provision_attempts`, `agent_db_audit`, `agent_db_deploy_requests`,
`agent_db_live_plans`, `agent_db_live_estimates`, `agent_db_live_authorizations`,
`agent_db_live_receipts`, `agent_db_monitoring_policies`,
`agent_db_monitoring_state`, and `agent_db_monitoring_work`. Child tables FK to
deployments with `ON DELETE CASCADE` where applicable.

---

## 4. Agent Requests → Deployment Flow; Approval, Cost, Policy Gates

### 4.1 Request intake + policy decision

`CreateRequest` (`store.go:17-44`) normalizes, applies idempotency
(`tenant_id + idempotency_key` unique; mismatched body hash → `ErrConflict`), and runs
`DecideRequest` (`policy.go:12-44`). Decisions:

- Missing tenant/agent → **deny**.
- Region not in `AllowedRegions` → **deny**.
- Sensitive `DataClassification` (production/restricted/pii/phi/pci) without a
  `MaskingPolicyID` → **deny**; with one → **review**.
- `schema`/`database` isolation → **allow**.
- `instance` on a cloud provider with `BudgetUSD<=0` → **review** (budget approval);
  otherwise **allow** (plan only).
- `external` → review; `branch` → defer; else deny.

Note: masking is a **gate** (it requires a policy id) but no data masking is actually
applied anywhere — it is a precondition check, not an enforcement mechanism.

### 4.2 Approval → provision

`SetRequestDecision` records approve/deny. `ProvisionApprovedRequest`
(`provision_links.go:139-171`) refuses unless `status=approved && policy_decision=allow`,
then builds a `RegisterRequest` and calls `Provision`. `Provision` (`schema.go:64-90`)
routes: local → real `CREATE SCHEMA`/`CREATE DATABASE`; cloud instance → build plan +
register with `planned` status (no cloud call yet — execution is a separate, gated step).

### 4.3 Cost estimation & budget gates (`provider_cost.go`)

`EstimateAgentDBCost` returns a monthly + TTL-prorated estimate from small hardcoded
price tables per provider/class, with `confidence` and `unknown_components`.
`BudgetGate` doubles the compared cost when confidence is low/unknown; if it exceeds the
budget → not allowed; if above half the budget → requires review. Separately,
`BudgetStatus` (`policy.go:46-57`) classifies running cost (`under_budget`/`soft_limit`
at 90%/`hard_limit`), and `applyBudgetStatus` (`operations.go:281-296`) flips a
deployment to `budget_exceeded` when cost samples cross the hard limit. Cost samples are
agent-reported via `AddCostSample` — there is **no automatic metering**; chargeback is
self-reported.

### 4.4 Live provisioning policy and authority

`ResolveEffectiveLiveProvisionPolicy` is fail-closed across runtime runner capability,
global hard ceilings, persisted provider policy, and exact-operation authorization. It
intersects allowlists and takes the narrowest positive TTL/cost limits. Missing,
unreadable, stale, or provider-mismatched layers deny execution. `manual` denies provider
mutation; `auto_within_policy` additionally requires a high-confidence current estimate
with no unknown cost components. Provider settings can narrow but never widen global
policy, and provider-policy mutation is admin-only.

`IssueLiveExecutionRecords` normalizes the size profile/provider parameters into an
immutable hashed plan. The estimate, pricing revision, policy hash/generation,
authorization nonce, authenticated requester, and idempotency key are persisted before
execution. Create and destroy authorizations are distinct and single-consumption. An
exact replay returns its stored receipt; any mismatched replay fails without a provider
call.

### 4.5 Deploy requests (schema-change review, `deploy_requests.go`)

Distinct from *provisioning*: a `DeployRequest` is a reviewed schema migration *against*
an agent DB. `draft → review_requested → approved|denied`. Gate results record
`has_migration_sql`, `has_verification_sql`, `has_rollback_or_forward_fix`,
`migration_statement_count`, and `review_only=true`. **Critically, this is review-only
metadata: pg_sage never executes the migration SQL.** It captures intent + approval, not
execution.

---

## 5. Monitoring Registration / Fleet Integration

### 5.1 Agent-ping path (LIVE)

Each deployment can mint scoped ping tokens (`CreatePingToken`/`RotatePingToken`,
`identity.go`), hashed (SHA-256) at rest, with expiry, rotation lineage, and a
brute-force lockout (5 failures / 5 min → `ErrRateLimited`, `identity.go:15-18`,
298-330). `AgentPing` (`identity.go:264-274`) validates the token then records a row in
`agent_db_pings` and updates `last_ping_at`/`status`. The ping endpoint
`POST /api/v1/agent-dbs/{id}/agent-ping` is the **only** AgentDB route not behind
admin/operator role (`agent_db_handlers.go:26-29`) — it is token-authenticated for the
agent itself.

### 5.2 Fleet collector integration (partial)

After lifecycle reconciliation, `syncAgentDBsToFleet` registers active deployments that
carry inline host/database connection information under an `agentdb:` fleet name. It
starts a collector and exposes snapshot-level fleet visibility. Deployments whose
credentials are represented only by `secret_ref` are skipped because runtime secret
resolution is not wired into this path. The analyzer and executor are also not attached,
so this does not grant autonomous mutation authority.

### 5.3 Lifecycle reconciliation (scheduled LIVE logic)

`ReconcileAbandonedDeployments` (`lifecycle.go:9-73`) archives lease-expired deployments
(`ArchiveExpired`) then, for cloud instances, attempts live destroy (if live runner +
live mode + destroyable status) or dry-run destroy, recording `Blocked` entries when a
verified restore is required or the runner is unavailable. `ReconcileLiveProvisioning`
sweeps in-flight states (`provisioning`/`destroying`/`status_unknown`) and reconciles
against provider live status. `startAgentDBReconciler` schedules both paths at
`agentdb.reconcile_interval_seconds` (300 seconds by default), then runs the partial fleet
sync described above. Expiry uses durable compare-and-swap claims; renewal invalidates a
stale claim and concurrent reconcilers cannot authorize two provider mutations for one
lifecycle version.

### 5.4 Durable monitoring work (implemented, worker not wired)

`ScheduleMonitoring` persists `light_sql_probe` work after scanning at most 10,000 rows and grouping
at most 100 physical targets per pass. `ClaimMonitoringWork` issues bounded leases (100
maximum), reclaims expired leases with row locks, and enforces global, provider, tenant,
and deployment concurrency policy. Claims carry `secret_ref`, not resolved credentials,
for just-in-time secret acquisition. No running worker currently calls the scheduler,
executes probes, escalates to costlier tiers, or completes the claims.

---

## 6. Governance / Security / Chargeback / Archival — Today vs Absent

### Present today (LIVE)

- **Security:** ping-token auth with hashing + rotation + lockout; admin/operator RBAC on
  management routes (`RequireRole`, `agent_db_handlers.go:25`); pervasive secret redaction;
  secret-settings rejection; AWS Secrets Manager–managed master passwords; storage
  encryption + SSL enforced in plans/runners; public-IP and `0.0.0.0/0` denial; Terraform
  policy scanner blocking dangerous constructs and state/secret files.
- **Governance:** policy decision engine on intake; live-provision allowlist policy
  (region/account/project/workspace/TTL/cost); blueprint/template approval workflow with
  policy findings; deploy-request review workflow; full append-only `agent_db_audit` trail
  on every state change.
- **Lifecycle / TTL:** real — leases (`lease_expires_at`), `ExtendLease`, `ArchiveExpired`
  on expiry, TTL propagated to RDS tags and Lakebase branch TTL, cleanup state machine
  (`CleanupDecisionFor`) blocking deletion until provider resource destroyed + verified
  backup exists.
- **Multi-tenancy:** partial — `tenant_id` is first-class on identities, requests,
  deployments, deploy-requests; idempotency and indexes are tenant-scoped; policy requires
  tenant identity. **But** management list endpoints return all tenants (no row-level
  tenant filtering / isolation enforcement on reads) — it is tenant-*tagged*, not
  tenant-*isolated*.
- **Archival:** partial — deployments archive on lease expiry and a verified-backup gate
  precedes destructive cleanup; `CheckBackupAssurance` records backup rows. **But** there
  is no actual data archival/export of the agent DB's contents; "archive" is a status, and
  backup "verification" in dry-run mode is explicitly *not* a real restore drill
  (`backup_assurance.go:141`).
- **Chargeback:** partial — per-deployment `budget_usd`, cost samples, budget-state
  classification, and auto-`budget_exceeded`. **But** cost is agent-self-reported; there is
  no cloud billing integration or automatic metering, so chargeback is advisory.

### Absent / scaffolding

- **No Terraform execution** — only render/policy/manifest (see §3.4).
- **No runtime consumer for durable monitoring work** (see §5.4).
- **No analyzer/executor attachment for AgentDB fleet entries** (see §5.2).
- **No async provisioning queue** — `queued/cancelling` states are declared but unused.
- **No data masking enforcement** — masking is a request precondition only (§4.1).
- **No real restore-drill / data archival** (§ Archival above).
- **GCP/Lakebase secret handoff** is incomplete relative to AWS (no secret ARN retrieved).

---

## 7. Frontend

`web/src/pages/AgentDBsPage.jsx` + `web/src/pages/agentdb/*` provide the operator UI:
workspace tabs, provisioning panels, form controls, and section views over the REST API.
This is the primary management surface (consistent with the project's web-UI-first
direction). It surfaces requests, deployments, blueprints, templates, provider configs,
provision attempts, cost/budget, backups, and audit. Runtime monitoring is partial:
eligible inline-credential deployments receive fleet snapshots, while durable monitoring
work has no worker and the analyzer/executor pipeline is not attached.
