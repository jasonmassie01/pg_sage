# AgentDB deployment and provisioning paths: code audit

Date: 2026-10-05. Input to `AGENTDB-SPEC.md`.

Scope: the code paths that create, change, promote and destroy databases, plus the
deployment-adjacent core packages (`clone`, `rollout`, `migration`) and pg_sage's own
enterprise deployment. Audited `origin/master` at `72646ab1` in the worktree
`C:/Users/jmass/pg_sage-agentdb-spec`. Read-only: no cloud API calls, no database
connections, and no file changes other than this one.

Conventions:

- Paths are relative to `sidecar/` unless they start with `docs/`, `reviews/`, `e2e/` or `.`.
- Every `path:line` was read at this commit.
- **CONFIRMED** means the code path was traced end to end.
- **PLAUSIBLE** means the code path was traced, but the trigger depends on provider behavior,
  timing or operator action that could not be checked offline.
- Sections 9 and 10 summarize two sub-audits that ran in parallel. Their key citations were
  re-read and held; the spot checks are listed in each section.

Baseline. The 2026-09-26 review (`reviews/2026-09-26/group-08-agentdb.md`) found 30 AgentDB
bugs, and `reviews/2026-09-26/fixes-agentdb.md` records fixes for most of them. This audit
re-checks the deployment paths after those fixes and after the D4 approval changes of
2026-09-27. When a finding here is what remains of a G8 item, it says so.

---

## 0. Summary

1. **Five live provider runners exist, and none of them runs Terraform.**
   - AWS RDS uses the AWS SDK. Cloud SQL, Lakebase, Neon and Supabase use REST.
   - No production code imports `os/exec`. The `terraform apply -auto-approve` plans are text
     shown in the UI, and the repo has no `.tf` module (section 2.1).
   - Terraform templates are labelled "review only".
2. **The authorization core is careful.**
   - Server-owned plan hash, 15-minute estimate, 10-minute single-use authorization, atomic
     claim, 15-minute provider mutation lease, durable create operation id, creation receipts,
     tag and label ownership checks, and compare-and-swap teardown claims.
   - Most of the remaining risk sits around that core: defaults, provider identity, blind spots
     in reconciliation, and resources that nobody can use.
3. **The defaults switch TTL cleanup off in practice (DP-01, P0).**
   - `require_backup_before_destroy` defaults to true, and every deployment is forced to
     `backup_required`.
   - Only an admin's hand-entered restore-drill attestation grants `restore_verified`.
   - So every expired live resource stays blocked and keeps billing. There is no alert, no
     metric and no UI signal.
4. **Agents cannot use the databases pg_sage creates, and pg_sage does not monitor them**
   (DP-15, DP-16, DP-18).
   - RDS hands out only the master user's Secrets Manager ARN.
   - Cloud SQL is created with no user, no password and no secret.
   - Neither can enter pg_sage's fleet monitoring.
5. **Several paths can orphan billed resources** (DP-02, DP-07, DP-08, DP-09).
   - A provider "not found" seen through the wrong region, project or credentials is recorded
     as `destroyed`.
   - Neon, Supabase and Lakebase creates that fail ambiguously wedge forever.
   - The status reconciler ignores the mutation lease.
   - Two installs that share a cloud account can adopt each other's instances.
6. **Live authority is effectively single-person and fragile** (DP-03, DP-04, DP-05, DP-11).
   - The admin who authorizes is recorded as the reviewer and must also execute.
   - The policy hash embeds the process start time, so authorizations fail after a restart or
     on another replica.
   - Provider policy goes "stale" 30 days after the last save.
   - Destroys are checked against the create policy, so the kill switch also blocks cleanup.
7. **The D4 request path cannot go live with the seeded size profiles (DP-06).**
   `provider_params` in the provision call are silently dropped, and no seeded profile carries
   a region.
8. **Promotion does not exist.**
   - Deploy requests are review-only text records. Nothing runs `migration_sql`, and nothing
     goes through `policy.Gate` or `Executor.Apply` (section 4).
   - The real migration machinery (`migration/plan`, `rehearsal`) is not connected, and its
     rehearsal can never return "promote" anyway (section 9).
9. **Live evidence is old.**
   - Provider runs and a product-chain "gauntlet" passed on 2026-05-09/10.
   - Nothing has run live since the 2026-09-26/27 rewrites. The gauntlet is now stale and
     would fail (DP-28).
   - Neon and Supabase have never run live, and the receipts file is gitignored.
10. **AgentDB reuses none of pg_sage's core.**
    - It imports no other internal package.
    - It has its own approvals, authorization ledger, audit table, tokens, emergency-stop
      reader, leases and cost model (section 11).
11. **Verdict:** stop building instance provisioning.
    - Delete Terraform, the dry-run executor, and the Cloud SQL runner.
    - Freeze RDS.
    - Keep the Neon and Lakebase branch paths and hand TTL, credentials and lookup to the
      providers.
    - Rebuild `local_postgres` as per-agent databases on a sandbox cluster.
    - Move every provider mutation under `policy.Gate` and `Executor.Apply` (section 12).

---

## 1. Map of the provisioning surface

### 1.1 Size and history

| Area | Files | Lines |
|---|---|---|
| `internal/agentdb` (non-test) | 69 | 13,689 |
| `internal/agentdb` tests | 54 | 10,753 (232 `Test*` funcs, 24 files need a DB and skip without one: `store_test.go:22-32`) |
| Deployment-path files in scope (listed in the brief) | 52 | ~9,900 |
| `internal/api/agent_db_*` (non-test) | 14 | 2,638 |

`internal/agentdb` has 39 commits; the last change was 2026-09-27. The live tests are gated by
environment variables:

- `PG_SAGE_LIVE_AWS_RDS`, `PG_SAGE_LIVE_GCP_CLOUDSQL`, `PG_SAGE_LIVE_DATABRICKS_LAKEBASE` and
  `PG_SAGE_LIVE_AGENTDB_GAUNTLET`: `aws_rds_live_test.go:12`, `gcp_cloudsql_live_test.go:12`,
  `lakebase_live_test.go:12`, `live_product_chain_test.go:14`.
- The hosted test sits behind a build tag plus `PG_SAGE_HOSTED_TEST_*`
  (`hosted_live_test.go:1,15-18`).

### 1.2 Provider matrix

| Provider | Runner and API | Enable gates | Provisioning credential | Ownership proof | Provider-native TTL | What the agent can read back | Last live run |
|---|---|---|---|---|---|---|---|
| `aws_rds` | `AWSRDSRunner`, AWS SDK v2: CreateDBInstance, DescribeDBInstances, DeleteDBInstance (`aws_rds_runner.go:214-273`) | `PG_SAGE_LIVE_PROVISIONING=1`, `PG_SAGE_ENABLE_AWS_RDS_RUNNER=1`, region env (`runtime_registry.go:12,25-35`) | AWS default chain, loaded once (`aws_rds_runner.go:56-69`) | Receipt plus tag `pg_sage_deployment_id` (`aws_rds_ownership.go:181-192`) | None; a `ttl` tag only (`aws_rds_runner.go:146,169-174`) | Endpoint host and master-user secret ARN (`aws_rds_runner.go:304-327`) | 2026-05-10 |
| `gcp_cloudsql` | `CloudSQLRunner`, REST `sqladmin` v1beta4 (`gcp_cloudsql_runner.go:233-308`) | `PG_SAGE_ENABLE_GCP_CLOUDSQL_RUNNER=1`, project env (`runtime_registry.go:42-57`) | Metadata-server token or static token (`credentials.go:133-143`) | Receipt plus `userLabels` (`gcp_cloudsql_ownership.go:38-41,61-64`) | None | Connection name, private IP, **no credential** (`gcp_cloudsql_runner.go:186-207`) | 2026-05-10 |
| `databricks_lakebase` | `LakebaseRunner`, REST `/api/2.0/postgres/projects/{p}/branches` (`lakebase_runner.go:216-250`) | `PG_SAGE_ENABLE_LAKEBASE_RUNNER=1`, host, token (`runtime_registry.go:59-79`) | Static bearer token | Receipt only; tags are built but not sent (DP-19) | Branch `spec.ttl` (`lakebase_runner.go:220-225`) | Endpoint and project (`lakebase_runner.go:196-207`) | 2026-05-10 |
| `neon` | `HostedRunner`, REST `console.neon.tech/api/v2` (`hosted_http.go:43-50`) | `PG_SAGE_ENABLE_NEON_RUNNER=1`, `PG_SAGE_NEON_API_KEY` (`hosted_plans.go:38-54`) | Static API key | Receipt, hash-suffixed name, scope, not default or protected (`hosted_runner.go:112-131,152-155`; `provider_naming.go:13-21`) | Not used | Endpoint, project, operator's `env:` reference (`hosted_runner.go:186-196`) | **Never** |
| `supabase` | `HostedRunner`, REST `api.supabase.com/v1` | `PG_SAGE_ENABLE_SUPABASE_RUNNER=1`, access token, master DB password (`hosted_plans.go:39-51`) | Static token; per-project password derived by HMAC (`credentials.go:35-39`) | As for Neon | Not used | As for Neon | **Never** |
| `local_postgres` | Store DDL on the **control** pool (`local_provisioning.go:49,70`) | `PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1` (`local_provisioning.go:16-18`) | pg_sage's own control-DB login | Row; refuses objects that already exist (`local_provisioning.go:114-122`) | None, and never dropped | Schema or database name only (`local_provisioning.go:55,76-78`) | n/a |

### 1.3 End-to-end flow, as built

1. **Agent asks.** `POST /api/v1/agent-api/agent-db-requests` with an `agt_` token
   (`api/agent_db_agent_api.go:56-70,102-121`). This runs `CreateRequest` and intake policy
   (`store.go:17-44`).
2. **Operator approves.** `POST /agent-dbs/requests/{id}/approve`
   (`api/agent_db_request_handlers.go:96-125`; `request_decision.go:26-68`). Any operator can
   approve, including the request's creator.
3. **Operator plans.** `POST /agent-dbs/requests/{id}/provision`
   (`api/agent_db_request_handlers.go:127-175`).
   - This calls `ProvisionApprovedRequest` (`request_provision.go:14-25`), then
     `claimApprovedRequest` (`request_provision.go:163-182`), which makes the request
     single-use.
   - Then `Provision` (`schema.go:94-125`), `BuildProvisionPlan` (`providers.go:8-31`) and
     `Register` (`register.go:11-53`). The row ends in `planned`.
   - Blueprint and template provisioning take the same single-use claim
     (`request_provision.go:83-108`; `provision_links.go:47-150`).
4. **Optional "preflight".** `PreflightProvision` (`execution.go:28-60`) checks the plan's shape
   only. `ProviderRunner.Preflight` has no production caller (`git grep '\.Preflight('`).
5. **Optional "dry-run execute".** `ExecuteProvision` (`execution.go:62-117`) runs
   `DryRunProvisionRunner`, which echoes `"dry run: terraform apply ..."` (`execution.go:13-26`).
   The row moves to `dry_run_ready`.
