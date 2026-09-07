# Agent DB Live Provisioning GA Spec

## Goal

Ship Agent DB live provisioning as a safe pg_sage control-plane feature. Agents
and operators should be able to request, approve, create, observe, tune, cost,
back up, and clean up disposable or semi-persistent PostgreSQL environments
across local Postgres, AWS RDS, GCP Cloud SQL, and Databricks Lakebase without
leaking credentials or accidentally creating unbounded cloud spend.

This spec upgrades the current state from dry-run plans plus external live
smoke tests into an integrated execution path inside pg_sage.

## Current State

The repo already has meaningful Agent DB foundations:

- Local Postgres provisioning supports schema and database levels.
- Cloud providers are modeled as instance-level only.
- Agent DB deployments, pings, leases, costs, backups, tuning hints,
  recommendations, deploy requests, audit events, and custom size profiles are
  persisted under `sage.agent_db_*`.
- The UI exposes Agent DB deployments, size profiles, provider cloud settings,
  Lakebase branch/full-instance selection, and provisioning dry-run controls.
- Provider plans exist for AWS RDS, Cloud SQL, and Lakebase.
- The 8085 local sidecar has been used for live product validation.
- Live cloud smoke tests proved actual create/delete capability outside the
  sidecar:
  - Cloud SQL: create, status, delete through Cloud SQL Admin API.
  - AWS RDS: create, wait for available, delete through boto3.
  - Lakebase: create branch, observe `READY`, delete through Databricks REST.

The main go-live gap is that `sidecar/internal/api/agent_db_execution_handlers.go`
still injects `agentdb.DryRunProvisionRunner{}` for execute, status, backup,
restore-drill, destroy, and reconcile.

## Research Summary

Primary provider docs and community threads point to the same adoption risks:
creating the database is the easy part; safely connecting, governing, proving
backup/restore, reconciling drift, and bounding cost are the product.

- AWS RDS creation requires VPC-aware choices, subnet groups, security groups,
  backup/deletion settings, and credentials decisions. AWS docs explicitly call
  out VPC/subnet prerequisites and deletion protection/backup behavior.
- RDS Secrets Manager integration should be preferred for master password
  handling rather than pg_sage generating and storing plaintext passwords.
- Cloud SQL creation needs edition/tier compatibility, backup configuration,
  and an explicit public/private network model. Google docs recommend Cloud SQL
  Auth Proxy/connectors for secure access and document private services access
  requirements for private IP.
- Cloud SQL public IP without authorized networks can still be reachable through
  Auth Proxy, but direct public authorized networks are a major footgun.
- Databricks Lakebase branch create/delete is a long-running API flow; branch
  docs require project permissions and note that branch renames are unsupported.
- Databricks recommends OAuth machine-to-machine service principals for
  unattended automation instead of user credentials.
- Neon, Supabase, Crunchy Bridge, and Aiven show competitive expectations:
  cheap copy-on-write branches, preview environments, API/Terraform automation,
  backup/fork flows, and termination protection. Forum reports also show real
  pain around unclear branch lifecycle, data security, migration drift, and
  surprising deletion behavior.
- Recent database-branching research highlights a trade-off agents will hit:
  fast branching systems are not automatically fast for deep or long-lived
  branch workloads. pg_sage should track branch age, branch depth where
  available, read/write intensity, and promotion intent.

References:

- AWS RDS CreateDBInstance API:
  https://docs.aws.amazon.com/AmazonRDS/latest/APIReference/API_CreateDBInstance.html
- AWS RDS DB instance prerequisites and VPC model:
  https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_CreateDBInstance.html
- AWS RDS Secrets Manager integration:
  https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html
- Cloud SQL create instance:
  https://cloud.google.com/sql/docs/postgres/create-instance
- Cloud SQL Admin API:
  https://cloud.google.com/sql/docs/postgres/admin-api/rest
- Cloud SQL Auth Proxy:
  https://cloud.google.com/sql/docs/postgres/sql-proxy
