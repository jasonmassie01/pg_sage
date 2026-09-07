# Agent DB Live Provisioning GA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build safe live cloud provisioning for Agent DBs across AWS RDS,
GCP Cloud SQL, and Databricks Lakebase, with policy gates, redaction, cost,
backup, reconciliation, UI controls, and live E2E validation.

**Architecture:** Keep the existing Agent DB store/API/UI, but replace
command-shaped dry-run execution with a provider-runner registry. Live
execution is opt-in through config and policy, while dry-run remains the
default and compatibility path.

**Tech Stack:** Go sidecar, pgx/Postgres schema, AWS SDK for Go v2, Google
Cloud SQL Admin REST/client, Databricks REST API, React/Vitest/Playwright,
existing pg_sage config and Agent DB API patterns.

---

## File Structure

Create:

- `sidecar/internal/agentdb/provider_runner.go`: common runner interfaces,
  operation/request/result structs, registry, and provider-neutral errors.
- `sidecar/internal/agentdb/provider_policy.go`: live provisioning policy
  evaluation, allowlists, rate gates, TTL/budget/network decisions.
- `sidecar/internal/agentdb/provider_redaction.go`: recursive secret redaction.
- `sidecar/internal/agentdb/provider_cost.go`: heuristic cost estimates.
- `sidecar/internal/agentdb/terraform_templates.go`: Terraform template
  persistence, manifest extraction, validation summaries, and policy findings.
- `sidecar/internal/agentdb/terraform_policy.go`: static Terraform guardrails
  for provisioners, module sources, resource allowlists, and secret-looking
  files.
- `sidecar/internal/agentdb/provider_errors.go`: typed provider errors with
  actionable repair hints.
- `sidecar/internal/agentdb/provider_naming.go`: provider-safe resource name
  derivation and validation.
- `sidecar/internal/agentdb/aws_rds_runner.go`: AWS RDS SDK runner.
- `sidecar/internal/agentdb/gcp_cloudsql_runner.go`: Cloud SQL Admin runner.
- `sidecar/internal/agentdb/lakebase_runner.go`: Databricks Lakebase REST runner.
- `sidecar/internal/agentdb/live_test_helpers_test.go`: fake provider runner
  helpers and DB cleanup helpers.
- `sidecar/internal/api/agent_db_provider_config_handlers.go`: provider
  settings endpoints.
- `sidecar/internal/api/agent_db_terraform_template_handlers.go`: upload,
  validate, approve, and list Terraform templates.
- `sidecar/web/src/pages/agentdb/ProviderSettingsPanel.jsx`: UI for live
  enablement and provider allowlists.
- `sidecar/web/src/pages/agentdb/AgentDBWorkspaceTabs.jsx`: compact tab shell
  for the Agent DB page.
- `sidecar/web/src/pages/agentdb/TerraformTemplatePanel.jsx`: upload and
  review Terraform templates.
- `e2e/agentdb-live-provisioning.spec.ts`: flag-gated browser/live smoke.
- `docs/runbooks/agentdb-live-provisioning.md`: operator runbook.

Modify:

- `sidecar/internal/agentdb/types.go`: add config/result types.
- `sidecar/internal/agentdb/schema.go`: add provider config table and state
  indexes, provider resource fields, secret reference fields, and creation
  receipts, plus Terraform template tables.
- `sidecar/internal/agentdb/execution.go`: replace direct dry-run runner use
  with registry-driven operations and checked state transitions.
- `sidecar/internal/agentdb/lifecycle.go`: replace provider-blind reconcile
  with provider-aware reconcile and advisory locking.
- `sidecar/internal/agentdb/backup_assurance.go`: inject provider runners for
  managed backup checks.
- `sidecar/internal/agentdb/providers.go`: keep deterministic plans but remove
  any implication that plans are executable shell commands.
- `sidecar/internal/api/agent_db_execution_handlers.go`: inject runner registry
  and add live endpoints.
- `sidecar/internal/api/agent_db_handlers.go`: register provider config and
  live execution routes.