6. **Admin authorizes.** `POST /agent-dbs/{id}/provision/authorize-live` with
   `{operation: create|destroy}` and an `Idempotency-Key`, admin only
   (`api/agent_db_handlers.go:254-257`).
   - The path is `issueNewLiveAuthorization` (`api/agent_db_live_authority.go:98-144`), then
     `IssueLiveExecutionRecords` (`live_execution_issue.go:25-63`), then
     `PersistLiveExecutionRecords` (`live_execution_store.go:16-53`).
   - It returns a plan hash, an estimate valid for 15 minutes (`live_execution_issue.go:132`)
     and an authorization valid for 10 minutes (`live_execution_issue.go:153`).
7. **The same admin executes.** `POST /agent-dbs/{id}/provision/execute` with the tuple.
   - The path is `authorizedLiveRequest`, which requires the same requester
     (`api/agent_db_live_authority.go:303-336`), then `ExecuteProvisionLive`
     (`live_create.go:14-42`), then `runner.Create`.
8. **Reconciler,** every `agentdb.reconcile_interval_seconds`, default 300
   (`config/defaults.go:196`; `cmd/pg_sage_sidecar/agentdb_reconciler.go:13-68`).
   - `ReconcileAbandonedDeployments` (`teardown_reconcile.go:39-62`).
   - `ReconcileLiveProvisioning` (`lifecycle.go:23-70`).
   - Fleet sync (`cmd/pg_sage_sidecar/agentdb_fleet.go:89-117`).
9. **TTL teardown.** An expired lease is archived and claimed (`lifecycle_claim.go:14-28`). The
   teardown is authorized by compare-and-swap (`lifecycle_claim.go:30-46,73-128`). The provider
   destroy follows (`execution.go:119-233`).
10. **Early destroy.** The admin calls `authorize-live` with `destroy`, then `destroy-live`
    (`live_execution_destroy.go:25-73`).
11. **Row delete.** `DELETE /agent-dbs/{id}` only tombstones the row (`store.go:211-248`). For a
    cloud instance it requires `provisioning_status = 'destroyed'` (`policy.go:79-86`).

---

## 2. Each provisioning flow

### 2.1 "Terraform" and the dry-run executor: text only

- **Plans are argument lists stored in `provisioning_plan`.**
  - `terraform apply -auto-approve -var provider=aws_rds ...` (`providers.go:89-106`) and the
    Cloud SQL equivalent (`providers.go:108-129`).
  - `terraform destroy -auto-approve` (`providers.go:204-236`).
  - Lakebase and the hosted providers get `cloud_api` pseudo-commands (`providers.go:131-156`;
    `hosted_plans.go:5-27`).
- **Nothing executes them.** `git grep '"os/exec"'` matches only test files and `e2e/`, and
  `git ls-files` shows no `*.tf` file. The variables the plans pass (`db_instance_identifier`,
  `instance_name`, ...) refer to a module that does not exist.
- **Every non-live call uses the dry-run runner.** `CommandRunnerForProvider` falls back to
  `DryRunProvisionRunner{}` even when a live runner is registered (`provider_runner.go:92-101`).
  The provision, status and destroy "dry runs" only record the echoed text (`execution.go:373-411`).
- **Uploaded Terraform is reviewed but never applied.**
  - It is scanned by substring match (`terraform_policy.go:35-63`) and stored.
  - The plan is labelled `template_semantics=review_only`, and the runner builds from
    `provider_params` (`provision_links.go:265-275`).
  - Template approval is not tied to a content hash (DP-22).
- **Conclusion:** this whole surface is documentation that looks like execution. Delete it.

### 2.2 AWS RDS (`aws_rds_runner.go`, `aws_rds_ownership.go`)

- **Instance identifier.** `pgsage-<normalized deployment_id>`, cut to 63 characters, with no
  hash suffix (`provider_naming.go:22-23,33-59`).
- **Create input** (`aws_rds_runner.go:103-149`, sent at `aws_rds_runner.go:218-233`).
  - Defaults: class `db.t4g.micro`, 20 GiB, 7-day backups (`aws_rds_runner.go:338-349`).
  - Settings: `StorageEncrypted` (default KMS key), `DeletionProtection=false`,
    `AutoMinorVersionUpgrade`, optional `MultiAZ` and `EngineVersion`,
    `ManageMasterUserPassword=true`, master user `postgres`.
  - The region must equal the runner's process-wide region (`aws_rds_ownership.go:95-110`; the
    G8-B07 fix).
  - A public instance requires the policy flag (`aws_rds_runner.go:120-126`).
  - `deletion_protection=true` is refused (`aws_rds_ownership.go:114-125`).
- **Never set.** A `git grep` finds none of `DBSubnetGroupName`, `VpcSecurityGroupIds`,
  `EnableIAMDatabaseAuthentication` or `CopyTagsToSnapshot`.
  - Instances therefore land in the default VPC with the default security group.
  - `private_network` is only checked for a conflict with public access
    (`aws_rds_ownership.go:120-122`). G8-B20 recorded "subnet group / SG DEFERRED"
    (`reviews/2026-09-26/fixes-agentdb.md:33`).
- **Tags.** `app=pg-sage`, `pg_sage_deployment_id=<raw id>`, and `ttl=<lease expiry>`
  (`aws_rds_runner.go:143-147`). The `ttl` tag is not updated when the lease is extended
  (DEFERRED in `fixes-agentdb.md:21`).
- **Status** (`aws_rds_ownership.go:38-54`).
  - It uses the recorded id. The derived name is used only for an uncertain create
    (`aws_rds_ownership.go:171-179`).
  - Ownership is then checked by tag.
  - Mapping: `available` → available; `creating`, `modifying` or `backing-up` → provisioning;
    `deleting` → destroying; anything else → `status_unknown` (`aws_rds_runner.go:304-313`).
- **Destroy** (`aws_rds_ownership.go:58-90`).
  - A recorded id is required. If `DescribeDBInstances` says NotFound, the result is
    `destroyed`.
  - Otherwise the tags are checked, and `DeleteDBInstance` runs with a final snapshot unless
    `metadata.disposable` is set (`aws_rds_runner.go:258-273`). The snapshot name is unique
    per destroy (`aws_rds_ownership.go:136-142`).
- **Errors** are classified by substring of the AWS message (`aws_rds_runner.go:189-208`).
  Rejections that are definitive become `failed`; everything else is `create_uncertain`
  (`aws_rds_ownership.go:146-153`).
- **Credentials.** The SDK default chain: environment keys, profile, SSO, IMDS, IRSA. It is
  built once at registry construction with a 10-second load timeout
  (`runtime_registry.go:15-17,36-39`). A failed load is swallowed with no log line (DP-23).
- **Time to available:** 513 s create-to-available in the May gauntlet
  (`docs/reports/2026-05-09-agentdb-release-readiness-report.md:355-356`).

### 2.3 GCP Cloud SQL (`gcp_cloudsql_runner.go`, `gcp_cloudsql_ownership.go`)

- **Requests.** Create, get, delete and patch against `/sql/v1beta4/projects/{p}/instances`
  (`gcp_cloudsql_runner.go:233-308`).
  - Create returns `PENDING_CREATE` without following the long-running operation
    (`gcp_cloudsql_runner.go:261-266`).
  - The HTTP client is `http.DefaultClient`, which has no timeout (`gcp_cloudsql_runner.go:335-338`).
- **Create input** (`gcp_cloudsql_runner.go:90-176`).
  - The project comes from `provider_params.project`, else the runner (`gcp_cloudsql_runner.go:178-184`).
  - The region comes from params, else the runner. The runner region defaults to `us-central1`
    (`runtime_registry.go:51`).
  - Tier `db-custom-1-3840`, `ENTERPRISE` edition, 20 GiB, `requireSsl`, backups and PITR on.
  - `deletionProtection` is on unless the deployment is disposable.
  - `multi_az` maps to `REGIONAL` (`gcp_cloudsql_runner.go:157-175`).
  - Public IPv4 requires the policy flag (`gcp_cloudsql_runner.go:135-142`), and `0.0.0.0/0`
    is refused (`gcp_cloudsql_runner.go:143-152`).
- **Never set.** `git grep` finds no `privateNetwork`, `rootPassword`, user creation or IAM-auth
  flag.
  - So a private-only instance cannot be expressed (DP-16).
  - No credential is ever created. Yet the result claims
    `secret_ref_provider: gcp_secret_manager` (`gcp_cloudsql_runner.go:196-205`).
  - G8-B09 deferred "creating a DB user/credential for the agent" (`fixes-agentdb.md:22`).
- **Ownership.** The `pg_sage_deployment_id` label is lower-cased, has illegal characters folded
  to `_`, and is cut at 63 characters (`gcp_cloudsql_runner.go:312-326`). That makes collisions
  more likely than on AWS (DP-09).
- **Destroy.** It checks the labels, then **lifts deletion protection itself**, then deletes
  (`gcp_cloudsql_ownership.go:48-81`). No final backup is taken.
- **Credentials.** Either a metadata-server token, which refreshes and caches
  (`credentials.go:76-124`), or a static `PG_SAGE_GCP_ACCESS_TOKEN` assumed to last one hour
  (`credentials.go:43-72`). There is no support for ADC or a service-account key file.
- **Time and failures.** 875 s create-to-available
  (`docs/reports/2026-05-09-agentdb-release-readiness-report.md:363`). The first live run
  panicked at Go's 10-minute default test timeout while the instance was still
  `PENDING_CREATE` (`docs/reports/2026-05-10-agentdb-release-hardening-report.md:107-110`).

### 2.4 Databricks Lakebase (`lakebase_runner.go`)

- **Branch mode only.** Instance mode is refused unless `allowInstanceMode` is set, and the
  registry always passes `false` (`lakebase_runner.go:143-149`; `runtime_registry.go:78`).
- **Create.**
  - `POST /api/2.0/postgres/projects/{project}/branches?branch_id=pgsage-<id>` with
    `spec.source_branch` and `spec.ttl` (`lakebase_runner.go:216-229`).
  - The TTL is the lease's remaining time. If the lease has already expired, it is clamped to
    one second (`lakebase_runner.go:162-168`).
- **Ownership.** The ownership `Metadata` map is built (`lakebase_runner.go:179-182`) but never
  sent in the request body (`lakebase_runner.go:220-225`). Status and destroy rely only on the
  recorded id and receipt (`lakebase_runner.go:83-126`). A derived name is never adopted, by
  design (`lakebase_runner.go:89-95`).
- **Errors are untyped strings** (`lakebase_runner.go:286-290`), classified by substring in
  `mapProviderError` (`provider_errors.go:78-107`). A 404 at project level during destroy
  counts as `destroyed` (`lakebase_runner.go:114-118`).
- **Response parsing** assumes `name` contains `branches/<id>` (`lakebase_runner.go:296-324`).
  If the create response is an operation object without a branch segment, the result has an
  empty resource id. That becomes `create_uncertain`, which never resolves
  (`live_create.go:152-163`). PLAUSIBLE; it depends on the API's response shape.
- **Credential.** A static bearer token from the environment: a PAT or a pre-minted token, with
  no OAuth M2M refresh (`runtime_registry.go:63-77`).
- **Time:** 32 s create-to-available (`docs/reports/2026-05-09-agentdb-release-readiness-report.md:371`).
  This is the only path whose speed suits agents.

### 2.5 Neon and Supabase (`hosted_*.go`)