- Cloud SQL private IP:
  https://cloud.google.com/sql/docs/postgres/private-ip
- Cloud SQL backup/PITR:
  https://cloud.google.com/sql/docs/postgres/backup-recovery/pitr
- Databricks Lakebase API guide:
  https://docs.databricks.com/aws/en/oltp/projects/api-usage
- Databricks Lakebase branch management:
  https://docs.databricks.com/aws/en/oltp/projects/manage-branches
- Databricks service principal OAuth:
  https://docs.databricks.com/en/dev-tools/auth/oauth-m2m.html
- Neon branching:
  https://neon.com/docs/introduction/point-in-time-restore
- Crunchy Bridge backup/fork API:
  https://docs.crunchybridge.com/api/cluster-backup

## Design Principles

1. Dry-run is always available and remains the default for new installations.
2. Live execution requires explicit global and per-provider enablement.
3. Provider CLIs are allowed for local diagnostics, not as pg_sage's live
   provisioning path.
4. Every live operation is idempotent by deployment ID and provider tags/labels.
5. No provider secret, generated DB password, OAuth token, connection password,
   or cloud credential is stored in plaintext in pg_sage tables, logs, attempts,
   audits, UI, or API responses.
6. Every paid resource has owner, deployment ID, provider, region, and TTL
   tags/labels where the provider supports them.
7. Delete is harder than create: destructive cleanup requires backup assurance
   and a provider status check.
8. Reconcile is a first-class loop, not an afterthought.
9. Branches are treated differently from full instances. Branches are cheaper
   and faster, but require age/depth/parent tracking and promotion guardrails.
10. pg_sage should expose actionable failure reasons agents can use to repair
    requests, not just "provider failed."

## 1. Execution Contract

### Actions

- `preflight`: validate policy, credentials, plan, quotas where available,
  network shape, cost estimate, backup defaults, and idempotency target.
- `execute`: create or attach to the provider resource.
- `status`: read provider state and update pg_sage connection/provisioning
  metadata.
- `backup-check`: verify managed backup configuration or local backup evidence.
- `restore-drill`: prove a restore path without treating a dry-run as a
  verified restore.
- `destroy-dry-run`: show what would be deleted and why deletion is allowed or
  blocked.
- `destroy`: delete the provider resource only after backup and policy gates.
- `reconcile`: detect and repair stale pg_sage state or orphaned cloud
  resources.

### Provisioning States

Allowed `provisioning_status` values:

- `planned`
- `preflight_failed`
- `preflight_passed`
- `approval_required`
- `queued`
- `provisioning`
- `cancel_requested`
- `cancelling`
- `available`
- `failed`
- `status_unknown`
- `backup_unverified`
- `restore_verified`
- `destroy_pending`
- `destroy_preflight_failed`
- `destroying`
- `destroyed`
- `cleanup_blocked`
- `orphaned`

State transitions must be checked in code. Invalid transitions return
`ErrInvalid` and record an audit event with the attempted transition.

State entry rules:

| State | Valid From |
| --- | --- |
| `planned` | initial cloud deployment registration |
| `preflight_failed` | `planned`, `preflight_passed`, `approval_required` |
| `preflight_passed` | `planned`, `preflight_failed` |
| `approval_required` | `preflight_passed` |
| `queued` | `approval_required`, `preflight_passed` |
| `provisioning` | `queued`, `preflight_passed`, `failed` |
| `cancel_requested` | `queued`, `provisioning` |
| `cancelling` | `cancel_requested` |
| `available` | `provisioning`, `status_unknown` |
| `failed` | any active provisioning or destroy state |
| `status_unknown` | `provisioning`, `available`, `destroying` |
| `backup_unverified` | `available` |
| `restore_verified` | `available`, `backup_unverified` |
| `destroy_pending` | `available`, `restore_verified`, `cleanup_blocked` |
| `destroy_preflight_failed` | `destroy_pending` |
| `destroying` | `destroy_pending` |
| `destroyed` | `destroying`, `cancelling` |
| `cleanup_blocked` | `available`, `destroy_pending`, `destroy_preflight_failed` |
| `orphaned` | provider reconcile found tagged resource with no active deployment |

