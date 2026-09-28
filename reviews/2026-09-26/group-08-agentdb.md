# Group 08 — AgentDB

Reviewer: G8 agent, 2026-09-26, worktree `claude/full-review-ai-sre-2026-09-26` @ `b396595`.
READ-ONLY review. All paths are relative to `sidecar/` unless prefixed `docs/`.

## Scope (packages/files reviewed, LOC)

| Area | Files | LOC (non-test) |
|---|---|---|
| Core store/state machine | `internal/agentdb/*.go` (55 files) | ~12.1k (20.3k incl. tests) |
| REST API | `internal/api/agent_db_*.go` (11 files) | ~2.1k |
| Runtime wiring | `cmd/pg_sage_sidecar/agentdb_fleet.go`, `agentdb_secret_ref.go`, `main.go:2045-2123`, `wire.go:50-60` | ~300 |
| UI | `web/src/pages/AgentDBsPage.jsx`, `web/src/pages/agentdb/*` (17 files) | ~3.5k |
| Docs checked | `docs/reverse_spec/05-agentdb.md`, `docs/agent-db-deployments.md`, `docs/neon-supabase.md` | — |

Read end-to-end: provisioning (register → plan → authorize-live → execute), live destroy
(authorized and TTL), lifecycle claim/teardown SQL, all five provider runners, ping-token auth,
request/blueprint/template approval, cost/budget, backup assurance, fleet sync, and every UI action
handler that calls the API.

**Headline:** the *authorization* layer for live mutations (plan hash + estimate + single-use
authorization + mutation lease) is carefully built. Almost every **P0 lives around it**: identity of
the cloud resource (derived names, upsert-on-register, no ownership check), lifecycle states that
fall out of every sweep (archived / failed / "deleted"), and a ping token that can rewrite
lifecycle status. Net effect: **pg_sage can leak live cloud resources indefinitely and, under name
collision or a shared cloud account, destroy a resource it does not own.** The UI cannot perform a
live create or destroy at all.