- `sidecar/internal/api/router.go`: build registry from config.
- `sidecar/internal/config/config.go`: add `agentdb` live provisioning config.
- `sidecar/web/src/pages/AgentDBsPage.jsx`: fetch provider settings and wire
  live actions, workspace tabs, and template upload.
- `sidecar/web/src/pages/agentdb/CloudProvisioningPanel.jsx`: separate dry-run
  and live actions and show disabled reasons.
- `sidecar/web/src/pages/agentdb/AgentDBProvisioningPanels.jsx`: expose
  provider settings UI or link to settings.
- `sidecar/web/src/pages/AgentDBsPage.test.jsx`: add live-disabled,
  live-enabled, and no-secret tests.
- `sidecar/web/src/pages/agentdb/AgentDBWorkspaceTabs.test.jsx`: tab workflow
  tests.
- `sidecar/web/src/pages/agentdb/TerraformTemplatePanel.test.jsx`: upload and
  validation UX tests.

---

## Task 1: Execution Contract and State Machine

**Files:**

- Create: `sidecar/internal/agentdb/provider_runner.go`
- Create: `sidecar/internal/agentdb/provider_redaction.go`
- Create: `sidecar/internal/agentdb/provider_errors.go`
- Create: `sidecar/internal/agentdb/provider_naming.go`
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Modify: `sidecar/internal/agentdb/execution.go`
- Test: `sidecar/internal/agentdb/execution_test.go`
- Test: `sidecar/internal/agentdb/provider_redaction_test.go`
- Test: `sidecar/internal/agentdb/provider_naming_test.go`

- [ ] **Step 1: Write failing state transition tests**

Add tests that assert:

- `planned -> preflight_passed -> provisioning -> available`
- `planned -> preflight_failed`
- `available -> destroy_pending -> destroying -> destroyed`
- `queued -> cancel_requested -> cancelling`
- `provisioning -> status_unknown`
- duplicate live create returns `ErrConflict`
- destroy from `planned` returns `ErrInvalid`

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestProvisionStateTransitions|TestLiveExecuteRejectsInvalidTransitions|TestProviderResourceName|TestRedactProviderDetail"
```

Expected: fail because the new transition validator and live states do not
exist yet.

- [ ] **Step 2: Add operation/result types and transition validator**

Implement `ProvisionOperation`, `ProvisionRequest`, `ProvisionResult`,
`ProviderRunner`, `RunnerRegistry`, typed provider errors, resource naming,
and:

```go
func validProvisionTransition(from, to string) bool
func requireProvisionTransition(from, to string) error
```

- [ ] **Step 3: Add schema fields and creation receipts**

Append idempotent schema changes in `schema.go`:

- `provider_resource_id text NOT NULL DEFAULT ''`
- `secret_ref text NOT NULL DEFAULT ''`
- `secret_ref_provider text NOT NULL DEFAULT ''`
- `secret_ref_expires_at timestamptz`
- `live_mode boolean NOT NULL DEFAULT false`
- `sage.agent_db_creation_receipts`

Receipt columns: deployment ID, provider, provider resource ID, region,
account/project/workspace, request hash, operation mode, created_at, updated_at.

- [ ] **Step 4: Route existing dry-run execution through safe helpers**

Keep existing API behavior, but make every provisioning status update pass
through the validator, every provider detail pass through the redactor, and
every live-intent transition write a creation receipt before calling a provider.

- [ ] **Step 5: Add command-runner adapter for compatibility**

Add a `commandRunnerAdapter` so existing tests using `recordingProvisionRunner`
and `failingProvisionRunner` keep compiling while live runners migrate to the
new interface.

- [ ] **Step 6: Rerun focused tests**

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestProvisionStateTransitions|TestLiveExecuteRejectsInvalidTransitions|TestProviderResourceName|TestRedactProviderDetail"
```

Expected: pass.

---

## Task 2: Provider Runner Registry and Dry-Run Compatibility

**Files:**