Entering `destroy_pending` from `cleanup_blocked`, forcing destroy, or
recovering `orphaned` resources requires an operator or admin role and must
write an audit event.

### Idempotency

All create requests use:

- deterministic provider resource name from `deployment_id`
- provider tags/labels containing `pg_sage_deployment_id`
- provider lookup before create
- result adoption when a matching tagged resource already exists

### Resource Naming

Provider resource names are derived, not blindly copied from deployment IDs.
The derived value is persisted as `provider_resource_id`.

Default derivation:

```text
provider_resource_id = "pgs-" + lowercase(hex(sha1(deployment_id))[:20])
```

RDS uses this derived name to satisfy identifier constraints: 1-63 characters,
letter-leading, lowercase letters/digits/hyphens, no trailing or consecutive
hyphens. Cloud SQL uses the same derived name unless a stricter provider rule
requires a shorter hash. Lakebase branches may use a readable sanitized
deployment ID only when it satisfies Lakebase branch ID rules; otherwise they
use the same hash form.

All delete requests are safe to retry:

- not found from provider maps to `destroyed` only if provider name and
  deployment ID match the stored deployment
- partial delete states map to `destroying`
- failed deletes keep the deployment active or cleanup-blocked, never deleted

### Creation Receipts

Before any live create call leaves pg_sage, pg_sage writes a creation receipt
inside the same transaction that changes the deployment to `provisioning`.
The receipt stores deployment ID, provider, provider resource ID, region,
account/project/workspace, request hash, operation mode, and timestamp.
Reconcile uses receipts to recover crash-during-create cases where the provider
accepted the request but pg_sage crashed before recording the final state.

### Typed Provider Errors

Provider runners return typed errors so agents and operators can repair the
right field:

- `ErrProviderQuota`
- `ErrProviderRateLimit`
- `ErrProviderTransient`
- `ErrProviderConfig`
- `ErrProviderAuth`
- `ErrProviderConflict`
- `ErrProviderNotFound`

Every typed error carries a redacted provider message and an actionable repair
hint.

## 2. Provider Runner Interface

Replace the current command-shaped runner with a provider-native operation
interface. Keep `DryRunProvisionRunner` as one implementation.

```go
type ProvisionOperation string

const (
    OperationPreflight ProvisionOperation = "preflight"
    OperationCreate    ProvisionOperation = "create"
    OperationStatus    ProvisionOperation = "status"
    OperationDestroy   ProvisionOperation = "destroy"
    OperationBackup    ProvisionOperation = "backup_check"
    OperationRestore   ProvisionOperation = "restore_drill"
)

type ProvisionRequest struct {
    Deployment Deployment
    Profile    SizeProfile
    Operation  ProvisionOperation
    Live       bool
    Now        time.Time
}

type ProvisionResult struct {
    ProviderResourceID string
    ProviderState      string
    Endpoint           string
    Port               int
    DatabaseName       string
    SecretRef          string
    CostEstimateUSD    float64
    BackupStatus       string
    SafeForDestroy     bool
    Detail             map[string]any
}

type ProviderRunner interface {
    Provider() string
    Preflight(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    Create(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    Status(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    Destroy(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    BackupCheck(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
    RestoreDrill(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
}
```

Runners:

- `DryRunRunner`
- `AWSRDSRunner`
- `GCPCloudSQLRunner`
- `DatabricksLakebaseRunner`

Runner selection is injected into API routes through a registry. Tests can
install fake runners.

## 3. Credential Model

### pg_sage Stores

pg_sage may store:

- provider name
- account/project/workspace ID
- region
- resource ID
- provider resource ID
- endpoint hostname
- port
- database name
- secret reference ARN/resource/name
- secret reference provider
- secret reference expiration
- creation receipt metadata
- non-sensitive tags/labels
- redacted audit details