- **Neon branch.**
  - `POST /projects/{project}/branches` with `init_source: parent-schema` (schema only, no
    data) and one read-write endpoint fixed at 0.25 CU (`hosted_api.go:41-63`).
  - Readiness is read from the project's endpoints (`hosted_api.go:158-186`).
  - Neon's own branch expiry is not used.
  - The connection URI and role password that Neon returns are discarded
    (`hosted_response.go:34-60`).
- **Neon project.** `POST /projects` with `org_id`, region and database name
  (`hosted_api.go:42-53`).
- **Supabase branch.** `POST /projects/{ref}/branches` with `with_data:false`
  (`hosted_api.go:68-70`). Before creating, the organization's `branching_limit` entitlement is
  checked (`hosted_scope.go:35-59`).
- **Supabase project.** `POST /projects` with a database password derived by HMAC from a master
  secret (`hosted_api.go:65-89`; `credentials.go:35-39`, the G8-B19 fix).
- **Resource name.** `pgsage-<base>-<sha256(id)[:4]>`, which is collision-resistant
  (`provider_naming.go:13-21`).
- **Ownership.** The recorded id, the name and the scope must all match, and default or
  protected branches are refused (`hosted_runner.go:112-160`).
- **Every create error becomes `status_unknown`** (`hosted_runner.go:92-96`), including local
  errors raised before any HTTP call (`hosted_api.go:71-80`). That produces a permanent wedge
  (DP-07).
- **HTTP.** A 30-second timeout, no redirects, and errors typed by status code
  (`hosted_http.go:63-121`).
- **Credentials.** Static tokens from the environment (`hosted_plans.go:38-54`). Plans for these
  providers force `PublicIP=true` (`live_execution_contract.go:68-72`), so using them requires
  the single global `agentdb.allow_public_ip` flag.

### 2.6 `local_postgres` (`local_provisioning.go`)

- **What it runs.** `CREATE SCHEMA "<name>"` or `CREATE DATABASE "<name>"` on the store's own
  pool (`local_provisioning.go:49,70`).
  - That pool is the control database: the meta-DB, or, without one, the first connected
    monitored database. The API store is built on the router's pool (`api/router.go:218-223`)
    and the reconciler on `authPool` (`cmd/pg_sage_sidecar/api_server.go:123-124`); see G8-B17.
  - It is opt-in. It is checked against the emergency stop and validated before the DDL runs.
    It refuses to adopt an existing object (42P06/42P04) and drops the object if registration
    fails (`local_provisioning.go:85-139`).
- **No role or grant management.** No `CREATE ROLE` or `GRANT` exists anywhere in
  `internal/agentdb` (`git grep -i 'CREATE ROLE|GRANT '`).
  - The agent gets a schema name inside pg_sage's own control database, plus whatever login an
    operator hands over.
- **Never dropped.** `Delete` only tombstones the row (`store.go:230-247`). G8-B17 deferred a
  scoped role and `DROP` on teardown (`fixes-agentdb.md:30`).

### 2.7 What the agent gets back

Agents can read only through `GET /api/v1/agent-api/agent-dbs[/{id}]`. That returns the whole
`Deployment` JSON, without secret values (`api/agent_db_agent_api.go:72-100`). Agents cannot
provision, authorize, extend, archive or destroy (`api/agent_db_agent_api.go:11-21`).

| Provider | `connection_info` | Can an agent log in? |
|---|---|---|
| RDS | `endpoint` (host, no port), `secret_ref` = ARN of the AWS-managed **master** secret (`aws_rds_runner.go:319-324`; `aws_rds_ownership.go:26-31`) | Only with IAM `GetSecretValue` on that ARN, and only as the master user. No least-privilege role exists. |
| Cloud SQL | `instance_connection_name`, `private_ip_address` (empty when the instance is public-only) (`gcp_cloudsql_runner.go:200-204`) | No. No user, password or secret was created. |
| Lakebase | `endpoint`, `project` (`lakebase_runner.go:199-202`) | Only with Databricks identity obtained some other way. |
| Neon/Supabase | `endpoint`, `project`, plus the operator's `env:` reference (`hosted_runner.go:186-196`) | Only if an operator copied credentials into that environment variable on the sidecar host. |
| local | `schema_name` or `database_name` (`local_provisioning.go:55,76-78`) | Only with control-DB credentials handed over by hand. |

### 2.8 Credentials pg_sage uses to provision

| Provider | Source | Where stored | Refresh and rotation |
|---|---|---|---|
| AWS | SDK default chain (`aws_rds_runner.go:56-69`) | SDK memory | The SDK refreshes temporary credentials. |
| GCP | Metadata server, or `PG_SAGE_GCP_ACCESS_TOKEN` (`credentials.go:133-143`) | Process memory | The metadata source refreshes (`credentials.go:92-122`). The static token is reported expired after its assumed TTL (`credentials.go:64-72`). |
| Databricks | `PG_SAGE_DATABRICKS_TOKEN` or `DATABRICKS_TOKEN` (`runtime_registry.go:67-77`) | Process memory | None |
| Neon/Supabase | `PG_SAGE_NEON_API_KEY`, `PG_SAGE_SUPABASE_ACCESS_TOKEN`, `PG_SAGE_SUPABASE_DATABASE_PASSWORD` (`hosted_plans.go:38-54`) | Process memory | None. Rotating the Supabase master key changes every derived password. |
| Agent DB login used by fleet sync | `env:PG_SAGE_AGENTDB_*` only (`credentials.go:22-30`) | Process environment; the reference is persisted, not the value (`cmd/pg_sage_sidecar/agentdb_secret_ref.go:21-62`) | Manual |

Provider settings reject keys that look like secrets (`provider_config.go:24-26`;
`provider_policy.go:119-131`), and provider output is redacted before storage
(`provider_redaction.go:37-135`). Neither pg_sage nor the agent databases use Vault or any
cloud secret manager; see section 10.

### 2.9 Safety controls inventory

| Control | Where | Effective? | Gap |
|---|---|---|---|
| Feature gates | Config `agentdb.live_provisioning_enabled` and per-provider `enabled` (`config/config.go:144-160`, defaults off at `:1035-1041`); env gates (`runtime_registry.go:12`) | Yes | — |
| Single-use approved request (D4) | `request_provision.go:36-58,163-182`; `api/agent_db_register_handler.go:23-34` | Yes | Approves tenant, agent, provider, level and budget, but **not** size, region or network. Self-approval is allowed. |
| Trusted size profile (request path) | `schema.go:109-123`, asserted by `store_test.go:192-196` | Yes | Seeded profiles cannot go live (DP-06). Any operator can edit profiles (`api/agent_db_handlers.go:160-175`). |
| Plan, estimate and authorization tuple | `live_execution_contract.go:194-409`; `live_execution_claim.go:12-57` | Yes; integrity is good | Single person (DP-11); restart and replica fragility (DP-03); 30-day staleness (DP-04) |
| Effective policy (3 layers plus the authorization) | `effective_policy.go:50-186`; `provider_policy.go:45-94` | Partly | `allowed_accounts` and `allowed_projects` are checked against caller-shaped params, not the runner's real scope (DP-10). Destroys are checked against create rules (DP-05). |
| Cost ceiling and budget | `cost_guard.go:15-56`; `provider_cost.go:38-125` | Partly | Static price table. HA, edition, I/O and snapshots are unpriced. Total spend under repeated extensions is unbounded (DP-13). |
| Rate limits and quotas | — | **Absent** | The GA spec's `max_live_creates_per_hour/day` and rate-limit dimensions were never built (DP-29). No per-tenant cap on live resources. |
| Provider mutation lease | `teardown_mutation.go:55-99` (15 min, `:11`) | Create and destroy only | Status reconcile ignores it (DP-08). |
| Emergency stop | `store_options.go:50-72` (persisted `sage.config.emergency_stop` plus the in-memory fleet gate for the reconciler, `agentdb_reconciler.go:21-23`) | Yes | It also blocks all TTL teardown. API routes see only the persisted flag (`fixes-agentdb.md:31`). The runbook describes a different stop (section 8). |
| Ownership | `teardown_ownership.go:9-28`, plus runner tag, label and name checks | Mostly | The scope (region, account, project, workspace) is not recorded (DP-02). Tags carry no install id (DP-09). |
| Backup and restore gate | `lifecycle_claim.go:41-45,93-95`; `teardown_mutation.go:40-44`; `restore_drill.go:24-58` | Yes, and too strong | An attestation, not a drill. The default blocks all TTL teardown (DP-01). |
| TTL | Lease (`register.go:104-106`; `queries.go:51`) plus the reconciler | Partly | The TTL starts at registration, not at availability (DP-14). Only Lakebase enforces a provider-side TTL. |
| Network posture | RDS public-IP gate; Cloud SQL IPv4 and `0.0.0.0/0` gates | Weak | No VPC, subnet or private-IP support (DP-15, DP-16). |
| Timeouts | Hosted 30 s (`hosted_http.go:66,72`); reconcile pass 60 s (`agentdb_reconciler.go:47`) | Partly | Cloud SQL and Lakebase use `http.DefaultClient`, and API calls inherit the request context (DP-31). |

---

## 3. Crash, retry and reconciliation

### 3.1 Primitives

| Primitive | Where | Guarantees |
|---|---|---|
| Request claim | `request_provision.go:163-182` | One deployment per approved request. Released if provisioning fails (`:186-196`). |
| Authorization and estimate consumption | `live_execution_claim.go:12-57` | Exactly one caller wins, in one transaction. |
| Idempotent replay | `live_execution_contract.go:206-212`; `api/agent_db_live_authority.go:258-260` | A replay returns the stored receipt only after a successful create. After a failure, the retry gets a 409 and needs a new authorization. |
| Create operation id | `live_create.go:100-109` | Recorded **before** the provider call, together with `live_mode`. |
| Provider mutation lease | `teardown_mutation.go:55-99` | Lasts 15 minutes and expires on its own after a crash. |
| Teardown claim and operation | `lifecycle_claim.go:14-46`; `teardown_mutation.go:16-53` | Compare-and-swap on `lifecycle_version` and state. One active teardown per deployment. |
| Live-reconcile singleton | `lifecycle.go:30-43,164-188` | A session advisory lock on a dedicated connection. |
| Archive claims | `lifecycle_claim.go:14-28`; `teardown_reconcile.go:14-33` | `FOR UPDATE SKIP LOCKED`, so replicas cooperate. |
| Creation receipt | `execution.go:563-596` | One per deployment. **It records no region, account or project** (`live_create.go:181-185`; `lifecycle.go:127-131`). |

### 3.2 Create: crash windows (`ExecuteProvisionLive`, `live_create.go:14-42`)

