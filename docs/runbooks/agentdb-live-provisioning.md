# AgentDB Live Provisioning Runbook

This runbook is for operators enabling AgentDB live provisioning after dry-run
validation. Live provisioning is intentionally gated: local/UI dry-runs should
pass before any real provider resource is created.

## Enablement

Live provisioning is off by default.

```yaml
agentdb:
  live_provisioning_enabled: false
  allow_public_ip: false
  require_backup_before_destroy: true
  providers:
    aws_rds:
      enabled: false
      allowed_regions: ["us-east-1"]
      allowed_accounts: ["123456789012"]
      max_ttl_seconds: 86400
      max_estimated_cost_usd: 5
    gcp_cloudsql:
      enabled: false
      allowed_regions: ["us-central1"]
      allowed_projects: ["example-project"]
      max_ttl_seconds: 86400
      max_estimated_cost_usd: 5
    databricks_lakebase:
      enabled: false
      allowed_workspaces: ["dbc-example"]
      max_ttl_seconds: 86400
      max_estimated_cost_usd: 5
```

Do not store cloud secrets in pg_sage config or provider settings. Provider
settings may store non-secret runtime knobs such as project, region, workspace,
allowed instance classes, and safety limits. Credentials should come from AWS
SDK default credentials, Google ADC/service account identity, or a Databricks
service principal/workspace-native auth flow.

## Credential Setup

AWS RDS:

```powershell
aws login
aws sts get-caller-identity --output json
$env:AWS_REGION='us-east-2'
$env:PG_SAGE_AWS_REGION='us-east-2'
```

GCP Cloud SQL:

```powershell
gcloud auth login
gcloud auth application-default login
gcloud config set project <project-id>
$env:PG_SAGE_GCP_PROJECT='<project-id>'
$env:PG_SAGE_GCP_REGION='us-central1'
$env:PG_SAGE_GCP_ACCESS_TOKEN=(gcloud auth print-access-token)
```

Databricks Lakebase:

```powershell
databricks auth login --host <workspace-url>
databricks current-user me --output json
$env:PG_SAGE_DATABRICKS_HOST='<workspace-url>'
$env:PG_SAGE_DATABRICKS_TOKEN='<service-principal-token>'
$env:PG_SAGE_LIVE_LAKEBASE_PROJECT='<lakebase-project>'
$env:PG_SAGE_LIVE_LAKEBASE_SOURCE_INSTANCE='<source-instance-or-branch>'
```

Lakebase branch provisioning requires an explicit source instance or source
branch. Full Lakebase instance provisioning and branch provisioning use the same
AgentDB lifecycle, but branch mode should be preferred for disposable agent
databases because it is faster to create, cheaper to hold, and easier to clean.

## Required Permissions

AWS RDS:
- `rds:CreateDBInstance`
- `rds:DescribeDBInstances`
- `rds:DeleteDBInstance`
- `rds:AddTagsToResource`
- `secretsmanager:*` only when using managed master user password flows

GCP Cloud SQL:
- `cloudsql.instances.create`
- `cloudsql.instances.get`
- `cloudsql.instances.delete`
- `cloudsql.operations.get`
- Secret Manager access only for credential references

Databricks Lakebase:
- Project/branch read
- Branch create
- Branch delete
- Token refresh or service principal auth

## Safety Gates

Every live request should have:
- TTL
- provider allowlist match
- cost estimate ID
- no public IP unless explicitly approved
- backup/restore verification before destroy
- `app=pg-sage` and `pg_sage_deployment_id` tags/metadata

Terraform uploads are static-scanned. `.tfvars`, state files, provisioners,
external data sources, `null_resource`, and unpinned external modules are
rejected.

## Live Test Commands

Run individual provider smoke tests:

```powershell
cd C:\Users\jmass\pg_sage\sidecar

$env:PG_SAGE_LIVE_AWS_RDS='1'
go test -timeout 90m -count=1 ./internal/agentdb -run TestAWSRDSLiveProvisioning -v

$env:PG_SAGE_LIVE_GCP_CLOUDSQL='1'
$env:PG_SAGE_GCP_ACCESS_TOKEN=(gcloud auth print-access-token)
go test -timeout 90m -count=1 ./internal/agentdb -run TestCloudSQLLiveProvisioning -v

$env:PG_SAGE_LIVE_DATABRICKS_LAKEBASE='1'
go test -timeout 45m -count=1 ./internal/agentdb -run TestLakebaseLiveProvisioning -v
```

Run the product-chain gauntlet, which starts from AgentDB artifacts and verifies
create, status, ping, cost sample, backup assurance, destroy, and cleanup
receipts:

```powershell
$env:PG_SAGE_LIVE_AGENTDB_GAUNTLET='1'
go test -timeout 90m -count=1 ./internal/agentdb -run 'TestAgentDBLiveGauntlet' -v
```

For GCP-only validation:

```powershell
$env:PG_SAGE_LIVE_GCP_CLOUDSQL='1'
$env:PG_SAGE_LIVE_AGENTDB_GAUNTLET='1'
$env:PG_SAGE_GCP_ACCESS_TOKEN=(gcloud auth print-access-token)
go test -timeout 90m -count=1 ./internal/agentdb -run 'TestCloudSQLLiveProvisioning|TestAgentDBLiveGauntletTerraformTemplateToCloudSQL' -v
```

Use explicit `-timeout`; managed database creates can take more than Go's
default ten-minute test timeout.

## Cleanup Sweeps

After every live run:

```powershell
gcloud sql instances list `
  --format='table(name,region,databaseVersion,state,settings.tier)' --quiet

aws rds describe-db-instances `
  --query "DBInstances[?contains(DBInstanceIdentifier, 'pgsage') || contains(DBInstanceIdentifier, 'pg-sage') || contains(DBInstanceIdentifier, 'agentdb')].[DBInstanceIdentifier,DBInstanceStatus,DBInstanceArn]" `
  --output table

databricks api get /api/2.0/database/instances
```

Expected cleanup evidence is either an empty provider-specific result for
`pgsage`/`agentdb` disposable resources or only known pre-existing resources.
Live test receipts are written to `sidecar/test-output/agentdb-live-receipts.jsonl`
and include provider resource id, create/delete timing, backup status where
available, cost guard text, and `cleanup_confirmed=true`.

## Emergency Stop

1. Set `agentdb.live_provisioning_enabled=false`.
2. Disable provider configs through `/api/v1/agent-dbs/provider-configs/{provider}`.
3. Stop sidecar workers if a provider API is behaving incorrectly.
4. Use cloud console/API to verify resources tagged `app=pg-sage`.

## Stuck Create

1. Check `/api/v1/agent-dbs/{id}/provision/attempts`.
2. Check `sage.agent_db_creation_receipts` for the provider resource ID.
3. Run provider status.
4. If cloud resource exists, reconcile should adopt the state.
5. If cloud resource is absent, mark the Agent DB failed and retry after policy review.

## Stuck Destroy

1. Confirm restore-verified backup exists.
2. Confirm the provider resource ID.
3. Run provider delete/status once.
4. Never delete untracked cloud resources automatically. Orphans require manual review.

Run live tests only in approved low-cost test accounts/projects/workspaces.