pg_sage must not store:

- cloud access keys
- AWS session tokens
- Google OAuth tokens
- Databricks OAuth tokens
- generated database passwords
- raw connection URLs containing credentials
- private keys

### Provider Auth

AWS:

- MVP local/dev: SDK default credential chain with optional profile name.
- Enterprise: IAM role, IRSA/workload identity, or explicit AWS config mounted
  into the sidecar environment.
- Database credentials: prefer RDS-managed master password in AWS Secrets
  Manager. If unsupported, generate through provider runner and store only an
  external secret reference.

GCP:

- MVP local/dev: Application Default Credentials.
- Enterprise: service account impersonation or workload identity.
- Database credentials: prefer Cloud SQL IAM database authentication where the
  target workload can use it. Otherwise store a secret in Secret Manager and
  keep only the secret resource name in pg_sage.

Databricks:

- MVP local/dev: Databricks unified auth profile.
- Enterprise: OAuth machine-to-machine service principal.
- Database credentials: use Lakebase database credential generation where
  available; store only credential expiration and reference metadata.

### Redaction

All provider result details pass through `RedactProviderDetail` before storage
or response. Redaction covers keys matching:

- `password`
- `secret`
- `token`
- `credential`
- `private_key`
- `connection_string`
- `dsn`
- `access_key`
- `session`

Redaction is recursive for maps and arrays.

## 4. Policy and Safety

New config:

```yaml
agentdb:
  live_provisioning_enabled: false
  max_live_creates_per_hour: 5
  max_live_creates_per_day: 20
  rate_limit_dimensions: [global, provider, provider_account, requester]
  require_approval_for_live: true
  require_ttl_seconds: true
  default_ttl_seconds: 14400
  max_ttl_seconds: 604800
  allowed_providers: [local_postgres]
  allowed_regions: []
  allowed_accounts: []
  allowed_projects: []
  allowed_workspaces: []
  allow_public_ip: false
  require_private_network_for_production: true
  require_backup_before_destroy: true
  provider_execution:
    aws_rds:
      enabled: false
      profile: ""
      account_id: ""
      default_region: ""
    gcp_cloudsql:
      enabled: false
      project: ""
      default_region: ""
      use_adc: true
    databricks_lakebase:
      enabled: false
      workspace_host: ""
      profile: ""
```

Policy gates:

- live create denied unless global and provider live flags are enabled
- provider must be allowed
- empty allowlist means deny-all for that live-mode dimension; use `["*"]`
  explicitly to allow any account, project, workspace, or region
- account/project/workspace must be allowed
- region must be allowed
- TTL is required for agent-created live resources
- TTL countdown starts when the resource first reaches `available`, not at
  request, preflight, or queued time
- profile-level TTL overrides the global default; recommended defaults are
  3600 seconds for Lakebase branches and 14400 seconds for full RDS/Cloud SQL
  instances
- budget estimate must be under `budget_usd` unless operator override exists
- production safety mode requires private networking or explicit exception
- destructive actions require backup/restore gate
- live create approvals reuse the existing Agent DB deploy-request review
  workflow instead of introducing a second approval workflow
- rate limits apply at global, provider, provider account/project/workspace,
  and requester dimensions

Production safety mode is a provider-config setting. When enabled, all live
creates require private networking, deletion protection or an approved
TTL-bound exception, automated backups of at least seven days, and admin-role
approval for any policy override.

## 5. Networking

Networking is the most likely enterprise blocker.

### AWS RDS

MVP:

- support existing `db_subnet_group_name`
- support existing `vpc_security_group_ids`
- support `publicly_accessible=false` default
- support `publicly_accessible=true` only if policy allows public IP
- support `deletion_protection=false` only for TTL-governed disposable DBs

Enterprise follow-up:

- create managed security group from a named template
- validate subnet group spans required availability zones
- optional RDS Proxy
- optional IAM database authentication