- Modify: `sidecar/internal/agentdb/provider_runner.go`
- Modify: `sidecar/internal/agentdb/execution.go`
- Modify: `sidecar/internal/agentdb/lifecycle.go`
- Modify: `sidecar/internal/agentdb/backup_assurance.go`
- Modify: `sidecar/internal/api/agent_db_execution_handlers.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`
- Test: `sidecar/internal/agentdb/execution_test.go`

- [ ] **Step 1: Write failing registry tests**

Test that:

- unknown provider returns `ErrInvalid`
- dry-run registry handles all cloud providers
- handler uses injected registry instead of hard-coded
  `DryRunProvisionRunner{}`
- reconcile and backup assurance use injected registry instead of hard-coded
  dry-run runner
- in-flight operations keep their original runner if config reload swaps the
  registry for future operations

Run:

```powershell
go test -count=1 ./internal/agentdb ./internal/api -run "TestRunnerRegistry|TestAgentDBExecuteUsesInjectedRunner"
```

Expected: fail.

- [ ] **Step 2: Implement `RunnerRegistry`**

Registry API:

```go
func NewRunnerRegistry(fallback ProviderRunner) *RunnerRegistry
func (r *RunnerRegistry) Register(runner ProviderRunner)
func (r *RunnerRegistry) ForProvider(provider string) (ProviderRunner, error)
```

- [ ] **Step 3: Inject registry into API routes**

Update route construction to accept a registry. In current router setup, build
dry-run-only registry unless live config is enabled.

- [ ] **Step 4: Inject registry into reconcile and backup assurance**

Replace the remaining dry-run injection points in lifecycle/reconcile and
backup assurance. Store the selected runner name on the attempt so audits show
which implementation handled the operation.

- [ ] **Step 5: Rerun focused tests**

Run the command from Step 1. Expected: pass.

---

## Task 3: Credential Config, Policy Gates, and Provider Settings

**Files:**

- Create: `sidecar/internal/agentdb/provider_policy.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Modify: `sidecar/internal/config/config.go`
- Create: `sidecar/internal/api/agent_db_provider_config_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Test: `sidecar/internal/agentdb/provider_policy_test.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] **Step 1: Write failing policy tests**

Cover:

- global live disabled denies create
- provider disabled denies create
- region allowlist deny
- account/project/workspace allowlist deny
- empty live-mode allowlist denies every request unless it contains `*`
- missing TTL deny
- public IP deny by default
- live approval reuses the existing Agent DB deploy-request review workflow
- admin override records reason but does not bypass secret redaction

Run:

```powershell
go test -count=1 ./internal/agentdb ./internal/api -run "TestLiveProvisionPolicy|TestProviderConfigAPI"
```

Expected: fail.

- [ ] **Step 2: Add config structs**

Add `AgentDBConfig` and nested provider structs to config parsing with safe
defaults:

- `live_provisioning_enabled=false`
- provider enabled flags false
- `allow_public_ip=false`
- `require_backup_before_destroy=true`
- empty allowlists mean deny-all in live mode; `["*"]` explicitly means any

- [ ] **Step 3: Add provider config persistence/API**

Store non-secret settings only. Reject any provider config payload containing
secret-looking keys.

- [ ] **Step 4: Add policy evaluator**

Return structured `PolicyDecision` with `allowed`, `reasons`,
`disabled_reasons`, `requires_approval`, rate-limit dimension, and the
existing deploy-request approval reference when approval is required.

- [ ] **Step 5: Rerun focused tests**

Run the command from Step 1. Expected: pass.

---

## Task 4: Recursive Redaction and Audit Hardening

**Files:**

- Create: `sidecar/internal/agentdb/provider_redaction.go`
- Modify: `sidecar/internal/agentdb/execution.go`
- Modify: `sidecar/internal/agentdb/audit.go`
- Test: `sidecar/internal/agentdb/provider_redaction_test.go`

- [ ] **Step 1: Write failing redaction tests**

Test nested maps, arrays, mixed case keys, DSNs, and provider result details.
Assert no value containing `password`, `token`, `secret`, or connection DSN
survives in provision attempts or audit detail.

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestRedactProviderDetail|TestProvisionAttemptsDoNotLeakSecrets"
```

Expected: fail.