| After step | Durable state | Recovery | Residual risk |
|---|---|---|---|
| Mutation lease taken (`:24`) | Lease row | Expires in 15 minutes | None |
| Authorization claimed (`:29`) | Authorization and estimate consumed | The caller re-authorizes; a retry gets 409 | Wasted authorization only |
| `beginLiveCreate`, first statement (`:101`) | `provisioning`, no create op, `live_mode=false` | **None.** Reconcile needs a recorded id or a create op (`aws_rds_ownership.go:171-179`), and new creates refuse `provisioning` (`live_create.go:85-89`) | Row wedged; nothing was created. The two `UPDATE`s are not in one transaction (DP-24). |
| Create op recorded (`:104-107`) | `provisioning`, create op, `live_mode` | RDS and Cloud SQL: find by derived name, verify the tag, adopt (`lifecycle.go:103-145`) or mark failed | Lakebase, Neon and Supabase: blocked forever (`lakebase_runner.go:89-95`; `hosted_runner.go:119-121`). **Orphan if the resource exists.** |
| Provider call returns | Same as above until `finishLiveCreate` | As above | As above. A client disconnect cancels the request context mid-call (`api/agent_db_live_authority.go:265-270`). |
| Receipt and outcome written (`:111-148`) | `provisioning` or `available`, resource id | Normal status polling | — |

### 3.3 Destroy: crash windows (`runProviderDestroy`, `execution.go:143-172`)

| After step | Durable state | Recovery |
|---|---|---|
| Teardown authorized (`lifecycle_claim.go:30-46`) | `destroy_pending` with a teardown op | `ReconcileLiveProvisioning` resumes (`lifecycle.go:83-91`; `teardown_ownership.go:32-37`) |
| `destroying` set (`execution.go:158-166`) | `destroying` | Resumed. A repeated delete is safe: RDS returns an invalid-state error and is retried; NotFound becomes `destroyed`. |
| Provider error | `status_unknown` with the teardown op (`execution.go:213-215`) | Retried every pass |
| Provider accepted the delete | `destroying` | A status check maps NotFound to `destroyed` (`lifecycle.go:157-158`; `execution.go:279-282`) |

Gaps: a NotFound seen through the wrong scope is accepted as proof of destruction (DP-02).
Restore and Archive can clear the teardown operation in the middle of a destroy (DP-30).

### 3.4 The reconciler

`reconcileAgentDBsOnce` (`cmd/pg_sage_sidecar/agentdb_reconciler.go:42-68`) runs inside one
60-second context:

1. **`ReconcileAbandonedDeployments`** (`teardown_reconcile.go:39-62`).
   - Archives expired `active` and `budget_exceeded` rows.
   - Then re-claims archived live rows in `available` or `status_checked` that have no teardown
     op, at most 100 per pass, with blocked rows rotated to the back
     (`teardown_reconcile.go:10-33`; the G8-B02 fix).
   - Each block reason is persisted (`teardown_reconcile.go:181-198`).
2. **`ReconcileLiveProvisioning`** (`lifecycle.go:23-70`).
   - Selects **every** row in `provisioning`, `create_uncertain`, `destroy_pending`,
     `destroying` or `status_unknown`, with no `LIMIT` or `ORDER BY` (`lifecycle.go:46-52`).
   - Calls the provider sequentially, and returns on the first database error
     (`lifecycle.go:64-67`).
   - Its blocks go only into the in-memory result (`lifecycle.go:203-208`). The caller discards
     that result (`agentdb_reconciler.go:60-62`). So uncertain creates that never resolve leave
     **no log line, no row field and no metric**.
3. **Fleet sync** (`agentdb_fleet.go:89-117`): adds and removes agent DBs from the monitoring
   fleet.

The operator endpoint `POST /agent-dbs/reconcile` runs only step 1
(`api/agent_db_execution_handlers.go:207-223`). `POST /agent-dbs/cleanup` only archives
(`api/agent_db_handlers.go:297-306`).

No inventory sweep exists. pg_sage never lists provider resources by `app=pg-sage` tag or label
to find resources it has forgotten. The runbooks rely on manual CLI sweeps, and the Lakebase
sweep queries the wrong API (section 8).

### 3.5 Orphaned-resource register

| # | Path | What keeps billing | Detected by pg_sage? |
|---|---|---|---|
| O1 | TTL expiry with the default backup gate (DP-01) | Every live instance | Only `teardown_blocked_reason` in the API JSON. Not shown in the UI (no match in `web/src`), no metric. |
| O2 | NotFound through a changed region, account, project or workspace (DP-02) | The real instance | No; the row says `destroyed` |
| O3 | Hosted or Lakebase uncertain create (DP-07) | A branch or project with no recorded id | No |
| O4 | Reconcile marks a create failed while it is in flight, then the create times out (DP-08) | The instance | No; the row says `failed` |
| O5 | RDS destroy without `disposable` | A final snapshot, forever (`aws_rds_runner.go:267-270`; no `DeleteDBSnapshot` anywhere) | No (G8-B22 sweep DEFERRED, `fixes-agentdb.md:39`) |
| O6 | `local_postgres` delete | The schema or database in the control DB | No; never dropped |
| O7 | Cross-install adoption (DP-09) | Not an orphan: install B destroys install A's instance | No |

---

## 4. Promotion: deploy requests

- **What it is.** A text record with `migration_sql`, `verification_sql`, `rollback_sql`,
  `forward_fix_notes`, `risk_tier` and `gate_results`.
  - States: `draft`, `review_requested`, then `approved` or `denied` (`deploy_requests.go:11-185`).
  - Routes are operator-only (`api/agent_db_handlers.go:195-214`;
    `api/agent_db_deploy_request_handlers.go:35-110`).
- **What is promoted: nothing.**
  - `gate_results.review_only=true` is hard-coded (`deploy_requests.go:221`).
  - No code reads `MigrationSQL` except storage; a `git grep` outside `deploy_requests.go` finds
    only struct fields and the DDL.
  - No schema, data or config moves, and there is no target database connection.
  - "Branch promotion" was explicitly deferred
    (`docs/reports/2026-05-09-claude-opus-agentdb-live-provisioning-review.md:49`).
- **Review quality.**
  - Any operator can approve, including the request's author. Only the author's identity is
    taken from the session (`api/agent_db_deploy_request_handlers.go:89-103`;
    `deploy_requests.go:131-153`).
  - `gate_results` from the caller are copied in as-is, so a body can claim
    `"rehearsal_passed": true` (`deploy_requests.go:216-221`).
  - The statement count is a naive split on `;` (`deploy_requests.go:230-238`). The `migration`
    package already has a sanitizing splitter.
- **Integrity bug (DP-21).** The insert is an upsert,
  `ON CONFLICT (deploy_request_id) DO UPDATE SET status=EXCLUDED.status, migration_sql=...`,
  with no guard (`deploy_requests.go:286-308`).
  - A retried create (the default id is deterministic, `deploy_requests.go:27-29`) silently
    resets an approved request to `draft`.
  - Reusing another deployment's id overwrites that request's SQL, while the audit row is
    written to the caller's deployment.
  - `reviewed_by` and `reviewed_at` survive the rewrite.
- **Not wired to any of the safety stack.** It goes through neither `policy.Gate` nor
  `Executor.Apply`. It does not use migration lint, clone rehearsal, verification or rollback
  (section 9).

---

## 5. Teardown guarantees

| Guarantee | Holds? | Evidence |
|---|---|---|
| Never deletes a resource pg_sage did not create | **Within one install, yes.** Across installs, no (DP-09). | Live destroy needs `live_mode`, a recorded id and a live creation receipt (`teardown_ownership.go:9-28`). RDS and Cloud SQL then verify the tag or label (`aws_rds_ownership.go:74-76`; `gcp_cloudsql_ownership.go:61-64`). Hosted providers verify name, scope and not-default (`hosted_runner.go:127-155`). Lakebase has the receipt only. |
| Expired leases are torn down | **Not with default config** (DP-01) | `config/config.go:1038`; `register.go:116`; `lifecycle_claim.go:93-95` |
| Teardown is durable across crashes | Yes | Section 3.3 |
| Teardown is not blocked by unrelated state | No | Any fleet instance's emergency stop blocks every AgentDB teardown (`cmd/pg_sage_sidecar/agentdb_fleet.go:158-170`). The kill switch blocks authorized destroys (DP-05). |
| "Destroyed" means gone | Mostly | A scope change can fake NotFound (DP-02). RDS final snapshots remain (O5). Cloud SQL loses its backups on delete (no final backup). |
| Operators are told about stuck teardowns | No | Not in the UI, no alerting or metric hook (`git grep teardown_blocked` outside `internal/agentdb` finds nothing). The reconciler logs only counts. |
| Local objects are dropped | No | O6 |

---

## 6. Were the live paths ever run for real?

**Yes, in May 2026, against an older shape of the code.**

- **2026-05-09.** Runner-level create, status and delete passed for RDS (us-east-2,
  `db.t4g.micro`, 20 GiB) and for Cloud SQL (`db-f1-micro`, **public IPv4 enabled** because
  that was the only shape that worked) (`docs/reports/2026-05-09-agentdb-release-readiness-report.md:27-53`).
  - Lakebase did not run at first because CLI auth was invalid (`:55-66`). It passed later
    (`:224-228,264-270`).
- **2026-05-10.** The product-chain "gauntlet" (blueprint to RDS, template to Cloud SQL, agent
  request to Lakebase) passed, and cleanup sweeps found no leftovers (`:324-384`).
- **Failures recorded.**
  - The Cloud SQL test panicked at Go's 10-minute default test timeout
    (`docs/reports/2026-05-10-agentdb-release-hardening-report.md:107-110`).
  - Deletes left rows at `destroying`; this was fixed by mapping NotFound to `destroyed`
    (`:135-138`).
  - The AWS sweep could not run because the session had expired (`:120-121`).
- **Evidence is not in the repo.** Receipts go to `sidecar/test-output/agentdb-live-receipts.jsonl`
  (`live_receipts_test.go:40-62`), which is gitignored (`.gitignore:54-55`).
- **Nothing has run live since the 2026-09-26/27 rewrites.** Those rewrites added
  `create_uncertain`, receipt and tag ownership, deletion-protection lifting, region binding,
  the restore-drill gate and D4 approvals. The fixes record says: "No real cloud APIs were
  called; the six live cloud tests remain skipped" (`reviews/2026-09-26/fixes-agentdb.md:7`).
- **The gauntlet is stale and would fail (DP-28).**
  - It expects `SafeForDestroy` after a live backup check (`live_product_chain_test.go:332-335`),
    but since G8-B11 only an admin drill grants `restore_verified` (`backup_assurance.go:97-106`;
    `restore_drill.go:24-58`).
  - Its Lakebase leg passes `provider_params` through the request path
    (`live_product_chain_test.go:258-271`), which is discarded since 2026-07-19 (DP-06).
- **Neon and Supabase,** added 2026-09-07, have no recorded live run.
- **The root Playwright "live provisioning" spec only navigates tabs**
  (`e2e/agentdb-live-provisioning.spec.ts:7-22`).

---

## 7. Bugs and gaps

Severity scale:

- **P0:** silent cost leak or wrong-resource destroy under default settings.
- **P1:** a broken core flow, or a security or policy bypass.
- **P2:** a correctness problem with a workaround.
- **P3:** hygiene.

### DP-01 — The default configuration blocks TTL teardown of every live resource (P0, CONFIRMED)