### GCP Cloud SQL

MVP:

- support public IP only when Cloud SQL Auth Proxy or connector usage is
  configured and verified by preflight
- support private IP when a VPC network and private services access are
  provided
- support `sslMode` / require SSL
- block `authorizedNetworks=0.0.0.0/0`
- reject non-proxy public IP paths by default when live provisioning is enabled

Enterprise follow-up:

- VPC connector checks
- Cloud SQL connector metadata for application runtimes
- IAM database auth bootstrap

### Databricks Lakebase

MVP:

- support Lakebase branch create/delete in an existing project
- record source branch, created branch, operation name, TTL, and state
- support full provisioned instance only when API shape is verified by live
  test in the target workspace

Enterprise follow-up:

- service principal ownership
- endpoint credential generation
- branch promotion workflow
- branch age/depth/parent observability

## 6. Backup and Restore

Managed providers:

- RDS: verify automated backup retention, deletion protection policy, final
  snapshot policy, and snapshot/restore listing permission.
- Cloud SQL: verify backup configuration, PITR where configured, and backup
  run listing permission.
- Lakebase: verify branch parent/source, TTL, branch existence, and provider
  restore/fork semantics available for the selected project.

Local/self-managed:

- require `pg_dump` or configured backup command before deletion
- restore drill must run outside the source DB and prove catalog readability

Destroy gate:

- `backup_required=false` can only bypass backup gate for explicitly disposable
  data classification.
- `restore_drill_dry_run` never counts as `restore_verified`.
- Managed provider backup-check can mark `backup_status=verified`, but
  `safe_for_destroy=true` requires provider-specific policy or a verified
  restore/fork/snapshot check.

Emergency destroy:

- `force_destroy=true` bypasses the backup gate only for admin users.
- Agent API tokens cannot force destroy.
- The request requires a dual-control marker: operator reason plus admin
  confirmation.
- pg_sage emits an `unsafe_destroy` audit event with reason, actor IDs, provider
  resource ID, and redacted provider state.
- Emergency destroy is intended for runaway-cost or compliance incidents where
  provider backup APIs are unavailable or stuck.

## 7. Cost Tracking

Preflight produces a cost estimate with:

- provider
- region
- size profile
- storage
- backup retention
- estimated monthly cost
- estimated TTL cost
- confidence
- unknown-cost flags

MVP estimates can be heuristic tables stored in versioned YAML/config with
explicit confidence. Later versions can integrate AWS Pricing, Cloud Billing
Catalog, and Databricks billing exports.

Estimates with `confidence=low` or non-empty `unknown_components` are doubled
before budget comparison and require admin override if the doubled estimate
exceeds 50% of the deployment budget.

Budget behavior:

- reject if estimated TTL cost exceeds deployment budget
- warning if monthly equivalent exceeds profile budget
- mark `budget_exceeded` when observed samples exceed budget
- include cost feedback in agent-facing recommendations

## 8. UI and API

### API

Keep existing endpoints and add:

- `POST /api/v1/agent-dbs/{id}/provision/destroy`
- `GET /api/v1/agent-dbs/{id}/provision/preflight-summary`
- `GET /api/v1/agent-dbs/provider-config`
- `PUT /api/v1/agent-dbs/provider-config`
- `GET /api/v1/agent-dbs/terraform-templates`
- `POST /api/v1/agent-dbs/terraform-templates`
- `GET /api/v1/agent-dbs/terraform-templates/{template_id}`
- `POST /api/v1/agent-dbs/terraform-templates/{template_id}/validate`
- `POST /api/v1/agent-dbs/terraform-templates/{template_id}/approve`

`POST /api/v1/agent-dbs/{id}/provision/execute` accepts:

```json
{
  "mode": "dry_run",
  "cost_estimate_id": ""
}
```

`mode=dry_run` preserves current behavior. `mode=live` requires explicit
policy enablement, operator/admin role, and a fresh `cost_estimate_id` returned
by preflight. Cost estimates expire after a short configurable window, default
15 minutes.