- [ ] **Step 2: Implement recursive redactor**

Redact keys containing:

- `password`
- `token`
- `secret`
- `credential`
- `private_key`
- `connection_string`
- `dsn`
- `access_key`
- `session`

- [ ] **Step 3: Apply redaction before storage and API response**

Redact at the boundary before `recordProvisionAttempt`, `audit`, and JSON
responses.

- [ ] **Step 4: Rerun tests**

Run the command from Step 1. Expected: pass.

- [ ] **Step 5: Add redaction fuzz test**

Run the redactor with arbitrary nested maps and arrays and assert sensitive
keys never survive:

```powershell
go test -count=1 ./internal/agentdb -run TestRedactProviderDetail -fuzz FuzzRedactProviderDetail -fuzztime 10s
```

---

## Task 5: Cost Estimation and Budget Gates

**Files:**

- Create: `sidecar/internal/agentdb/provider_cost.go`
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/provider_policy.go`
- Test: `sidecar/internal/agentdb/provider_cost_test.go`

- [ ] **Step 1: Write failing cost tests**

Cover RDS `db.t4g.micro`, Cloud SQL `db-f1-micro`,
`db-custom-1-3840`, Lakebase branch, unknown size, TTL-cost calculation, and
budget denial.

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestEstimateAgentDBCost|TestBudgetGate"
```

Expected: fail.

- [ ] **Step 2: Implement heuristic estimate tables**

Use versioned YAML/config-backed tables so estimates can be updated without a
binary rebuild. Use low-confidence defaults when exact provider pricing is
unknown. Include `confidence` and `unknown_components`.

- [ ] **Step 3: Feed estimates into preflight policy**

Reject when estimated TTL cost exceeds deployment budget. Warn when monthly
equivalent exceeds size profile budget. Double estimates with `confidence=low`
or non-empty `unknown_components` before comparing against deployment budget,
and require admin override above 50% of budget.

- [ ] **Step 4: Rerun tests**

Run the command from Step 1. Expected: pass.

---

## Task 6: AWS RDS Live Runner

**Files:**

- Create: `sidecar/internal/agentdb/aws_rds_runner.go`
- Modify: `go.mod`
- Test: `sidecar/internal/agentdb/aws_rds_runner_test.go`
- Live test: `sidecar/internal/agentdb/aws_rds_live_test.go`

- [ ] **Step 1: Write fake-client tests first**

Fake the AWS client interface and test:

- preflight validates region, class, storage, backup retention, and network
- create uses deterministic DB identifier
- create sets backup retention, encryption, tags, and deletion protection per
  policy
- status maps AWS statuses to pg_sage states
- destroy uses skip-final-snapshot only for disposable class
- not-found during destroy maps to destroyed
- UUID-style deployment IDs that start with digits are mapped to valid RDS
  identifiers
- `Throttling`, `InstanceQuotaExceeded`, `InvalidParameterValue`, and
  `DBInstanceAlreadyExists` map to typed provider errors

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestAWSRDSRunner"
```

Expected: fail.

- [ ] **Step 2: Add AWS SDK for Go v2 dependency**

Use service-specific imports. Do not shell out to `aws`.

- [ ] **Step 3: Implement runner**

Use SDK default credential chain plus optional configured profile/region.
Prefer RDS-managed Secrets Manager credentials where supported; otherwise
return a secret reference requirement rather than storing password values.
Persist `provider_resource_id` from `provider_naming.go`.

- [ ] **Step 4: Add live test behind flag**

Only runs when `PG_SAGE_LIVE_AWS_RDS=1`. Test create/status/destroy and verify
deletion. Always tag `app=pg-sage`, `ttl`, and `pg_sage_deployment_id`.

- [ ] **Step 5: Rerun fake tests**

Run the command from Step 1. Expected: pass.

---

## Task 7: GCP Cloud SQL Live Runner

**Files:**

- Create: `sidecar/internal/agentdb/gcp_cloudsql_runner.go`
- Modify: `go.mod`
- Test: `sidecar/internal/agentdb/gcp_cloudsql_runner_test.go`
- Live test: `sidecar/internal/agentdb/gcp_cloudsql_live_test.go`

- [ ] **Step 1: Write fake-client tests first**

Cover:

- Enterprise edition required for `db-f1-micro`
- public IP denied unless policy allows
- `authorizedNetworks=0.0.0.0/0` denied
- private IP requires network/private services access params
- public IP requires Cloud SQL Auth Proxy or connector verification
- backup config enabled
- status maps `RUNNABLE` to `available`
- delete operation polling maps not-found to destroyed
- quota, permission denied, already exists, and transient operation failures map
  to typed provider errors

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestCloudSQLRunner"
```