- **Code.**
  - `require_backup_before_destroy` defaults to true (`config/config.go:1035-1041`).
  - `normalizeRegister` ORs it into every row (`register.go:116`), so `backup_required` is
    always on.
  - The teardown claim requires `restore_verified` (`lifecycle_claim.go:41-45,93-95`;
    `teardown_mutation.go:40-44`).
  - Only `RecordRestoreDrill` sets that status. It is an admin attestation per deployment, with
    the runner recorded as `operator_attestation` (`restore_drill.go:10-58`;
    `api/agent_db_handlers.go:241-245`).
  - A live backup check records only `verified` or `unverified` (`backup_assurance.go:97-106`).
- **Scenario.** An agent sandbox on RDS reaches its TTL. The reconciler archives it and calls
  `blockTeardown("verified restore required")` (`teardown_reconcile.go:119-122,160-176`) on
  every pass. With 100 sandboxes, an admin must attest 100 restore drills, or they bill
  indefinitely.
- **Signal.** The block reason is visible only in the API JSON (`deployment_types.go:41-44`).
  It is not in the UI, and there is no metric or alert.
- **History.** This is the remainder of G8-B02 scenario 1: rows are now revisited, but the
  gate itself is unchanged.
- **Fix.** Make backup-before-destroy a per-class policy that is off for disposable or sandbox
  classes. Raise an alert when a live resource has been blocked for longer than N minutes.

### DP-02 — A provider "not found" seen through the wrong scope is recorded as destroyed (P1, CONFIRMED path; trigger PLAUSIBLE)

- **Code.**
  - Destroy treats NotFound as `destroyed` for RDS (`aws_rds_ownership.go:66-71,79-82`), Cloud
    SQL (`gcp_cloudsql_ownership.go:57-60,83-89`), Lakebase (`lakebase_runner.go:114-118`) and
    the hosted providers (`hosted_runner.go:145-148`).
  - The scope comes from the current process: the AWS region and credentials
    (`runtime_registry.go:28-36`), the GCP project (`gcp_cloudsql_runner.go:178-184`), the
    Databricks host, and the hosted API key's organization.
  - The creation receipt never records region, account or project (`live_create.go:181-185`;
    `lifecycle.go:127-131`), although the table has those columns (`execution.go:571-576`).
- **Scenario.**
  - The RDS runner supports a single region per process (`aws_rds_ownership.go:95-110`).
  - An operator restarts with `PG_SAGE_AWS_REGION=us-east-1` to serve a new region.
  - At TTL, every existing us-east-2 deployment calls `DescribeDBInstances` in us-east-1, gets
    `DBInstanceNotFound`, and is marked `destroyed`.
  - The us-east-2 instances bill forever and `DELETE` tombstones their rows.
  - The same happens after rotating to credentials for a different account, project or
    workspace.
- **Fix.** Record region, account, project and workspace on the receipt. Refuse to destroy, or
  to accept NotFound, when the runner's scope differs. Require two NotFound readings, separated
  in time, before `destroyed`.

### DP-03 — The authorization policy hash embeds the process start time (P2; P1 for HA) (CONFIRMED)

- **Code.**
  - The global layer's `Version` is `a.loadedAt.UnixNano()`, set once per process
    (`api/agent_db_live_authority.go:33-35,180-183`).
  - The policy hash includes `GlobalVersion` (`effective_policy.go:349-369`).
  - Execution requires `authz.PolicyHash == CurrentPolicyHash` (`live_execution_contract.go:375-377`).
- **Scenario.** With two replicas behind a load balancer, authorizing on A and executing on B
  always returns "invalid request". Any restart between authorize and execute does the same.
- **Fix.** Version the global layer by a hash of its content, not by the load time.

### DP-04 — Live provisioning stops 30 days after the last provider-config save (P2, CONFIRMED)

- **Code.**
  - The provider layer uses `ValidatedAt = persisted.UpdatedAt` with
    `ValidationTTL = 30 * 24h` (`api/agent_db_live_authority.go:16,184-187`).
  - Staleness becomes the deny reason "provider policy validation is stale"
    (`effective_policy.go:120-123`).
  - The `last_validated_at` column is never written; `git grep` shows only selects
    (`provider_config.go:35,49,64`).
- **Scenario.** Every create and every authorized destroy fails until someone re-saves provider
  settings. TTL teardown is unaffected because it does not consult policy.
- **Fix.** Add a real validation probe that writes `last_validated_at`, or drop the TTL.

### DP-05 — Destroy authorizations are judged by create-time rules (P1, CONFIRMED)

- **Code.**
  - `IssueLiveExecutionRecords` builds a plan for a destroy the same way as for a create. It
    requires TTL > 0 (`live_execution_contract.go:73-78`), where the TTL is the remaining lease
    from `live_execution_issue.go:102-107`.
  - It requires a cost estimate above zero (`live_execution_contract.go:264-266`). The estimate
    is rounded to cents (`provider_cost.go:121`).
  - It applies the same policy, with no special case for the operation
    (`provider_policy.go:45-94`; `effective_policy.go:60-64`).
- **Scenarios.**
  - An operator cannot use `authorize-live` to destroy:
    - an expired deployment (TTL ≤ 0);
    - an RDS micro instance with 20 GiB and less than about 15 minutes left (14.3 USD/month,
      so under ~906 s the estimate rounds to
      $0.00);
    - any deployment while `live_provisioning_enabled=false`, the provider is disabled, the
      mode is `manual`, public IP is now disallowed, or the region was removed from the
      allowlist.
  - In other words, the kill switch blocks cleanup. The only remaining route is to wait for
    TTL, or to extend the lease to 1 second and wait.
- **Fix.** Give destroy its own policy: always allowed for owned resources, and gated only by
  the emergency stop and ownership.

### DP-06 — The D4 request path cannot go live with seeded profiles; `provider_params` are silently dropped (P1, CONFIRMED)

- **Code.**
  - The API parses `provider_params` (`api/agent_db_request_handlers.go:139-157`), and
    `registerFromRequest` stores them (`request_provision.go:152-156`).
  - `Provision` then overwrites them with the size profile's params (`schema.go:121-123`; since
    `492cd7b3`, 2026-07-19). This is deliberate, as `store_test.go:192-196` asserts.
  - The seeded cloud profiles carry no region, project or organization (`profiles.go:180-201`).
  - The live plan requires a region (`live_execution_issue.go:93`;
    `live_execution_contract.go:73-78`).
- **Scenario.** The documented flow (request, approve, provision) produces a deployment whose
  `authorize-live` call always fails as an invalid request. It works only after an operator
  creates a custom size profile per region and project.
- **Docs.** `docs/agent-db-deployments.md:139-145` does not mention this.
- **Fix.** Either bind region, project and network on the approved request, or reject unknown
  `provider_params` with a 400 instead of dropping them.

### DP-07 — Hosted creates that error, and every uncertain Lakebase create, wedge forever and can leave billed orphans (P1, CONFIRMED)

- **Code.**
  - `HostedRunner.Create` returns `status_unknown` for every error (`hosted_runner.go:92-96`).
    That includes local errors raised before any request, such as a missing Supabase region or
    master password (`hosted_api.go:71-80`), and definitive 400, 402 and 403 responses
    (`hosted_http.go:103-121`).
  - `liveCreateOutcome` turns that into `create_uncertain` (`live_create.go:152-163`).
  - The hosted `Status` needs a recorded id (`hosted_runner.go:119-121`), so the reconciler
    blocks it in memory (`lifecycle.go:147-161,203-208`).
  - New creates refuse `create_uncertain` (`live_create.go:85-89`).
  - Teardown refuses it too, because the row owns no recorded resource
    (`teardown_ownership.go:10-12`).
  - The row can never be deleted, because it never reaches `destroyed` (`policy.go:79-86`).
  - No API or UI path resolves `create_uncertain`; there are no matches in `internal/api` or
    `web/src`.
- **Scenario.** A browser tab closes during a Neon branch create. The request context is
  cancelled (`api/agent_db_live_authority.go:265-270`), but Neon has already created the
  branch. pg_sage never records it and never deletes it, and nothing logs it (section 3.4).
- **History.** G8-B06 accepted "Hosted/Lakebase stay blocked for manual reconciliation"
  (`fixes-agentdb.md:19`). No manual tool exists. Neon and Supabase branch names include a
  hash of the deployment id (`provider_naming.go:13-21`), so adoption by name is feasible.
- **Fix.** Classify definitive errors as `failed`. Adopt by deterministic name plus scope.
  Persist block reasons, and expose an admin "resolve uncertain create" action.

### DP-08 — The status reconciler ignores the mutation lease; status writes are not compare-and-swap (P2, code CONFIRMED / race PLAUSIBLE)

- **Code.**
  - `reconcileLiveStatus` and `CheckProvisionStatusLive` never take the lease
    (`lifecycle.go:103-145`; `execution.go:257-310`).
  - `updateProvisioningStatus` and `applyProvisionResult` read the status, validate the
    transition, and then `UPDATE ... WHERE deployment_id=$1` with no check that the status is
    unchanged (`execution.go:447-516`).
- **Scenario A.** The reconciler GETs the derived name while a create is in flight and gets
  NotFound. It marks the row `failed` and clears `create_operation_id` and `live_mode`
  (`lifecycle.go:141-143`; `live_create.go:201-208`). The create then times out, giving
  `create_uncertain`. But `failed → create_uncertain` is not a valid transition
  (`provider_runner.go:181`), so `recordLiveCreateOutcome` errors and the row stays `failed`.
  If the instance exists, it is an orphan.
- **Scenario B.** A reconciler status write (`provisioning → available`) lands between a
  destroy's `destroying` update and its outcome write. The destroy outcome is then an invalid
  transition, the row reads `available` while the instance is deleting, and the teardown
  cannot resume.
- **Fix.** Make every status write compare-and-swap on `lifecycle_version`, and have status
  reconcile take or respect the lease.

### DP-09 — Ownership tags carry no installation id; adoption can cross installs (P1, PLAUSIBLE)

- **Code.**
  - Tags and labels hold only the deployment id (`aws_rds_runner.go:143-147`;
    `gcp_cloudsql_runner.go:171-174`).
  - Adopting an uncertain create trusts that tag (`lifecycle.go:126-135`;
    `aws_rds_ownership.go:181-192`).
  - "Instance already exists" maps to a conflict, which becomes `create_uncertain`
    (`aws_rds_runner.go:192-193`; `aws_rds_ownership.go:146-153`).
  - Deployment ids default to a hash of the request id (`request_provision.go:53`). Request ids
    default to a hash of the request body (`store.go:38-41`).
  - The GCP label folds and truncates, adding more collisions (`gcp_cloudsql_runner.go:312-326`).
- **Scenario.** Staging and production sidecars share an AWS account, and the same agent sends
  the same request to both. Install B's create collides with install A's instance. B's
  reconciler adopts A's instance by tag, and B's TTL then deletes A's database. On GCP, B
  lifts deletion protection itself first (`gcp_cloudsql_ownership.go:65-71`).
- **Fix.** Add an `installation_id` tag and label. Use collision-resistant names everywhere (the
  hosted scheme). Never adopt on a conflict.

### DP-10 — Account and project allowlists check caller-shaped params, not the runner's real scope (P2, CONFIRMED)

- **Code.**
  - The plan takes `Account` and `Project` from `provider_params` (`live_execution_issue.go:93-95`).
  - The policy check is skipped when they are empty (`provider_policy.go:68-73`).
  - The AWS runner never reads an account and has no STS check (`git grep -i 'sts|account'`
    finds nothing in the AWS files).
  - Cloud SQL falls back to the env project (`gcp_cloudsql_runner.go:178-184`).