### UI

Agent DBs page:

- reduce scrolling by moving the page into task-focused tabs:
  `Deployments`, `Provision`, `Profiles`, `Provider Settings`, `Terraform`,
  and `Activity`
- keep deployment summary metrics and selected deployment status visible in a
  sticky compact header
- move long forms into progressive sections with collapsible advanced fields
- show dry-run and live actions separately
- show live disabled reasons inline
- show provider resource ID and state
- show secret reference, never secret value
- show cost estimate before live create
- show backup/destroy gates
- show branch versus full instance for Lakebase
- keep action history and audit details behind an `Activity` tab instead of
  forcing operators through a long vertical page

Settings page:

- add Cloud Provisioning tab or Agent DB Provider Settings panel
- configure provider enablement and allowlists
- expose readonly provider readiness and last preflight failure

Agent API:

- agents can request deployments
- agents can poll status
- agents can ping liveness
- agents can fetch query tuning recommendations
- agents cannot bypass approval, budget, backup, or live provider policies

### Terraform Template Uploads

Operators can upload their own Terraform definitions as provider templates, but
pg_sage does not blindly apply arbitrary uploaded code.

Supported upload inputs:

- single `.tf`
- single `.tf.json`
- `.zip` containing a root Terraform module

MVP behavior:

- upload stores a template draft with a content hash, file manifest, provider
  list, detected resource types, variable list, outputs, and validation status
- raw templates are never treated as trusted execution code until they pass
  policy checks
- uploaded `*.tfvars`, `*.auto.tfvars`, local state files, `.terraform/`, and
  files containing secret-looking keys are rejected
- `provisioner`, `local-exec`, `remote-exec`, `external` data sources,
  `null_resource`, `local_file`, and unrestricted module sources are rejected
  by default
- allowed resource types are limited to Agent DB provider resources needed by
  pg_sage, for example RDS DB instances, Cloud SQL instances, Databricks
  Lakebase branches/instances, network attachments, subnet group references,
  security group references, and provider-native secret references
- external modules are allowed only when the module source is pinned and on an
  operator allowlist
- validation runs in an isolated temporary workspace with no cloud credentials
  for static checks; live plans require explicit provider config and policy
  approval
- `terraform validate` and `terraform show -json` output are parsed when
  available so pg_sage can show resources, variables, planned changes, and
  policy findings in the UI
- uploaded templates may be used by a size profile through
  `terraform_template_id`
- template execution still flows through the same preflight, cost, approval,
  redaction, and destroy gates as built-in runners

The first release should support templates as reusable, policy-checked
definitions. Direct user-provided `terraform apply` is out of scope.

## 9. Testing Plan

Unit tests:

- state transition matrix
- runner registry selection
- redaction recursion
- redaction fuzzing for nested provider detail
- policy denial reasons
- typed provider error mapping
- provider params normalization
- cost estimate calculations
- idempotency lookup/adoption behavior

Integration tests with fake provider servers:

- AWS RDS fake runner create/status/delete
- Cloud SQL fake runner create/status/delete
- Lakebase fake REST server branch create/status/delete
- crash-after-create reconciliation
- crash-during-create reconciliation using creation receipts
- provider not-found delete reconciliation
- concurrent reconcile from two sidecars does not double-destroy

Live tests behind explicit environment flags:

- `PG_SAGE_LIVE_AWS_RDS=1`
- `PG_SAGE_LIVE_GCP_CLOUDSQL=1`
- `PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1`

Live tests must:

- generate unique deployment IDs
- tag/label resources with TTL and owner
- run create/status/backup-check/destroy
- verify resource is gone or cleanup is requested
- fail loudly if cleanup cannot be verified

Browser tests:

- provider settings page
- disabled live action reasons
- preflight summary
- Lakebase branch/full instance selection
- live execution confirmation modal
- no secret rendering
- Terraform upload validation rejects provisioners, tfvars, state files,
  secret-looking keys, and unpinned external modules