Expected: fail.

- [ ] **Step 2: Add Cloud SQL Admin client**

Use Google auth/ADC. Do not shell out to `gcloud`.

- [ ] **Step 3: Implement runner**

Use Admin API instance insert/get/delete and operation polling. Store only
instance connection name and secret references.

- [ ] **Step 4: Add live test behind flag**

Only runs when `PG_SAGE_LIVE_GCP_CLOUDSQL=1`. Test create/status/backup/delete
with low-cost Enterprise settings.

- [ ] **Step 5: Rerun fake tests**

Run the command from Step 1. Expected: pass.

---

## Task 8: Databricks Lakebase Runner

**Files:**

- Create: `sidecar/internal/agentdb/lakebase_runner.go`
- Test: `sidecar/internal/agentdb/lakebase_runner_test.go`
- Live test: `sidecar/internal/agentdb/lakebase_live_test.go`

- [ ] **Step 1: Write fake HTTP server tests first**

Cover:

- project lookup
- default branch discovery
- branch create with TTL
- long-running operation capture
- branch status ready
- branch delete and not-found verification
- branch-name uniqueness collision fails predictably because Lakebase branch
  rename is not supported
- OAuth token refresh during long-running create refreshes once and does not
  persist the token
- provisioned instance mode denied unless `allow_lakebase_instance=true`
- OAuth token not persisted

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestLakebaseRunner"
```

Expected: fail.

- [ ] **Step 2: Implement REST client**

Use configured workspace host and auth token provider. For MVP, support
Databricks unified auth/profile locally and service principal OAuth in
enterprise config.

- [ ] **Step 3: Implement branch runner**

Create branch under existing project, record source branch, operation name,
branch name, state, TTL, and endpoint metadata if returned.

- [ ] **Step 4: Gate full instance mode**

Return actionable denial unless live instance API shape has been validated in
the configured workspace.

- [ ] **Step 5: Add live test behind flag**

Only runs when `PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1`. Create/status/delete a
branch and verify not found after delete.

- [ ] **Step 6: Rerun fake tests**

Run the command from Step 1. Expected: pass.

---

## Task 9: API/UI Live Execution UX

**Files:**

- Modify: `sidecar/internal/api/agent_db_execution_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Modify: `sidecar/web/src/pages/agentdb/CloudProvisioningPanel.jsx`
- Create: `sidecar/web/src/pages/agentdb/ProviderSettingsPanel.jsx`
- Test: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Test: `sidecar/web/src/pages/ProviderSettingsPanel.test.jsx`

- [ ] **Step 1: Write failing API tests**

Assert:

- dry-run `execute` remains dry-run
- `execute` with `mode=live` and a fresh `cost_estimate_id` uses live runner
- missing, stale, or mismatched `cost_estimate_id` rejects live mode
- live disabled returns structured reason
- `destroy` requires backup/restore gate

Run:

```powershell
go test -count=1 ./internal/api -run "TestAgentDBLiveProvisionAPI"
```

Expected: fail.

- [ ] **Step 2: Write failing UI tests**

Assert:

- live actions are visible but disabled with reasons
- enabled provider shows confirmation modal
- preflight summary shows cost/network/backup warnings
- no secret text is rendered
- Lakebase branch/full-instance choice remains visible

Run:

```powershell
npm --prefix sidecar/web test -- --run src/pages/AgentDBsPage.test.jsx src/pages/ProviderSettingsPanel.test.jsx
```

Expected: fail.