- **Scenario.** `allowed_projects: [sandbox]`, params with no project, and
  `PG_SAGE_GCP_PROJECT=prod` together produce an instance in prod. On AWS, `allowed_accounts`
  is decoration. This is the same class as G8-B07, which was fixed only for the AWS region.
- **Fix.** Derive account and project from the runner's credentials (STS, metadata, config)
  and bind them into the plan hash.

### DP-11 — Live approval is single-person (P1 for enterprise, CONFIRMED)

- **Code.**
  - `ReviewerID: requesterID` (`api/agent_db_live_authority.go:124-129`).
  - The executor must be the same user (`api/agent_db_live_authority.go:326-328`).
  - D4 request approval and deploy-request approval also allow self-approval
    (`request_decision.go:26-68`; `deploy_requests.go:131-153`).
- **History.** G8-B08 "two-person review DEFERRED" (`fixes-agentdb.md:21`).

### DP-12 — `auto_within_policy` is unreachable; no autonomous provisioning path exists (P2, CONFIRMED)

- **Code.**
  - Auto mode requires an estimate with confidence `"high"` (`live_execution_contract.go:283-290`).
  - No estimator ever returns `"high"`; they return only `medium` or `low`
    (`provider_cost.go:62-125`; `cost_guard.go:60-66`).
  - `AutoIssued` is never true in production code (`git grep AutoIssued`).
- **Effect.** The documented mode (`docs/agent-db-deployments.md:253-256`) is dead, and nothing
  connects to the trust ladder or earned autonomy.

### DP-13 — The cost model under-prices and does not bound total spend (P1, CONFIRMED)

- **Code.**
  - Prices are static per class (`provider_cost.go:62-102`).
  - The model ignores `multi_az` and REGIONAL availability. Since G8-B20 the runners do honor
    them (`aws_rds_runner.go:131`; `gcp_cloudsql_runner.go:170`), so the cost of an HA instance
    is understated by about half.
  - It also ignores edition, I/O, backup storage and RDS final snapshots.
  - `leaseExtensionAllowed` prices only the new window against the whole budget
    (`cost_guard.go:36-56`), so repeated extensions spend without limit.
  - `budget_exceeded` takes no provider action (G8-B29 DEFERRED, `fixes-agentdb.md:45`).
  - Extensions are capped only by the YAML provider TTL, not by the persisted provider policy
    (`api/agent_db_request_handlers.go:40-45`).
- **Fix.** Track cumulative spend: elapsed lifetime plus the requested window, compared with
  the budget. Add the HA multiplier. Take budgets from provider billing APIs where they exist.

### DP-14 — TTL starts at registration; short TTLs cannot be estimated (P2, CONFIRMED)

- **Code.**
  - The lease is set at register (`queries.go:51`; default 3600 s at `register.go:104-106`) and
    never reset when the resource becomes available. The GA review claimed the opposite
    (section 8).
  - The plan TTL is the remaining lease (`live_execution_issue.go:102-107`). Rounding cost to
    cents (`provider_cost.go:121`) yields $0.00 for TTLs under about 15 minutes on RDS micro with 20 GiB,
    and a $0.00 estimate is invalid (`live_execution_contract.go:264-266`).
- **Scenario.** An approval that sits for an hour lets the lease expire. The deployment is
  archived before it is ever created, and the authorization cannot be issued.

### DP-15 — RDS network and identity posture is not enterprise-safe (P1, CONFIRMED; DEFERRED in G8-B20)

- **Code.**
  - No subnet group, security group, IAM database auth, `CopyTagsToSnapshot` or KMS key choice
    (section 2.2).
  - `private_network` is not applied (`aws_rds_ownership.go:120-122`).
  - Agents get only the master user's secret (section 2.7).

### DP-16 — Cloud SQL instances are unusable and possibly not creatable private-only (P1)

- **No credential is created** (CONFIRMED, section 2.3).
- **Private-only cannot be created** (PLAUSIBLE: GCP rejects an instance that has no public IP,
  no private network and no PSC). `ipv4_enabled=false` is the safe default
  (`gcp_cloudsql_runner.go:135`), and `privateNetwork` is never set. The live runs enabled
  public IPv4 (`docs/reports/2026-05-09-agentdb-release-readiness-report.md:50`), and the seeded
  profile sets `ipv4_enabled: true` (`profiles.go:190-196`).
- **Destroy fails under the documented minimum IAM** (doc gap CONFIRMED / runtime PLAUSIBLE).
  Destroy patches deletion protection (`gcp_cloudsql_ownership.go:65-71`), but both runbooks
  omit `cloudsql.instances.update` (`docs/runbooks/agentdb-cloud-provider-setup.md:145-151`;
  `docs/runbooks/agentdb-live-provisioning.md:96-101`). With that IAM, every non-disposable
  destroy returns 403 and retries forever.

### DP-17 — RDS final snapshots leak (P2, CONFIRMED)

- **Code.** Non-disposable is the default because nothing sets `disposable`; it is only read at
  `aws_rds_runner.go:161-167`. Without `CopyTagsToSnapshot` the snapshots are untagged, and no
  sweep exists (O5).

### DP-18 — pg_sage cannot monitor the databases it provisions (P1 for the AI-DBA promise, CONFIRMED)

- **Code.**
  - `applyProvisionResult` overwrites `secret_ref` with the provider's reference
    (`execution.go:500`). For RDS that is the Secrets Manager ARN (`aws_rds_ownership.go:26-31`).
  - The fleet resolver accepts only `env:PG_SAGE_AGENTDB_*` (`agentdb_secret_ref.go:25`;
    `credentials.go:22`). The RDS secret ARN is never resolved (DEFERRED,
    `fixes-agentdb.md:37`).
  - Cloud SQL rows have no `host` or `endpoint`, so they are never eligible
    (`agentdb_secret_ref.go:37-43`).
  - The inline path needs `host`, which no runner emits (`agentdb_fleet.go:30-37`).
  - In the `env:` path, `sslmode` defaults to `prefer` for non-hosted providers
    (`agentdb_secret_ref.go:53-55`). That contradicts the "never plaintext" rule of the inline
    path (`agentdb_fleet.go:57-61`).
  - The deployment's `safety_mode` is not passed to the fleet runtime
    (`agentdb_fleet.go:62-71`; `agentdb_secret_ref.go:56-59`). Agent DBs therefore inherit the
    fleet's default trust and execution mode (`cmd/pg_sage_sidecar/mode_init.go:189-200`).

### DP-19 — Lakebase gaps (P2)

- **Ownership metadata is never sent** (CONFIRMED; `lakebase_runner.go:179-182` vs `:220-225`).
- **Errors are untyped**, so a project-level 404 becomes `destroyed` (CONFIRMED;
  `lakebase_runner.go:286-290,114-118`).
- **The branch TTL is not extended with the lease** (CONFIRMED; `store.go:165-201` makes no
  provider call). PLAUSIBLE impact: Databricks deletes the branch while pg_sage shows it as
  `available`.
- **The create-response parse may yield no id** (PLAUSIBLE; section 2.4).
- **A one-second TTL clamp on an expired lease** (CONFIRMED; `lakebase_runner.go:162-168`).

### DP-20 — `local_postgres` puts agent schemas inside pg_sage's control database with no isolation (P1, CONFIRMED; partly DEFERRED in G8-B17)

- **Code.** Section 2.6.
- **Blast radius.** Any credential an operator hands to an agent for its schema can reach
  `sage.*`: approvals, receipts, trust ledger and sessions.

### DP-21 — Deploy-request upsert integrity (P2, CONFIRMED)

- **Code.** Section 4 (`deploy_requests.go:27-29,216-221,286-308`).

### DP-22 — Template approval is not tied to the reviewed content (P2, CONFIRMED)

- **Code.**
  - `CreateTerraformTemplate` upserts content and status (`terraform_templates.go:19-46`).
  - `ApproveTerraformTemplate` re-reads only the findings and approves by id
    (`terraform_templates.go:75-103`), without an expected `content_sha256`.
  - So content can change between review and approval.
- **Impact.** Low, because templates are never applied (section 2.1). Fix by deleting them.

### DP-23 — Runner registration fails silently (P3, CONFIRMED)

- **Code.** `if err == nil { registry.Register(runner) }` with no log on error
  (`runtime_registry.go:36-39`). Missing runner environment variables also return silently
  (`runtime_registry.go:25-35,52-54,71-73`).
- **Effect.** An operator learns of it only from readiness or from `"live runner unavailable"`
  teardown blocks (`teardown_reconcile.go:106-109`).

### DP-24 — `beginLiveCreate` is two statements and not atomic (P3, CONFIRMED)

- **Code.** `live_create.go:100-109`; consequence in section 3.2.

### DP-25 — The live reconcile pass is unbounded and unordered, and aborts on one error (P2, CONFIRMED code / impact PLAUSIBLE)

- **Code.** `lifecycle.go:46-52,64-67`. The 60-second budget is shared with the archive sweep
  (`agentdb_reconciler.go:47-62`).
- **Effect.** With many in-flight rows, the rows late in the scan starve.

### DP-26 — The provider is never consulted before an authorization is spent (P2, CONFIRMED)

- **Code.**
  - `ProviderRunner.Preflight` has no production caller.
  - `PreflightProvision` checks plan shape only (`execution.go:28-60`).
  - Bad credentials, quota or subnet problems surface only after the authorization is consumed.

### DP-27 — No inventory sweep, and the Lakebase sweep command checks the wrong API (P2, CONFIRMED)

- **Code.** Section 3.4. The runbook command `databricks api get /api/2.0/database/instances`
  (`docs/runbooks/agentdb-cloud-provider-setup.md:253`; `agentdb-live-provisioning.md:183`)
  queries a different API from the runner's `/api/2.0/postgres/projects/{p}/branches`
  (`lakebase_runner.go:226-228`).

### DP-28 — The live gauntlet is stale; the current code is unproven against real providers (P1 evidence gap, CONFIRMED)

- **Code.** Section 6 (`live_product_chain_test.go:258-271,332-335`).

### DP-29 — Promised gates were never built (P1, CONFIRMED)

- **Spec.** The GA spec's config block lists `max_live_creates_per_hour/day`,
  `rate_limit_dimensions`, `default_ttl_seconds`, `require_private_network_for_production` and
  `provider_execution` (`docs/superpowers/specs/2026-05-09-agentdb-live-provisioning-ga.md:387-418`).
  It also requires "TTL countdown starts when the resource first reaches `available`" and
  "live create approvals reuse the existing Agent DB deploy-request review workflow"
  (`:430-441`).
- **Code.** None of these exist in `AgentDBConfig` (`config/config.go:144-160`) or anywhere else.

### DP-30 — Lifecycle `status` and `provisioning_status` are separate state machines with an unguarded setter (P3, CONFIRMED code / PLAUSIBLE harm)

- **Code.** `Restore` and `Archive` call `setStatusFields` (`store.go:203-209,280-310`). With
  status `active`, it clears `teardown_operation_id`, the provider mutation lease and the claims,
  whatever the `provisioning_status` (`store.go:291-295`).