- Agent DB UI tab navigation keeps the primary workflows reachable without
  scrolling through every panel

Security tests:

- no redacted key leaks in attempts, audit, API, logs, or UI
- malicious provider params cannot inject commands
- invalid regions/accounts/projects are denied
- public IP and broad authorized networks are blocked by default

## 10. GA Exit Criteria

GA is not just "the button creates a database." GA requires:

- dry-run remains available and safe
- live create/status/delete are implemented through provider SDK/REST APIs
- RDS, Cloud SQL, and Lakebase branch live E2E tests pass through pg_sage API
- all live resources are tagged/labeled with deployment and TTL
- reconcile handles resource exists/state stale, resource gone/state stale, and
  partial delete
- crash-during-create is recoverable from creation receipts
- no secrets leak in test fixtures, logs, DB rows, API, or UI
- backup/restore gate blocks unsafe deletion
- emergency destroy is admin-only, dual-control, and fully audited
- provider settings and policy denials are visible in UI
- Agent DB UI is split into task-focused tabs with long forms collapsed by
  default
- Terraform template upload/import is policy-checked and never applies
  arbitrary uploaded code directly
- operator runbook exists
- cost estimate is shown and enforced before create
- low-confidence cost estimates fail closed or require admin override
- no non-proxy public IP path is allowed by default
- tenant model is explicitly single-tenant admin/operator for MVP
- in-flight cancel exists for operations stuck in `queued` or `provisioning`
- failure messages are actionable enough for an agent to repair request fields

## What Enterprises Deploying Agents Will Need

The baseline feature should assume the enterprise buyer will ask:

- Can I prove which agent created which database and why?
- Can I cap spend per agent, tenant, team, and provider account?
- Can I disable public networking globally?
- Can I use my existing VPC/subnet/security group/network policy templates?
- Can I require approval before live create or before promotion?
- Can I guarantee cleanup of abandoned resources?
- Can I recover from an agent-created DB before deleting it?
- Can I export audit evidence to a SIEM?
- Can I rotate or revoke credentials without deleting the DB?
- Can I enforce data classification rules so production PII is not branched
  into unsafe test environments?
- Can I make agent tuning recommendations available over API so agents repair
  poor queries themselves?
- Can I see which branch or disposable DB has become long-lived and expensive?

## MVP Scope

The first go-live implementation should include:

- provider config schema and UI
- Agent DB UI task tabs and Terraform template upload microslice
- runner registry
- dry-run runner refactor
- provider resource naming and creation receipts
- typed provider errors
- AWS RDS live runner
- GCP Cloud SQL live runner
- Lakebase branch live runner
- live execute and live destroy endpoints
- redaction
- policy gates
- cost estimate heuristics
- backup checks
- reconcile
- live E2E tests behind flags
- operator runbook

Full Lakebase provisioned instance support can remain behind a feature flag
until the target workspace API is validated for that shape.

MVP tenancy is single-tenant at the pg_sage operator boundary: admin/operator
users can see and manage all Agent DB deployments. Tenant-scoped RBAC,
per-tenant delegates, and team-level resource ownership are post-GA work.

## Out of Scope

- Cloud schema/database-level provisioning. Cloud remains instance-level only.
- HNSW experiment planner. That remains a separate product.
- Automatic production promotion without human/operator approval.
- Provider resource creation through `aws`, `gcloud`, or `databricks` CLI.
- Direct `terraform apply` of arbitrary user-uploaded modules without pg_sage
  validation, approval, and policy gates.
- Full cloud billing ingestion in the first live execution slice.
- Multi-tenant authorization beyond admin/operator.
- Real snapshot-restore-to-temp-instance drills; MVP verifies restore
  capability and provider backup configuration, not full restore execution.
- Cross-region replication and DR automation.
- Agent credential rotation.
- SIEM export.
- Lakebase branch promotion workflow.