- [ ] **Step 3: Implement API routes**

Keep `execute` as the route and add request body fields:
`mode=dry_run|live` and `cost_estimate_id`. Add `destroy`, provider config
GET/PUT, and preflight summary.

- [ ] **Step 4: Implement UI**

Add provider settings panel and live action controls. Make disabled reasons
clear without instructional wall text.

- [ ] **Step 5: Rerun API and UI tests**

Run commands from Steps 1 and 2. Expected: pass.

---

## Task 9a: Microslice - Agent DB UI Tabs and Terraform Template Upload

**Files:**

- Create: `sidecar/internal/agentdb/terraform_templates.go`
- Create: `sidecar/internal/agentdb/terraform_policy.go`
- Create: `sidecar/internal/api/agent_db_terraform_template_handlers.go`
- Create: `sidecar/web/src/pages/agentdb/AgentDBWorkspaceTabs.jsx`
- Create: `sidecar/web/src/pages/agentdb/TerraformTemplatePanel.jsx`
- Modify: `sidecar/internal/agentdb/schema.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Test: `sidecar/internal/agentdb/terraform_templates_test.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`
- Test: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Test: `sidecar/web/src/pages/agentdb/TerraformTemplatePanel.test.jsx`

- [ ] **Step 1: Write failing Terraform template policy tests**

Assert:

- `.tf`, `.tf.json`, and `.zip` uploads are accepted only under max size.
- `.tfvars`, `.auto.tfvars`, `.tfstate`, `.terraform/`, and secret-looking
  files are rejected.
- `provisioner`, `local-exec`, `remote-exec`, `external` data sources,
  `null_resource`, and `local_file` are rejected.
- external module sources require an allowlisted source and immutable version.
- allowed provider resources produce a summary with providers, resources,
  variables, and outputs.

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestTerraformTemplatePolicy|TestTerraformTemplateManifest"
```

Expected: fail.

- [ ] **Step 2: Write failing API tests**

Assert:

- `POST /api/v1/agent-dbs/terraform-templates` stores a draft and returns a
  template ID.
- invalid uploads return structured policy findings.
- validation never stores secret-looking values.
- approval requires operator/admin role.
- approved template IDs can be referenced by size profile provider params as
  `terraform_template_id`.

Run:

```powershell
go test -count=1 ./internal/api -run "TestAgentDBTerraformTemplateAPI"
```

Expected: fail.

- [ ] **Step 3: Write failing UI declutter tests**

Assert:

- Agent DB page renders tabs: `Deployments`, `Provision`, `Profiles`,
  `Provider Settings`, `Terraform`, `Activity`.
- only the active tab's main panel is visible.
- selected deployment summary remains visible while switching tabs.
- Terraform tab has upload, validation findings, and approval status.

Run:

```powershell
npm --prefix sidecar/web test -- --run src/pages/AgentDBsPage.test.jsx src/pages/agentdb/TerraformTemplatePanel.test.jsx
```

Expected: fail.

- [ ] **Step 4: Add schema and store methods**

Add `sage.agent_db_terraform_templates` with:

- `template_id`
- `name`
- `status`
- `source_kind`
- `content_sha256`
- `files_json`
- `manifest_json`
- `policy_findings`
- `created_by`
- `approved_by`
- `created_at`
- `updated_at`

Store file manifests and summaries, not secret values.

- [ ] **Step 5: Implement Terraform static validation**

Use structured parsing where practical and conservative text scanning for
dangerous Terraform blocks. Optional CLI validation can run only in an isolated
temporary directory with no cloud credentials. Use `terraform validate -json`
for syntax/config checks and `terraform show -json` only for saved plan output
generated by an approved dry-run flow.

- [ ] **Step 6: Implement upload/list/validate/approve API**

Reject invalid uploads before persistence when possible. Persist invalid drafts
only when useful for UI review and only after redaction.

- [ ] **Step 7: Implement tabbed Agent DB workspace**

Move the existing long page into task-focused tabs. Keep summary metrics and
selected deployment status in a compact sticky header. Put audit/provision
attempt history under `Activity`.