- **Effect.** An operator Restore during `destroying` shows `active` while the instance is being
  deleted. `extendLeaseSQL` guards destroy states (`register.go:136-140`); this path does not.
  It is the same class as G8-B01, which was fixed only for pings.

### DP-31 — Timeouts on provider calls (P3)

- **Code.**
  - Cloud SQL and Lakebase use `http.DefaultClient`, with no timeout
    (`gcp_cloudsql_runner.go:335-338`; `lakebase_runner.go:258-261`).
  - API-driven creates and destroys inherit the HTTP request context
    (`api/agent_db_live_authority.go:265-270,295-300`).
- **Effect.** A hung call holds the 15-minute mutation lease. CONFIRMED code; impact PLAUSIBLE.

### DP-32 — Committed QA credentials (P3, CONFIRMED)

- **Where.** A local QA admin password is committed at
  `docs/reports/2026-05-09-agentdb-release-readiness-report.md:8` and
  `docs/reports/2026-05-10-agentdb-release-hardening-report.md:154`.
- **Also.** A fixture password appears at `docs/agent-db-deployments.md:350,358,378`. The values
  are not reproduced here. They are local test logins, but the readiness report itself says
  "do not commit credentials" (`:390`).

---

## 8. Docs versus code

| # | Claim | Doc | Code | Verdict |
|---|---|---|---|---|
| 1 | The AWS runner needs only `PG_SAGE_LIVE_PROVISIONING=1` | `docs/runbooks/agentdb-cloud-provider-setup.md:58-62`; `agentdb-live-provisioning.md:44-47` | Also needs `PG_SAGE_ENABLE_AWS_RDS_RUNNER=1` and a region (`runtime_registry.go:24-35`) | **False**: following the doc never registers the runner |
| 2 | Provider settings hold allowed instance classes or tiers and a default backup retention | `cloud-provider-setup.md:76-77`; `live-provisioning.md:39-40` | No such policy fields (`provider_policy.go:5-22`; `api/agent_db_execution_handlers.go:82-106`) | **False**: unenforced |
| 3 | Every live request carries `app=pg-sage` and `pg_sage_deployment_id` tags or metadata | `live-provisioning.md:119` | Lakebase never sends them; hosted providers have none | **Partly false** |
| 4 | Cloud SQL needs a static access token | `cloud-provider-setup.md:130-143` | A metadata source exists (`credentials.go:131-143`) | Stale |
| 5 | Minimum Cloud SQL IAM | `cloud-provider-setup.md:145-151`; `live-provisioning.md:96-101` | Destroy needs `instances.update` (DP-16) | Incomplete |
| 6 | Secrets Manager "only when using managed master password flows" | `cloud-provider-setup.md:113`; `live-provisioning.md:94` | Always on (`aws_rds_runner.go:226`) | Misleading |
| 7 | Lakebase cleanup sweep command | `cloud-provider-setup.md:253`; `live-provisioning.md:183` | Wrong API (DP-27) | **Wrong** |
| 8 | Emergency stop is `live_provisioning_enabled=false` plus disabling providers | `live-provisioning.md:192-197` | TTL destroys check only `emergency_stop` and the fleet stop (`teardown_reconcile.go:111-114`; `store_options.go:50-72`) | **Incomplete**: the doc'd stop does not stop the reconciler deleting databases |
| 9 | Stuck create: "reconcile should adopt"; "mark the Agent DB failed and retry" | `live-provisioning.md:199-205` | Adoption is RDS and Cloud SQL only. No API marks a row failed. | Partly false |
| 10 | Provisioning model lists four providers | `docs/agent-db-deployments.md:17-22` | Six (`types.go:32-37`) | Stale |
| 11 | Request example sends `allowed_regions` | `agent-db-deployments.md:132` | Ignored; server config wins (`api/agent_db_request_handlers.go:34`) | Stale (the doc says so at `:97-98`) |
| 12 | Provision body fields | `agent-db-deployments.md:143-145` | `provider_params` is accepted and then dropped (DP-06) | Incomplete |
| 13 | `auto_within_policy` | `agent-db-deployments.md:253-256` | Unreachable (DP-12) | **Misleading** |
| 14 | Fleet skips `secret_ref` deployments and attaches no analyzer or executor | `agent-db-deployments.md:314-319` | `env:` references are resolved (`agentdb_fleet.go:23-26`); the full runtime is built (`agentdb_fleet.go:172-206`) | Stale |
| 15 | Receipts as release evidence | `cloud-provider-setup.md:256-259` | Gitignored (`.gitignore:54-55`) | Evidence not kept |
| 16 | GA review "integrated": TTL starts at available, rate limits, dual-control emergency destroy, Auth Proxy required for public Cloud SQL, live approvals reuse deploy requests | `docs/reports/2026-05-09-claude-opus-agentdb-live-provisioning-review.md:27-34` | None exist (DP-14, DP-29) | **Not built** |
| 17 | "Live provisioning" Playwright spec | `e2e/agentdb-live-provisioning.spec.ts` | Navigation only (`:7-22`) | Misnamed |

---

## 9. Deployment-adjacent core packages: `clone`, `rollout`, `migration`

From the sub-audit. Re-read and confirmed by me:

- **B1:** `migration/runtime/postgres_rehearsal.go:27-63` never sets `AffectedQueries`, and
  `migration/rehearsal/orchestrator.go:117-131` therefore returns `recommend_only`.
- **B2:** `schema/ddl_agent_features.go:45` lacks `blocked`, which
  `mcp/production_intent_executor.go:222-226` writes.
- **R1:** `rollout/canary.go:355-372` only ever reports a regression of 0 or 100.
- **B7:** `migration/runtime/postgres.go:30-48` calls `executor.ExecConcurrently` and
  `ExecInTransaction` directly.

### 9.1 `internal/clone` (273 lines; 2 commits, 2026-07-23)

- **What it is.** An interface, `Provider{Create, Destroy, SnapshotAge}` (`clone/provider.go:8-23`),
  with two adapters.
  - **DLE:** an HTTP client with a 30-second timeout and a bearer token
    (`clone/dle_provider.go:29-152`). Its contract may be custom rather than the upstream
    Postgres.ai API (UNVERIFIED).
  - **Snapshot:** an abstract `SnapshotAPI` meant for RDS, Aurora, Cloud SQL or AlloyDB
    adapters (`clone/snapshot_provider.go:15-63`). **No adapter exists.** The config value
    `snapshot` always errors (`cmd/pg_sage_sidecar/mcp_migration_runtime.go:23-27`).
- **Wiring.** `clone.provider` (default `none`) feeds MCP `apply_migration`, game days and the
  bench (`mcp_migration_runtime.go:29-66`; `autonomy_runtime_loops.go:150-169`).