---

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G8-B01 | P0 | CONFIRMED | `internal/agentdb/store.go:189-203,333-362`; `identity.go:264-274` | Ping token (lowest-privilege, unauthenticated route) can set **any** deployment status, e.g. `deleted` hides a live cloud DB from the list, TTL cleanup, and destroy; default heartbeat `active` un-archives, clears teardown/claim ids, and clears `budget_exceeded` |
| G8-B02 | P0 | CONFIRMED | `lifecycle_claim.go:14-26`; `lifecycle.go:9-73`; `api/agent_db_handlers.go:392-401` | A deployment that becomes `archived` is **never revisited** by the TTL reconciler. Blocked (restore-required is the default), manual archive, the UI "Cleanup expired" button, and batch abort on the first provider error all strand live cloud resources forever |
| G8-B03 | P0 | CONFIRMED | `queries.go:34-80`; `provision_links.go:47-73,97-147,149-181` | `Register` is an unconditional `ON CONFLICT (deployment_id) DO UPDATE`: any re-register/re-provision with the same id (agent retry; default derived ids) wipes `provider_resource_id`, `live_mode`, teardown state and resets to `planned` → the live resource is orphaned and can never be destroyed by pg_sage; also rewrites `tenant_id` |
| G8-B04 | P0 | CONFIRMED (path) | `aws_rds_runner.go:130-152`; `gcp_cloudsql_runner.go:111-133`; `lakebase_runner.go:103-126`; `provider_naming.go:22-27`; `teardown_mutation.go:21-25` | Live destroy targets `"pgsage-"+normalize(deployment_id)` when `provider_resource_id` is empty, with **no ownership/tag check**, from a `dry_run_ready` deployment that never created anything. Normalization collides (case, punctuation, 63-char truncation) and is identical across pg_sage installs sharing a cloud account → deletes another tenant's/install's instance; `skip_final_snapshot` is taken from the *requesting* deployment |
| G8-B05 | P0 | CONFIRMED (design) | `api/agent_db_handlers.go:15-46`; `docs/agent-db-deployments.md:79-120` | No tenant isolation. Every management route is global `admin|operator`; docs tell agents to call it with a session cookie; `tenant_id`, `allowed_regions`, `created_by`, `approved_by`, `reviewed_by` are all caller-supplied. Agent A can list, approve, destroy, archive, and mint ping tokens for agent B's databases |
| G8-B06 | P0 | CONFIRMED (code) / PLAUSIBLE (provider) | `execution.go:195-211`; `hosted_runner.go:89-100,115-133`; `provider_runner.go:180` | Ambiguous create → orphan. `ExecuteProvisionLive` forces `failed` on any runner error (overriding the hosted runner's deliberate `status_unknown`); `failed` is neither destroyable nor swept; hosted runners cannot `Status` without a recorded id; Supabase/Neon project names are not unique so the retry creates a second billed project |
| G8-B07 | P1 | CONFIRMED | `aws_rds_runner.go:185-225,280-303`; `live_execution_issue.go:66-83` | AWS region policy bypass: allowlist is checked against `provider_params.region`, but `CreateDBInstance` always runs in the runner's env region; `RDSCreateInput.Region` is never sent |
| G8-B08 | P1 | CONFIRMED | `provider_cost.go:66-83,108-110`; `provider_policy.go:73-81`; `agent_db_live_authority.go:126`; `store.go:206-250` | Cost ceiling bypass: any unknown instance class is priced at a flat $100/month; "doubled" low-confidence estimates are never doubled and the review flag is neutralised because reviewer == requester; `extend-lease` has no TTL cap or re-estimate, so a 1-hour approved estimate becomes unbounded spend |
| G8-B09 | P1 | CONFIRMED | `runtime_registry.go:43-58`; `gcp_cloudsql_runner.go:218,227-236` | GCP runner is built from a static `PG_SAGE_GCP_ACCESS_TOKEN` (OAuth tokens expire ~1h) → after an hour every create/status/**destroy** fails; Cloud SQL instances are created with deletion protection ON unless `metadata.disposable` (nothing sets it) → pg_sage destroy can never succeed; no DB credentials are delivered, so the instance is unusable by the agent |
| G8-B10 | P1 | CONFIRMED | `store.go:81-109`; `provision_links.go:97-181,211-230`; `api/agent_db_handlers.go:488-520` | Policy-gate bypasses: operator "approve" flips a policy **deny** to allow; `POST /agent-dbs` registers cloud instances with no request/decision; approved requests are not single-use; request/blueprint `provider_params` override the approved spec; an approved Terraform template places no constraint on what is provisioned |
| G8-B11 | P1 | CONFIRMED | `web/src/pages/AgentDBsPage.jsx:600-616`; `operations.go:162-210` | Restore-verification (the only destroy safety gate) is self-attested by a UI button with no drill. `RecordBackup` upserts by `backup_id`, so a POST to deployment X can flip deployment Y's backup to `restore_verified`, with the audit row written to X |
| G8-B12 | P1 | CONFIRMED | `store.go:434`; `teardown_mutation.go:37-44`; `live_execution_destroy.go:7-49` | `agentdb.require_backup_before_destroy: false` is ineffective: `normalizeRegister` forces `backup_required=true` and the teardown SQL still demands `restore_verified`. The destroy returns 409 "idempotency conflict" **after** consuming the single-use authorization |
| G8-B13 | P1 | CONFIRMED | `AgentDBsPage.jsx:124-133,543-567`; `agent_db_live_authority.go:333-367` | UI "Live execute"/"Destroy live" send an obsolete body (`mode`, `cost_estimate_id`, region) and never call `authorize-live` → always HTTP 400. Live provisioning is unusable from the UI |
| G8-B14 | P1 | CONFIRMED | `AgentDBsPage.jsx:271,293`; `api/agent_db_handlers.go:488-515`; `docs/neon-supabase.md:91-97` | The UI sends `secret_ref` on register (and docs say to set it) but the register handler drops it; no endpoint can set `secret_ref`. Hosted fleet monitoring cannot be enabled |
| G8-B15 | P1 | CONFIRMED | `api/agent_db_provider_handlers.go:9-15`; `provider_readiness.go:28-35,50-70` | `GET /agent-dbs/providers` calls `ProviderReadinessList(ctx)` with no options → every cloud provider is always reported `runtime_dependencies_missing`, even when fully configured |
| G8-B16 | P1 | CONFIRMED (code) / high-confidence (PG lock level) | `schema.go:19-46`; 53 `s.Ensure` call sites | `Store.Ensure` has no memoisation: every call (including the unauthenticated `agent-ping`) runs the full DDL set, including 35 `ALTER TABLE … ADD COLUMN IF NOT EXISTS` (these take `ACCESS EXCLUSIVE` locks), under a global advisory lock. All AgentDB operations serialise, and readers get blocked. Without a meta-DB this runs on a monitored production DB |
| G8-B17 | P1 | CONFIRMED | `schema.go:55-90,120-146`; `cmd/…/wire.go:50-60` | `local_postgres` provisioning runs `CREATE SCHEMA`/`CREATE DATABASE` on `authPool` (the **first connected monitored DB** when there is no meta-DB), bypassing trust/emergency-stop. It adopts existing objects (`IF NOT EXISTS`; swallows "already exists"), creates no role or credentials, and never drops anything |
| G8-B18 | P1 | CONFIRMED | `lifecycle.go`, `execution.go`, `live_execution_destroy.go` (no reference) | Emergency stop and trust level are not consulted by any AgentDB mutation, including the autonomous TTL live-destroy every 300s |
| G8-B19 | P1 | CONFIRMED | `hosted_plans.go:47-51`; `hosted_api.go:78-96` | Supabase project mode sets every project's `db_pass` from one env var → all tenants' Supabase DBs share a password |
| G8-B20 | P1 | CONFIRMED | `blueprint.go:386-482` vs `aws_rds_runner.go:280-303`, `gcp_cloudsql_runner.go:238-276` | The approved Terraform differs from what the runner creates: `deletion_protection`, `multi_az`, PITR, private network and engine version are rendered but ignored, and the identifier is hardcoded to `pg-sage-agentdb` |
| G8-B21 | P2 | CONFIRMED | `cmd/…/agentdb_fleet.go:21-38,86-151`; `main.go:1257-1261` | Fleet sync never removes archived/destroyed/deleted DBs. It doesn't bootstrap `sage.snapshots`, so collector persistence fails every cycle. Inline defaults to `sslmode=disable`; no runner emits `host`, so the inline path is dead; RDS secret ARNs are never resolved; `rctx` is ignored (`connectMonitoredDB` uses Background). A 60s collector keeps Neon/Supabase scale-to-zero computes awake |
| G8-B22 | P2 | PLAUSIBLE | `provider_errors.go:55-80`; `aws_rds_runner.go:319-336` | Substring error classification: "rate" matches "operation"/"generate"/"migrate"; any "404"/"not found" (including project-level) during destroy marks the resource **destroyed**. RDS final snapshot `<id>-final` collides on re-create → destroy fails forever; final snapshots are never cleaned (storage cost) |
| G8-B23 | P2 | CONFIRMED | `lifecycle.go:124-226` | `ReconcileLiveProvisioning`: a `destroying` row without a teardown id stays Blocked forever on NotFound (only `CheckProvisionStatusLive` maps NotFound→destroyed); `destroy_pending` without a teardown id hits an invalid transition every pass; status checks are counted as `DestroyDryRun` in results/logs |
| G8-B24 | P2 | CONFIRMED | `identity.go:295-330`; `schema_statements.go:192-230`; `auth_middleware.go:97-126` | The unauthenticated `agent-ping` writes 2 rows per failed attempt (failure + audit). Lockout is per token hash, so random tokens are never rate-limited; no retention on `agent_db_pings`, `ping_token_failures`, `audit`, `provision_attempts` |
| G8-B25 | P2 | CONFIRMED | `AgentDBsPage.jsx:136,158`; `list_page.go:10,60-72` | UI renders only the first 100 deployments (ordered by id) and ignores `next_cursor`, so live resources past 100 are invisible |
| G8-B26 | P2 | CONFIRMED | `agent_db_blueprint_handlers.go`, `agent_db_terraform_template_handlers.go`, `agent_db_deploy_request_handlers.go:54,92`; `AgentDBsPage.jsx:639-643`; `store.go:139`, `operations.go` | Audit identity is spoofable (`approved_by`/`reviewed_by` from the body; UI hardcodes `"operator"`); every `s.audit` error is discarded (`_ =`) |
| G8-B27 | P2 | CONFIRMED | `live_execution_destroy.go:40-62` | Destroy consumes the authorization before `prepareDirectTeardown` validates state (a wasted auth on 409); the destroy receipt requires non-empty `ProviderResourceID`, so a successful destroy of a derived-name resource returns 400 |
| G8-B28 | P2 | PLAUSIBLE | `blueprint.go:386-523`; `terraform_policy.go:98-126` | Terraform rendering uses Go `%q`, which does not neutralise HCL `${…}`/`%{…}` in LLM-derived fields (region, class, extensions). Zip import caps each file but not the total decompressed size |
| G8-B29 | P2 | CONFIRMED | `policy.go:46-57`; `operations.go:281-296` | `budget_exceeded` ("Action: pause") takes no provider action and is reset by the next heartbeat (see B01); negative `cost_usd` samples are accepted |
| G8-B30 | P3 | CONFIRMED | various | Hygiene; see the B30 section below |

### G8-B01 — Ping token can rewrite lifecycle status (P0)
- **Scenario:** an agent holds only a ping token. It calls `POST /api/v1/agent-dbs/{id}/agent-ping`
  (session auth skipped, `auth_middleware.go:109`) with `{"status":"deleted"}`.
  `AgentPing` → `Ping` → `setStatusFields(status)` writes any string with no validation.
  `List`/`ListPage` filter `status <> 'deleted'`, so the deployment disappears from UI and fleet sync.
  `claimExpiredSQL` only selects `active|budget_exceeded`, so TTL never fires.
  `beginProviderMutation` refuses `deleted`, so the destroy-live path returns 409. The live RDS/Cloud
  SQL instance runs forever, invisible. Any other string (`"foo"`) escapes TTL while staying visible.
- The **benign default** is also harmful: `PingRequest.Status==""` becomes `"active"`, which un-archives,
  clears `cleanup_claim_id`, `teardown_operation_id`, `provider_mutation_id` (no provisioning-status
  guard, unlike `ExtendLease`), and clears `budget_exceeded`. A heartbeat during `destroying` strips the
  teardown id (feeds B23).
- **Root cause:** heartbeat and lifecycle status share one column and one setter.
- **Fix:** `Ping` must never write `status`. Store the agent-reported health in a separate
  `agent_status` column, validated against an enum (`healthy|degraded|busy`), and only update
  `last_ping_at`. Lifecycle status changes go through dedicated operator methods that check
  `provisioning_status`.
- **Test:** `TestAgentPingCannotChangeLifecycleStatus`: for each of `deleted|archived|active|foo`,
  on an archived deployment with a teardown id, assert status, cleanup claim, and teardown id are
  unchanged and `claimExpired` still selects it.

### G8-B02 — Archived deployments are never reconciled (P0, cost leak)
- **Scenario 1 (the default):** `normalizeRegister` forces `backup_required=true`, and the only way to get
  `restore_verified` is a manual POST. A live RDS deployment's lease expires. `claimExpiredDeployments`
  sets `status='archived'`, and `authorizeCleanup` returns `ErrRestoreRequired` → `Blocked`. The next pass
  selects only `active|budget_exceeded`, so the row is never seen again, even after an operator records
  a verified restore. The instance runs forever.
- **Scenario 2:** the operator clicks **"Cleanup expired"** (`AgentDBsPage.jsx:506`) →
  `POST /agent-dbs/cleanup` → `ArchiveExpired` only claims and archives, and never tears down. Every
  expired live DB is now permanently exempt from TTL destroy.
- **Scenario 3:** manual **Archive** (`setStatus('archived')`) sets no `cleanup_claim_id`, so
  `authorizeTeardownSQL` can never match.
- **Scenario 4:** in `reconcileExpiredDeployment` the live branch does `return err` (`lifecycle.go:51`)
  for any error not classified by `appendCleanupError` (e.g. `ErrInvalid` from GCP deletion
  protection, `ErrRateLimited` from a held mutation lease). `ReconcileAbandonedDeployments` then returns
  an **empty result**, dropping every other deployment archived in the same batch. They are stranded.
- **Fix:** make teardown a durable state machine and not a one-shot side effect of the archive
  transition. The sweep should select `status='archived' AND provisioning_status IN
  (available,status_checked,dry_run_ready,status_unknown) AND provider<>'local_postgres'` every
  pass, record `blocked_reason` and `next_attempt_at` on the row, and never abort the loop on a
  per-row error. `/cleanup` should call `ReconcileAbandonedDeployments`, or be removed. Manual
  archive should set a claim.
- **Tests:** `TestArchivedBlockedDeploymentIsRetriedAfterRestoreVerified`,
  `TestReconcileContinuesAfterProviderError` (3 expired, the 2nd runner errors → 1st and 3rd are still
  destroyed), `TestCleanupEndpointDoesNotExemptFromTeardown`.

### G8-B03 — Register upsert orphans live resources (P0)
- **Scenario:** following the docs, an agent POSTs `/api/v1/agent-dbs` with
  `deployment_id=adb_orders_run_001`, and later retries the same POST (network retry, or re-run). The
  deployment was already live-created (`provider_resource_id=pgsage-adb-orders-run-001`,
  `live_mode=true`, `available`). The upsert sets `provider_resource_id=''`, `live_mode=false`,
  `provisioning_status='planned'` (or `registered`), and `status='active'`. Now:
  (a) TTL expiry takes the dry-run path (`LiveMode` false), so the real instance is never destroyed;
  (b) `planned` is not destroyable;
  (c) a new live create gets `DBInstanceAlreadyExists` → `failed`.
  The same happens for `ProvisionApprovedRequest` (`dep_`+hash(request)), `ProvisionFromBlueprint`
  (`dep_`+hash(blueprint, agent)) and `ProvisionFromTerraformTemplate` when called twice without an
  explicit id. The upsert also rewrites `tenant_id`/`agent_id`, which transfers ownership.
- **Fix:** plain `INSERT … ON CONFLICT DO NOTHING RETURNING`. On conflict, load the row; if the
  immutable identity (tenant, agent, provider, level, plan hash) matches, return it (idempotent);
  otherwise `ErrConflict`. Never let register touch `provider_resource_id`, `live_mode`,
  `provisioning_status`, or teardown fields of an existing row.
- **Test:** `TestRegisterSameIDDoesNotResetLiveDeployment` and `TestRegisterSameIDDifferentTenantConflicts`.

### G8-B04 — Destroy by derived name without ownership check (P0, wrong resource)
- **Scenario A (collision):** tenant 1 has a live deployment `Orders.Run` → RDS `pgsage-orders-run`.
  Tenant 2 registers `orders-run` (the same normalised name), runs a dry-run execute → `dry_run_ready`,
  and an admin authorizes a live destroy for it. `validateDestroyLiveRequest` does not require
  `LiveMode` or a creation receipt, and `prepareDirectTeardown` accepts `dry_run_ready`.
  `AWSRDSRunner.Destroy` gets `ProviderResourceID==""` → derives `pgsage-orders-run` →
  `DeleteDBInstance` on **tenant 1's instance**. If tenant 2's metadata has `disposable:true`, it is
  deleted with **no final snapshot**. GCP and Lakebase have the same shape.
- **Scenario B (shared account):** staging and prod sidecars in one AWS account use the same
  deployment id. The same path deletes the other install's DB.
- The same derivation in `Status` lets `ReconcileLiveProvisioning` **adopt** a foreign instance
  (writing its endpoint and `secret_ref` ARN into the wrong deployment).
- **Root cause:** identity is derived rather than recorded, and there is no ownership proof. The hosted runner
  already does this correctly (`hosted_runner.go:115-133`: requires the recorded id, verifies
  name/scope, refuses default/protected).
- **Fix:** Destroy/Status must require a recorded `provider_resource_id` backed by a live creation
  receipt, and must verify provider tags/labels `pg_sage_deployment_id==dep.DeploymentID` and a new
  `pg_sage_install_id` before deleting. Reject destroy when `live_mode=false`. Make names
  collision-free (append a short hash of the raw deployment id, as Neon/Supabase already do) and
  enforce `UNIQUE(provider, provider_resource_id)`.
- **Tests:** `TestDestroyRefusesDerivedIdentityWithoutReceipt`, `TestDestroyRefusesTagMismatch`,
  `TestProviderResourceNameCollisionFree("Orders.Run","orders-run")`.

### G8-B05 — No tenant isolation (P0)
- **Scenario:** as documented (`docs/agent-db-deployments.md:79-120`), agents call the management API
  with `-b cookies.txt`. That is an operator session, and `RequireRole("admin","operator")` is the only
  check. Agent A can `GET /api/v1/agent-dbs` (all tenants; `tenant_id` is an optional filter), `POST
  /{B}/ping-tokens` (mint B's token), `/archive`, `DELETE`, `/requests/{B}/approve`, and so on. Request
  policy trusts the body: `allowed_regions` comes from the caller, so an agent chooses its own
  allowlist (`policy.go:20`).
- **Fix:** introduce agent principals (API key/JWT bound to `agent_identities.tenant_id`), derive
  `tenant_id` from the principal and not the body, scope every store query by tenant, and keep
  approve/deny/authorize for human roles only. Server-side policy (`AllowedRegions`) must come from
  config/provider policy, never from the request.
- **Test:** `TestAgentPrincipalCannotReadOtherTenant`, `TestRequestAllowedRegionsIgnoredFromBody`.

### G8-B06 — Ambiguous create orphans resources (P0)
- **Scenario:** the Supabase project-mode `CreateResource` POST succeeds, but the response lacks the
  expected name/scope (or JSON decode fails, or the 30s request deadline fires mid-call).
  `HostedRunner.Create` returns `status_unknown`, and `ExecuteProvisionLive` rewrites it to `failed`
  (`execution.go:207-211`). `failed` is neither swept (`lifecycle.go:156-160`) nor destroyable
  (`provider_runner.go:180`). A re-authorized retry creates a **second** project (names are not
  unique), and both bill. Even if the row stayed `provisioning`, hosted `Status` refuses without a
  recorded id, so the row stays Blocked forever. The AWS path (SDK error after the request was
  accepted) has the same `failed` outcome.
- **Fix:** honour the runner's status (`status_unknown` stays `status_unknown`), include it in the
  reconcile sweep, and give every runner an adopt-by-tag lookup (list by
  `pg_sage_deployment_id` label/name) before any create retry.
- **Test:** `TestAmbiguousCreateStaysStatusUnknownAndIsReconciled`,
  `TestHostedRetryAdoptsExistingByTagInsteadOfCreating`.

### G8-B07 — AWS region allowlist does not bind the actual region (P1)
- `issueNormalizedLivePlan` builds the plan from `provider_params.region` and the policy checks it,
  but `awsRDSSDKClient.CreateInstance` uses the client configured for `PG_SAGE_AWS_REGION`, and
  `input.Region` is unused. So `region=us-east-1` (allowlisted) creates in `eu-west-1`, a
  data-residency violation. **Fix:** build a per-region client from `input.Region`, or reject when
  `input.Region != r.region`. **Test:** a fake client asserts the region used equals the plan region.

### G8-B08 — Cost ceiling bypass (P1)
- `rdsCost` with `db.r6i.32xlarge` gives `unknownCost` = $100/mo, confidence low. The TTL-prorated
  estimate for 1h is $0.14, which passes any `max_estimated_cost_usd`. `EstimatedCostDoubled` only
  sets `RequiresReview` when `!Approved`, and `Approved` is `ReviewerID!=""`, where reviewer is the
  requester (`agent_db_live_authority.go:126`), so it never triggers. Then `extend-lease` (operator)
  extends to years with no `MaxTTLSeconds` check or re-estimate, and the RDS `ttl` tag is not
  updated.
- **Fix:** deny unknown classes (fail closed) or require an explicit price. Actually double
  low-confidence estimates. Separate reviewer from requester. Cap `ExtendLease` by the effective
  policy `MaxTTLSeconds` and re-evaluate the cost gate (or require re-authorization) for
  live deployments. Wire `BudgetGate` against `budget_usd` (see D04).
- **Test:** `TestUnknownInstanceClassDeniedLive`, `TestExtendLeaseCappedByPolicyTTL`.

### G8-B09 — GCP runner decays after 1h; destroy impossible; no credentials (P1)
- `registerCloudSQLFromEnv` wraps a static bearer token. Cloud SQL Admin returns 401 after expiry.
  TTL destroys go `status_unknown` and retry every 300s forever. Independently,
  `DeletionProtection: !isDisposable(dep)` is true for every deployment created from the UI/API
  (nothing sets `metadata.disposable`), so `DELETE` is refused even with a valid token. No user or
  password is created and `secret_ref` is empty, so the agent cannot log in.
- **Fix:** use `golang.org/x/oauth2/google.DefaultTokenSource` (ADC) with refresh. Before delete, PATCH
  `deletionProtectionEnabled=false` inside the authorized destroy, gated by the same authorization.
  Create a user with a generated password stored in Secret Manager and return its reference.
- **Test:** a token-source fake that expires; a destroy on a protected instance performs the patch
  first.

### G8-B10 — Policy gates can be bypassed (P1)
1. `SetRequestDecision("approved")` sets `policy_decision='allow'` regardless of the prior `deny`
   (e.g. PHI without a masking policy).
2. `POST /api/v1/agent-dbs` provisions a cloud instance plan with no request at all; the UI itself
   creates a request, then registers directly (`AgentDBsPage.jsx:284-306`).
3. `ProvisionApprovedRequest` never marks the request consumed. The UI generates a fresh id per
   click (`uniqueDerivedID`), so double-click gives two deployments and two potential live instances.
4. `profileFromBlueprint` copies `overrides` first and uses `setIfMissing` for the approved spec, so the
   provision body overrides region/class of an approved blueprint.
5. `ProvisionFromTerraformTemplate` uses the request's provider/params and ignores the template body,
   so approval is a blank check.
- **Fix:** policy `deny` is terminal for operators (admin override with a reason). Register for cloud
  instances requires an approved, unconsumed request/blueprint id, consumed atomically. The
  approved spec wins over overrides (overrides may only narrow).
- **Tests:** one per bypass.

### G8-B11 — Restore gate is self-attested; cross-deployment backup tamper (P1)
- UI `markRestoreVerified` posts `status:'restore_verified'` with `detail.source:'operator_ui'`.
  No restore ever runs (`PlanRestoreDrillDryRun` explicitly does not grant it). `RecordBackup`'s
  `ON CONFLICT (backup_id) DO UPDATE` keeps the original `deployment_id`, so
  `POST /{X}/backups {backup_id:<Y's id>, status:restore_verified}` makes **Y** destroyable, with the audit on X.
- **Fix:** reject `restore_verified` from the generic endpoint. Only a restore-drill runner (or an
  admin attestation with evidence URI plus a second approver) may set it. Scope the upsert to
  `WHERE agent_db_backups.deployment_id = EXCLUDED.deployment_id` (else conflict). The UI button should
  be relabelled "Attest restore (admin)" or removed.
- **Test:** `TestRecordBackupCannotModifyOtherDeploymentsBackup`.

### G8-B12 — `require_backup_before_destroy:false` does nothing; burns the authorization (P1)
- The config default is `true`. When an operator sets it `false`, `ValidateLiveDestroyPrerequisites`
  skips, then `ClaimLiveExecution` consumes auth, then `prepareDirectTeardown` SQL (`NOT backup_required
  OR EXISTS restore_verified`) matches 0 rows, because `backup_required` is always true, giving
  `ErrConflict`. The API reports "idempotency conflict" (misleading) and the auth is gone.
- **Fix:** drive `backup_required` from the effective policy (or honour the request's value), and
  validate all SQL-side preconditions before consuming. Map this case to `ErrRestoreRequired`.

### G8-B13 — UI live create/destroy cannot work (P1)
- `liveExecuteBody` sends `{mode:'live', cost_estimate_id:'ui-…', region,…}` and the destroy sends `{}`.
  The server requires `plan_hash, estimate_id, authorization_id, idempotency_key` from a prior
  `authorize-live`, which no UI code calls. `completeLiveAttempt` fails, giving 400 "invalid request".
- **Fix:** the UI must call `authorize-live` (admin), show the plan and estimate for confirmation, then
  execute with the returned tuple. **Test:** Playwright/vitest flow asserting that both calls are made.

### G8-B14 — `secret_ref` can't be set (P1)
- `hostedSecretReference` validates `env:NAME` and spreads it into the register body. The handler never
  reads `secret_ref`/`secret_ref_provider`. The docs instruct users to set it; no endpoint exists.
- **Fix:** accept `secret_ref` on register (validated `env:` + an allow-listed prefix such as
  `PG_SAGE_AGENTDB_`, so an operator cannot point it at `SAGE_DATABASE_URL`/the meta-DB DSN), plus a
  `PATCH /{id}/secret-ref`.

### G8-B15 — Provider readiness always "missing" (P1)
- The handler passes no options, so `providerReadinessWithoutRuntime()` runs. The full implementation
  (registry + effective policy) is only exercised by tests. **Fix:** pass the route's `registry`,
  `authority.config`, and persisted provider configs.

### G8-B16 — `Ensure` DDL on every call (P1)
- `Ensure` runs ~100 statements including 35 `ALTER TABLE … ADD COLUMN IF NOT EXISTS`, which
  acquire `AccessExclusiveLock` before the existence check. It runs 1–3 times per request
  (`Delete` → `Ensure`+`Get`+`Backups`) and on the unauthenticated ping. Consequences: all AgentDB
  traffic is serialised; any open reader blocks it and queued ACCESS EXCLUSIVE blocks later
  readers. Inside `ReconcileLiveProvisioning` the open cursor on `lockConn` plus `Get`→`Ensure` on
  another connection can self-block when the result exceeds socket buffers (PLAUSIBLE).
- **Fix:** `sync.Once`-style memoisation per pool, with a version row (`sage.agent_db_schema_version`),
  and run migrations only at startup.
- **Test:** count `Ensure` DDL executions across 100 pings (expect 1).

### G8-B17 — local_postgres mutates a monitored DB (P1)
- Without a meta-DB, `authPool = FleetMgr.PoolForDatabase("all")`, which is the first connected
  monitored DB. `CREATE DATABASE`/`CREATE SCHEMA` run there with no trust gate. `schema_name:"public"`
  or `"sage"` adopts existing schemas, and `database_name:"app_prod"` adopts an existing DB ("already
  exists" is swallowed). No role/grant/credential is produced, so the "provisioned" DB is unusable,
  and nothing is ever dropped. If "first connected" changes between restarts, all `sage.agent_db_*`
  state moves to another DB (a fleet-mode leak).
- **Fix:** require a meta-DB (or an explicit `agentdb.local_target` DSN) for AgentDB. Refuse adoption
  (`CREATE SCHEMA` without IF NOT EXISTS, and treat 42P04 as conflict unless the row is ours). Create a
  scoped role with a generated password. Implement DROP behind the same teardown claim.

### G8-B18 — No emergency-stop gate (P1)
- The fleet `EmergencyStop` and trust ramp are ignored by `ExecuteProvisionLive`,
  `ExecuteDestroyProvisionLive`, local DDL, and the autonomous reconciler. **Fix:** a single
  `mutationAllowed(ctx)` check (global emergency stop plus an `agentdb.autonomous_teardown` flag) before
  every provider/DDL mutation, with the reconciler recording `Blocked: emergency_stop`.

### G8-B19 — Shared Supabase DB password (P1)
- `PasswordFunc = staticToken(PG_SAGE_SUPABASE_DATABASE_PASSWORD)` for every project. **Fix:**
  generate a per-project password, store it in the configured secret backend, and persist only the
  reference.

### G8-B20 — Approved artifact ≠ created resource (P1)
- Reviewers approve Terraform with `deletion_protection=true`, `multi_az`, PITR, and private networking.
  The SDK runner creates single-AZ with no deletion protection, the default VPC/SG, and the default engine
  version. **Fix:** either execute the approved plan's parameters in the runner (and add
  `MultiAZ`, `EngineVersion`, `DBSubnetGroupName`, `VpcSecurityGroupIds`), or show the runner's
  effective create input as the approval artifact. Derive Terraform identifiers from the resource name.

### G8-B21 — Fleet integration is one-way and broken (P2)
- There is no removal path (`syncAgentDBsToFleet` only adds). The collector persists into `sage.snapshots` of
  the agent DB, which was never bootstrapped, so every cycle errors (in-memory `latest` still updates).
  No runner emits `host`, so inline eligibility never matches. RDS/GCP secret refs are never resolved.
  `connectMonitoredDB` ignores the reconcile ctx. For scale-to-zero providers, a 60s collector defeats
  autosuspend (a cost increase).
- **Fix:** reconcile the fleet set (add, update endpoint, remove on non-active). Persist agent snapshots in
  the meta-DB. Use an adaptive cadence (this is exactly what D03 was built for).

### G8-B22–B29
Covered in the table above; each fix is local:
- B22: typed provider errors / HTTP status codes instead of substrings; use snapshot `<id>-final-<ts>` plus a retention sweep.
- B23: handle NotFound→destroyed in the reconcile status path and fix the counters.
- B24: global per-IP limiter on the ping route, a `ping_token_failures` retention job, and read or delete `agent_db_pings`.
- B25: paginate.
- B26: take identity from `UserFromContext` and surface audit write failures.
- B27: validate before consuming; persist the receipt with the derived id.
- B28: reject `${`/`%{` in rendered values and cap the total zip size.
- B29: budget hard-limit → an authorized stop/destroy proposal.

### G8-B30 — P3 hygiene
- `readMap` ignores JSON decode errors: a malformed `/provision/execute` body silently runs a dry-run.
- Concurrent identical request creates hit the unique violation and return 500 instead of replaying.
- `agentDBProvisionStatusHandler` returns 400 "invalid request" when the live runner is unavailable after
  a restart; it should be `ErrRunnerUnavailable`.
- `Restore` resurrects `deleted`/`destroyed` rows to `active`.
- The CleanupDecision reason "lease expired without a fresh ping" is misleading, because pings never extend leases.
- Deleted default size profiles are re-seeded on the next `Ensure`.
- `AgentDBsPage.jsx` is 808 lines (limit 500). `agent_db_handlers.go` has lines over 100 chars.
- `TestEnsure*` (3 tests) fail instead of skip without a DB.
- `live_product_chain_test.go:328` ST1011.

---

## B. Dead / unwired / half-built

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G8-D01 | `blueprint.go:15-53,262-358` | `HeuristicBlueprintGenerator` + `infer*` | TEST-ONLY → move | Prod correctly fails closed: `newAgentDBBlueprintGenerator` returns nil without an LLM → `ErrBlueprintLLMRequired` (503). It is used by agentdb **and** api tests, so move it to `internal/agentdb/agentdbtest` (not `_test.go`, which is invisible cross-package). Note tests then exercise a generator prod never uses. |
| G8-D02 | `execution.go:248-267` | `Store.DestroyProvisionLive` | DELETE | Superseded by the authorized `ExecuteDestroyProvisionLive`. An exported destroy with no authorization tuple is a footgun. Port `lifecycle_concurrency_test.go` to the authorized path. |
| G8-D03 | `monitoring_claims.go` (all), `monitoring_schedule.go:17`, `monitoring_schema.go` (cols `monitoring_mode`, `execution_mode`, `wake_idle_allowed`; 3 tables) | Durable adaptive monitoring scheduler/claims | WIRE | No worker exists and no code writes `monitoring_mode`/policies. It is the right fix for B21 (bounded concurrency, secret_ref JIT resolution, no waking scale-to-zero). If not scheduled this quarter, DELETE (≈600 LOC + 3 tables). |
| G8-D04 | `provider_cost.go:36-60` | `BudgetGate` | WIRE | The only check comparing the estimate to the per-deployment `budget_usd`. Wire it into `IssueLiveExecutionRecords` after fixing the doubling (B08). |
| G8-D05 | `api/agent_db_handlers.go:15-26,50-52` | `registerAgentDBRoutes`, `agentDBSubrouter` | TEST-ONLY → move to `_test.go` | 54 API tests use them with `authority=nil` and `DefaultRunnerRegistry`, i.e. **not** the prod wiring. Switch tests to `registerAgentDBRoutesWithAuthority`. |
| G8-D06 | `provider_runner.go:193-195` | `queued→cancel_requested→cancelling` transitions | DELETE | Nothing emits them; no async queue. |
| G8-D07 | `provider_readiness.go:36-49,90-…` | Policy-aware readiness | WIRE | Only tests call it with options (B15). |
| G8-D08 | `schema_statements.go:192`; `store.go:195-201` | `agent_db_pings` | WIRE or DELETE | Written on every heartbeat and never read; unbounded. Either show the last N in the detail view with retention, or drop it and keep `last_ping_at`. |
| G8-D09 | `cmd/…/agentdb_fleet.go:27-37,48-70` | Inline-credential fleet path | DELETE (or map `endpoint`) | No runner produces `host`/`password`, and redaction strips passwords anyway. |
| G8-D10 | `schema.go:55-146` | Local schema/database provisioning | Half-built → finish or narrow | No role, credentials, or teardown (B17). |
| G8-D11 | `deploy_requests.go` | Deploy (migration) requests | Keep, relabel | Review-only metadata; nothing executes or verifies. The UI "Promotion approved" implies more than it does. |
| G8-D12 | `config.go:132` `require_backup_before_destroy` | Config key | WIRE properly | It has no effect when false (B12). |
| G8-D13 | `lifecycle.go:124` owner `agentdb` | Declared reconfigure owner | Note | No owner is registered, so the key fails closed to restart (OK). Either register an owner that resets the ticker, or drop the owner mapping. |

---

## C. Feature improvements (ranked by impact × effort)

**1. Resource identity & ownership (highest impact, M).**
Current: derived names, register upsert, no tag verification (B03/B04/B06).
Proposed:
- Record `provider_resource_id` plus an install id tag on create.
- Every Status/Destroy verifies tags; add `UNIQUE(provider, provider_resource_id)`.
- Use collision-free names with a hash suffix for all providers.
- Adopt-by-tag before retrying a create.
This single change closes three P0s.

**2. Durable teardown state machine (high, M).**
Current: teardown is a side-effect of the archive transition (B02).
Proposed:
- Add columns `teardown_state`, `blocked_reason`, `next_attempt_at`, `attempts`.
- The reconciler sweeps by state and not by transition, with per-row isolation and exponential backoff.
- Expose "Blocked teardowns" as an alert (notify) and a UI badge with the live monthly burn.
- `/cleanup` becomes "reconcile now".

**3. Agent principals & tenant scoping (high, L).**
Current: agents use operator cookies (B05).
Proposed:
- Agent API keys bound to `agent_identities` (scopes: `request`, `ping`, `extend-lease` within policy, `read-own`).
- Humans approve.
- All store queries take a `TenantScope`.
- MCP tools for agents would ride on the same principal.

**4. Heartbeat/lease semantics (high, S).**
Current: status-overwriting ping (B01).
Proposed:
- The ping updates only `last_ping_at` and `agent_status`.
- Optional policy `auto_extend_on_ping` bounded by `MaxTTLSeconds`, so abandonment is *real* abandonment.
- Add `grace_seconds` before archive.

**5. Cost governance that binds (high, M).**
Current: flat $100 for unknown classes, TTL-prorated compare, unbounded extend (B08, B29).
Proposed:
- Explicit price table, or deny unknown classes.
- Compare the **monthly** run-rate against `max_monthly_usd` in addition to the TTL cost.
- Extend-lease re-evaluation.
- Automatic metering from provider billing tags (AWS Cost Explorer by `pg_sage_deployment_id`; GCP billing export by label) to replace agent-self-reported samples.
- `budget_exceeded` produces a destroy/stop proposal in the actions queue.

**6. Make the UI able to do the live flow (medium, S).**
- Fix B13.
- Add a plan/estimate confirmation modal with the effective policy diff.
- Show real provider readiness (B15).
- Paginate (B25).
- Show "orphan risk" (live resource and non-active status).

**7. Credential handoff (medium, M).**
- AWS: resolve the Secrets Manager ARN just-in-time for the fleet collector.
- GCP: create a user with the secret in Secret Manager (B09).
- Supabase: per-project passwords (B19).
- Lakebase: OAuth token handoff.
- A single `SecretResolver` interface used by fleet sync and the D03 worker.

**8. Monitoring integration (medium, M).**
- Wire D03 as the scheduler.
- Persist snapshots in the meta-DB and not in the agent DB.
- Remove fleet entries on archive.
- Scale-to-zero-aware cadence (`wake_idle_allowed`).
- Attach analyzer in **advisory-only** mode so agent DBs get findings, which fits the "recommendations for agents" product claim. Today recommendations are agent-self-posted only.

**9. Schema init hygiene (medium, S).** Run migrations once at startup with a version table (B16).
This removes a DoS vector and lock storms.

**10. Approval integrity (medium, S).**
- Server-derived actor ids.
- No self-approval: reviewer ≠ requester for live authorization in `approval` mode.
- Approved spec is immutable; overrides may only narrow (B10, B26).

**11. Runner fidelity (medium, M).** Honour the approved plan fields (Multi-AZ, engine version,
networking, deletion protection with an authorized unprotect-before-destroy) so approval reviews
reality (B20). Use typed errors via `smithy.APIError`/HTTP status (B22).

**12. Local provider (low–medium, S).** Require a meta-DB or an explicit target and refuse adoption;
create scoped roles; add drop on teardown (B17). Otherwise mark local as "registration only".

---

## D. Questions the user isn't asking

1. **Who pays when pg_sage dies?** AWS RDS and Cloud SQL resources carry no provider-side TTL. Only
   Lakebase branches self-expire. If the sidecar is down or the meta-DB is lost, nothing destroys them.
   Should create also install a provider-native backstop (AWS EventBridge Scheduler delete, or at least a
   `pg_sage_expires_at` tag plus a documented janitor script)?
2. **Is "restore verified" ever true?** No code path performs a restore. With `backup_required` forced
   on, safe automated teardown is impossible without a human clicking a checkbox that lies. Either build
   a real restore drill (RDS `RestoreDBInstanceToPointInTime` into a throwaway plus a `SELECT 1` check, then
   destroy) or drop the pretence and rely on final snapshots.
3. **What is the blast radius of one sidecar's cloud credentials?** The AWS runner uses the default
   credential chain, i.e. whatever the host role can do, including deleting non-pg_sage instances. Is a
   tag-conditioned IAM policy (`rds:DeleteDBInstance` only where `aws:ResourceTag/app=pg-sage`)
   documented and required? It would have contained B04.
4. **Two installs, one account:** is `pg_sage_install_id` part of the product? Staging and prod
   sidecars will collide on names today.
5. **Is AgentDB meant to be multi-tenant SaaS or single-team?** The data model says tenant-first. The
   auth model says single operator team. The answer decides whether B05 is a P0 or a doc fix.
6. **What does an agent actually receive to connect?** Only AWS returns a secret ARN, and nothing hands
   the agent credentials. Is the product "pg_sage provisions, the agent brings its own creds"? If so,
   the local/GCP paths are incomplete.
7. **Should the TTL reconciler be autonomous at all by default?** It destroys cloud resources every 5
   minutes, with no emergency-stop hook (B18) and no notification. The rest of pg_sage has a trust
   ramp.
8. **Do we alert on orphans?** There is no metric for live-but-non-active resources, blocked teardowns,
   or monthly burn per tenant. These are the numbers a buyer asks for first.
9. **Snapshot cost:** every non-disposable RDS destroy leaves a final snapshot forever. Who deletes it,
   and when?
10. **Is 12k LOC the right size for a feature with no production users?** The live-authority layer is
    excellent, but half the surface (D03, D10, D11, Terraform execution) is unwired. Would it be
    better to cut to one provider done end-to-end (create, creds, monitor, TTL destroy, alert) before
    expanding?

---

## E. Verification notes

**Ran:**
- `go build ./...` succeeded.
- `go vet ./internal/agentdb/ ./cmd/pg_sage_sidecar/` was clean.
- `go test -count=1 ./internal/agentdb/`: 86 pass, **71 SKIP** (need Postgres), and 3 FAIL
  (`TestEnsureConcurrentColdStores`, `TestEnsureDatabaseLockCancellationAndRecovery`,
  `TestEnsureSeedFailureRollsBackSchemaAndReleasesLock`), which hard-fail on the missing DB instead of
  skipping. Coverage is 38.4%, dominated by skips. The baseline (`raw-baseline-unit.txt`) shows the same
  package FAIL.
- `go test -count=1 -run AgentDB ./internal/api/`: 6 pass, **28 SKIP**.
- `go test -run Agent ./cmd/pg_sage_sidecar/` fails on the missing meta-DB fixture.
- Per the brief, I did not run integration/e2e or touch Postgres/Docker.

**Not verified (external behaviour; PLAUSIBLE items):**
- Supabase/Neon project-name uniqueness (B06).
- The exact GCP error text for deletion protection (B09/B22).
- The Lakebase create-branch response shape (LRO name vs branch name; if it is an operation name,
  `ProviderResourceID` is empty and every Lakebase create becomes "failed"). Branch TTL bounds the cost.
- The `ALTER TABLE … ADD COLUMN IF NOT EXISTS` AccessExclusive acquisition (standard PG behaviour;
  not measured here).
- The self-block in `ReconcileLiveProvisioning` depends on result size versus socket buffers.

**Doc drift:**
- `docs/reverse_spec/05-agentdb.md` §3.1 calls the heuristic generator "LIVE". It is test-only.
- §5.2 says secret_ref deployments are "skipped". The env-ref path exists but can't be configured (B14).
- `docs/neon-supabase.md:91-97` instructs setting a `secret_ref` for which no API exists.
- `docs/agent-db-deployments.md` shows agents using operator cookies (B05).
- The reverse spec's statement that "the reconciler is scheduled" is accurate (`main.go:2049`, first run after one interval, only when `AuthPool!=nil`).