- [ ] **Step 8: Implement Terraform upload UI**

Add upload, manifest summary, policy findings, and approval status. Do not
show raw file content by default; provide a redacted preview only for small
files.

- [ ] **Step 9: Rerun focused tests**

Run the commands from Steps 1-3. Expected: pass.

---

## Task 10: Reconcile, Live E2E, Runbook, and GA Gate

**Files:**

- Modify: `sidecar/internal/agentdb/execution.go`
- Modify: `sidecar/internal/agentdb/lifecycle.go`
- Modify: `sidecar/internal/agentdb/operations.go`
- Create: `e2e/agentdb-live-provisioning.spec.ts`
- Create: `docs/runbooks/agentdb-live-provisioning.md`
- Modify: `tasks/todo.md`

- [ ] **Step 1: Write failing reconcile tests**

Cover:

- cloud resource exists but pg_sage status is `provisioning`
- cloud resource not found but pg_sage status is `destroying`
- resource exists without pg_sage row but has pg_sage tags
- expired branch is archived and destroy queued
- unsafe delete remains blocked
- crash-during-create receipt is adopted after restart
- two sidecars running reconcile concurrently do not double-destroy the same
  provider resource

Run:

```powershell
go test -count=1 ./internal/agentdb -run "TestReconcileLiveProvisioning"
```

Expected: fail.

- [ ] **Step 2: Implement provider-aware reconcile**

Use provider status/list-by-tag where available. Never delete orphaned resources
without policy and audit record. Use Postgres advisory locks around reconcile
work so HA or duplicated sidecars cannot issue duplicate destroys.

- [ ] **Step 3: Add Playwright flow**

Use flags and env vars. Flow:

1. login
2. configure provider
3. create Agent DB request
4. preflight
5. execute live
6. poll status
7. backup check
8. destroy
9. verify provider gone

- [ ] **Step 4: Write runbook**

Include:

- required IAM/service principal permissions
- config examples
- emergency stop
- stuck destroy and admin force-destroy with dual-control
- cleanup commands
- cost guardrails
- backup/restore caveats
- known provider limitations

- [ ] **Step 5: Final verification**

Run:

```powershell
go test -race -cover -count=1 -p 1 ./internal/agentdb ./internal/api
go vet ./...
golangci-lint run ./...
npm --prefix sidecar/web test -- --run src/pages/AgentDBsPage.test.jsx
npm --prefix sidecar/web run build
git diff --check
```

Live run only when credentials and cost approval are explicitly present:

```powershell
$env:PG_SAGE_LIVE_AWS_RDS='1'
$env:PG_SAGE_LIVE_GCP_CLOUDSQL='1'
$env:PG_SAGE_LIVE_DATABRICKS_LAKEBASE='1'
go test -count=1 -p 1 ./internal/agentdb -run "TestLive"
npm run test:e2e -- e2e/agentdb-live-provisioning.spec.ts --workers=1
```

Expected: all non-live tests pass; live tests pass only in explicitly enabled
environments and must verify cleanup.

---

## Spec Coverage Checklist

- [x] Execution contract: Tasks 1, 2, 9, 10.
- [x] Provider runner interface: Tasks 2, 6, 7, 8.
- [x] Credential model: Tasks 3, 4, 6, 7, 8.
- [x] Policy and safety: Tasks 3, 5, 9.
- [x] Networking: Tasks 3, 6, 7, 8.
- [x] Backup and restore: Tasks 1, 6, 7, 8, 10.
- [x] Cost tracking: Task 5.
- [x] UI/API: Tasks 9 and 9a.
- [x] Terraform template uploads: Task 9a.
- [x] Testing plan: Tasks 1-10.
- [x] GA exit criteria: Task 10.

## Execution Notes

- Use disjoint subagents by task where possible.
- Do not run live provider tests unless the explicit env flag for that provider
  is set.
- Keep the local 8085 sidecar available for browser validation after each UI
  slice.
- Pause 8085 before Agent DB package tests that mutate shared `adb_*` rows, or
  run those tests against an isolated DSN.
- Do not commit secrets or generated provider credentials.