- **Safety.** No `policy.Gate`. No check that a clone DSN is not production (the sub-audit's B5).
  A leaked clone cannot be found, because `Provider` has no `List` and the clone id is never
  persisted (B10).
- **AgentDB:** no references in either direction.

### 9.2 `internal/rollout` (1,172 lines; last commit 2026-10-01)

- **What it does.** `Engine.Rollout` re-verifies, applies and measures per instance, with a
  canary set, a halt that rolls back newest-first, and a blast-radius stop
  (`rollout/engine.go:34-139`). The fleet canary runs as a background job, one at a time per
  process (`rollout/canary.go:123-140`).
- **Wiring.** Admin `POST .../rollouts` (`api/autonomy_handlers.go:66-68`). Execution goes
  through `Executor.ExecuteManual`, so the policy gate and `Apply` are used
  (`cmd/pg_sage_sidecar/autonomy_fleet.go:104-110`; `executor/manual.go:55,68-70`).
  Scheduled rollouts with a `PolicySet` are unwired.
- **Bugs.**
  - R1: the regression figure is 0 or 100, and the 60-second settle time is shorter than the
    120-minute verify window, so the canary rarely halts.
  - R3: errors return without rolling back.
  - R4: progress is persisted only at the end, so a crash leaves the run in "canary" forever.
- **AgentDB:** none. It could drive schema changes across a fleet of agent databases after
  R1, R3 and R4 are fixed.

### 9.3 `internal/migration` (3,421 lines including `plan`, `rehearsal`, `runtime`)

- **Advisor.** Regex DDL classification with lock levels and catalog-stat risk scoring. It
  writes `sage.findings` (`migration/advisor.go:80-107`).
  - Off by default (`config/config.go:434-443`).
  - Also exposed as the MCP `lint_migration` tool (`agenttools/lint.go:47-82`).
  - It executes nothing.
- **Online path** (`plan`, `rehearsal`, `runtime`).
  - The planner handles exactly two shapes: `ADD CONSTRAINT UNIQUE` and `SET NOT NULL`
    (`migration/plan/planner.go:87-150`).
  - Rehearsal clones, runs the expand steps, and destroys the clone
    (`migration/rehearsal/orchestrator.go:79-115`).
  - Runtime calls `gate.Authorize` per step, then an applier that **bypasses `Executor.Apply`**
    (B7, `migration/runtime/postgres.go:30-48`).
  - **Rehearsal can never promote** (B1). A policy block also crashes on a CHECK constraint (B2).
- **AgentDB:** none. Deploy requests ignore all of it.

### 9.4 How they should relate to AgentDB

1. **Promotion means "migration PR".**
   - A deploy request's SQL should run through `lint_migration`.
   - Then a rehearsal on a branch of the target: Neon or Lakebase branch, or local
     `CREATE DATABASE ... TEMPLATE`. The rehearsal needs a "verbatim SQL" mode, because the
     planner refuses `CREATE TABLE` and other common DDL.
   - Then verification SQL, then rollback SQL on the branch.
   - Then `policy.Gate` and `Executor.Apply` on the target, with a verify window.
2. **One disposable-database abstraction, not two.** Implement `clone.Provider` on AgentDB's
   branch runners. That fixes clone's missing adapters (B3, B10), gives AgentDB clones per
   source database, and puts leaked rehearsal clones under the AgentDB reconciler.
3. **Rollout for fleets of agent databases.** Use it for the same migration across many tenant
   databases, after R1, R3 and R4 are fixed.

---

## 10. pg_sage's own enterprise deployment (summary)

From the sub-audit. Re-read and confirmed by me:

- `Dockerfile:26-38`: Alpine with curl, runs as non-root `sage`, and the health check curls
  `/health`.
- `api/router.go:274-280`: `/health` is a static 200.
- `auth/oauth.go:28-36`: OAuth state is an in-memory map.
- `config/config.go:650-653`: `SAGE_TLS_CERT` and `SAGE_TLS_KEY` are read; `git grep` shows no
  other use.
- `auth/types.go:21-35`: three global roles.
- `store/config_helpers.go:309-333`: config overrides are stored verbatim.
- No leader election (`git grep -i 'leader|election'`).

| Area | State | Gap and severity |
|---|---|---|
| Packaging | One Alpine image running as non-root uid 1000, plus goreleaser binaries (`Dockerfile:26-38`; `.goreleaser.yml:9-69`). No Helm or Kubernetes manifests; only a `replicas: 1` example in `docs/deployment.md:155-217`. The image is unsigned, has no SBOM or scan, and is single-arch (`.github/workflows/ci.yml:550-611`). | P1: no chart, probes, securityContext or PDB. `/health` is static (`api/router.go:274-280`) and there is no readiness endpoint. |
| HA | No leader election. Each replica loads every database (`cmd/pg_sage_sidecar/metadb.go:329-343`). Per-object DDL leases, the budget lock, SRE fenced leases and AgentDB claims are safe across processes. OAuth state, rate limiters, alert throttle, LLM budget counters and config overrides are per process. | P1: one replica only. The AgentDB policy hash also breaks across replicas (DP-03). |
| AuthN | bcrypt (cost 12) local users. OIDC, Google and GitHub with no PKCE or nonce; `id_token` is ignored and identity comes from userinfo (`auth/oauth.go:162-172,322-331`). No group-to-role mapping, domain allowlist, SAML or SCIM. No MFA. No password-change endpoint. The first admin's password is printed to the logs. | P1 (P0 when Google or GitHub is the provider) |
| AuthZ | Three global roles: admin, operator, viewer (`auth/types.go:21-35`). No per-database scope for people. MCP tokens have database scope and are the strongest part. | P1 |
| Secrets | AES-256-GCM with an argon2id key (`crypto/crypto.go:21-133`), stored in the same database as its salt, with no key id or rotation. Config set through the API, including `llm.api_key` and webhooks, is stored in plaintext in `sage.config` (`store/config_helpers.go:309-333`). No Vault, cloud secret manager or `*_FILE` support. | P0 |
| Audit | `action_log`, `config_audit`, `auth_audit` (OIDC only) and AgentDB JSONL export. Logins and user or role changes are not audited. No SIEM, syslog or OTLP export. No tamper evidence on the action and config logs. | P0 |
| Transport | The API is plain HTTP. `SAGE_TLS_CERT` and `SAGE_TLS_KEY` are read and never used (`config/config.go:650-653`). Targets default to `sslmode=prefer`. | P1 |
| Observability | Hand-written `/metrics` with no authentication. In meta mode, fleet metrics are missing and the connection metrics describe the meta DB. A documented alert metric does not exist. Logs are text only. | P1 |
| Upgrades | Idempotent DDL under an advisory lock with one 30-second budget. No version or downgrade guard for the core schema. The AgentDB schema version row flips back and forth when versions are mixed (`internal/agentdb/schema.go:75-78`). | P2 |

What this means for AgentDB: enterprise buyers of a governed database-for-agents control plane
will look first at SSO and group mapping, per-tenant RBAC, audit export, HA and secret-manager
integration. All five are missing from pg_sage itself, before AgentDB is considered at all.

---

## 11. Fit with the pg_sage core: what is reused and what is duplicated

`internal/agentdb` imports no other `internal/...` package; only `testdb` appears, in tests.
The only point of reuse is downstream: an agent DB that enters the fleet gets the standard
database runtime (collector, analyzer, `policy.Gate`, `Executor.Apply`) through
`connectAgentDBToFleet` (`cmd/pg_sage_sidecar/agentdb_fleet.go:176-206`). DP-18 makes that rare.

| Concern | Core | AgentDB's parallel version | Recommendation |
|---|---|---|---|
| Authorizing a mutation | `policy.Gate`, which already accepts non-SQL "trusted internal control" actions from an allowlist (`policy/gate.go:207-231`) | `EvaluateLiveProvisionPolicy` plus the effective-policy layers plus the live authorization ledger (`provider_policy.go`, `effective_policy.go`, `live_execution_*.go`) | Add `agentdb_provider_create` and `agentdb_provider_destroy` action types with typed contracts |
| Executing and verifying | `Executor.Apply`: Authorize, Admit, Execute, Verify, Refused (`executor/apply.go:21-35,93`) | `ExecuteProvisionLive` and `runProviderDestroy` (`live_create.go`; `execution.go:143-233`) | Admit = `ClaimLiveExecution`; Execute = `runner.Create`; Verify = poll until `available` or `destroyed` |
| Human approval | Core approvals and approval cards (`cmd/pg_sage_sidecar/api_server.go:125`) | Requests plus `authorize-live` plus deploy-request review | One queue, with a two-person rule per action class |
| Trust and autonomy | Trust levels and the earned-autonomy ledger (`policy.ActionRequest.IncidentFamily`, `policy/types.go:143-146`) | `auto_within_policy` (dead, DP-12) | Earn autonomy per provider and action class |
| Audit | `sage.action_log`, `config_audit` | `sage.agent_db_audit` and `agent_db_provision_attempts` (`schema_statements.go:317-325`) | One audit stream with export |
| Non-human tokens | `mcptoken`: hashed, scoped, expiring, narrowed to the owner's role, scoped to databases | `agt_` agent tokens (`agent_tokens.go:13`) plus ping tokens | Use MCP tokens with an `agentdb:*` scope |
| Emergency stop | `fleet/emergency_stop.go` | Reads `sage.config` directly (`store_options.go:50-72`) plus an in-memory gate | Use one gate, with "teardown allowed" semantics |
| Leases | Change leases (`policy/postgres_lease.go`) | Provider mutation lease (`teardown_mutation.go:55-99`) | Converge |
| Disposable databases | `clone.Provider` (no adapters) | Five runners | Merge (section 9.4) |
| Migration safety | `migration` lint, plan and rehearsal | Deploy requests (text) | Merge (section 9.4) |

---

## 12. Verdicts per provider path

First principle: agents need databases that are created in seconds, scale to zero, copy
cheaply (copy-on-write), carry per-branch credentials and expire on their own. Neon, Lakebase
(and, more heavily, Supabase) provide this natively; RDS and Cloud SQL instances do not
(8.5 and 14.6 minutes to available, a monthly floor, no branching). pg_sage's advantage is
governing and operating these databases (policy, approval, audit, monitoring, migration
safety), not provisioning instances.

| Path | Verdict | Rationale and conditions |
|---|---|---|
| Terraform plan text, Terraform templates, dry-run executor | **Delete** | Never executed, and no module exists (section 2.1). If an export is wanted, generate Terraform as a downloadable artifact from the approved spec. |
| AWS RDS instance runner | **Freeze, then delete** unless a design partner needs it | Slow and expensive, with no VPC, IAM auth or least-privilege identity (DP-15), no monitoring (DP-18), and scope-unsafe destroy (DP-02). If AWS matters, use Aurora fast clones (copy-on-write) or point to Neon or Lakebase on AWS. If kept, route through `Executor.Apply` with scope-bound receipts and install-id tags. |
| GCP Cloud SQL runner | **Delete** | Cannot hand out a credential; private-only creation is unsupported; static tokens; 14.6-minute creates (DP-16). If a customer needs it, rebuild on ADC, private IP and IAM database auth. |
| Databricks Lakebase branches | **Keep, simplify, delegate** | The right primitive (copy-on-write branch, native TTL, about 30 s). Send ownership metadata, use OAuth M2M, update the branch TTL when the lease is extended, adopt by deterministic branch id within the recorded scope, type the errors (DP-19), and re-validate the API shape with a live run. |
| Neon branches | **Keep and delegate** | Use Neon's branch expiry as the backstop TTL. Mint a per-branch role and connection URI through the API and deliver them through a secret reference. Adopt by name on an uncertain create (DP-07). Add a live run. |
| Neon projects, Supabase projects | **Delete** | A full billed project per agent is the wrong unit, and the Supabase master-key password derivation adds a key-management burden (`credentials.go:35-39`). |
| Supabase branches | **Narrow (optional)** | Keep only if customers ask. Each branch is a full project and needs a paid entitlement (`hosted_scope.go:35-59`). |
| `local_postgres` | **Replace** | Never on the control DB. Use a designated sandbox cluster: `CREATE DATABASE ... TEMPLATE` (PG15+ `STRATEGY FILE_COPY`) as "branching" for self-hosted Postgres; a per-agent role with GRANTs, `CONNECTION LIMIT` and `statement_timeout`; `DROP` on teardown; all through `Executor.Apply`. |
| Deploy requests | **Pivot** | Become migration PRs: lint, branch rehearsal with verbatim SQL, verify, rollback, then `policy.Gate` and `Executor.Apply` (section 9.4). Otherwise delete. |
| Live authorization ledger | **Keep the idea, move it into core** | Plan hash, estimate and single-use claim are good. Fix DP-03, DP-04, DP-05 and DP-11 by merging into `policy.Gate` and approvals. |
| Reconciler | **Keep and harden** | Compare-and-swap status writes (DP-08); bounded, ordered passes (DP-25); inventory sweep by `installation_id` tag (DP-09, DP-27); signals for blocks (DP-01, DP-07); a sane default backup gate (DP-01). |

Build order these findings suggest (for the spec):

1. **Stop the leaks.** DP-01 default, DP-02 scope binding, DP-07 classification and adoption,
   DP-09 installation id, block alerts.
2. **Delete dead surface.** Terraform, dry-run, Cloud SQL, project modes.
3. **Make created databases usable and monitored.** Per-branch role and credential handoff
   through a secret reference, fleet eligibility, `safety_mode` mapped to trust (DP-18).
4. **Fold provider mutations into `policy.Gate` and `Executor.Apply`,** with a two-person rule
   and stable policy versions.
5. **Pivot deploy requests into branch-rehearsed migration PRs.**
6. **Re-run live checks** for Neon and Lakebase, and keep the receipts.

---

## Appendix: method

- **Read in full.**
  - Every file in brief item 1: providers, runners, ownership, hosted, local, live create and
    execution, execution, deploy requests, request decision and provision, provision links,
    lifecycle and claims, teardown, Terraform, credentials, cost guard.
  - Also `provider_policy.go`, `effective_policy.go`, `register.go`, `queries.go` (register
    SQL), `schema.go`, `store.go`, `store_options.go` and `restore_drill.go`.
  - The API handlers that drive them: `agent_db_handlers.go`, `agent_db_execution_handlers.go`,
    `agent_db_live_authority.go`, `agent_db_register_handler.go`, `agent_db_request_handlers.go`,
    `agent_db_agent_api.go`, `agent_db_deploy_request_handlers.go`.
  - Wiring: `cmd/pg_sage_sidecar/agentdb_reconciler.go`, `agentdb_fleet.go`,
    `agentdb_secret_ref.go`.
  - Live tests: `aws_rds_live_test.go`, `gcp_cloudsql_live_test.go`,
    `live_product_chain_test.go`, `live_receipts_test.go`.
  - Docs: `docs/agent-db-deployments.md`, both runbooks, and the four `docs/reports/2026-05-*agentdb*` reports.
- **Searches** (`git grep`, all at `72646ab1`): `os/exec`, terraform invocations, `.Preflight(`,
  `AutoIssued`, `create_uncertain` in `api` and `web`, `teardown_blocked` outside agentdb,
  `CREATE ROLE`/`GRANT`, RDS network and auth fields, `DeleteDBSnapshot`,
  `last_validated_at`, `mcptoken` in agentdb, agentdb imports, leader election.
- **History:** `git blame` on `schema.go:109-125` and `request_provision.go:132-158`;
  `git log` on runner and live-test files.
- **Sub-audits:** sections 9 and 10 summarize parallel read-only audits; re-verified citations
  are listed at the head of each section.
- **Not done:** running tests, live cloud calls, database connections. Provider-behavior claims
  are marked PLAUSIBLE.
