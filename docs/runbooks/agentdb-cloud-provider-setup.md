# AgentDB Cloud Provider Setup

This page describes how to prepare AWS RDS, GCP Cloud SQL, and Databricks
Lakebase for AgentDB live provisioning. AgentDB can always run local dry-runs
and policy checks. Real cloud creation requires explicit sidecar enablement,
provider allowlists, cloud credentials outside pg_sage, and cleanup evidence.

## Release Safety Model

AgentDB separates four concerns:

- Provider settings in the UI store non-secret policy and shape defaults.
- Credentials come from the cloud SDK or environment at sidecar startup.
- Blueprints and Terraform templates are reviewed before provisioning.
- Live operations run only when global and provider-specific gates are enabled.

The UI should never be used to store cloud tokens, passwords, private keys, or
access keys. Secret-shaped keys are stripped before provider settings are saved.

## Sidecar Configuration

Start with live provisioning disabled, then enable one provider at a time:

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

The native runtime registry also requires these environment gates:

| Provider | Required live gates |
| --- | --- |
| AWS RDS | `PG_SAGE_LIVE_PROVISIONING=1` |
| GCP Cloud SQL | `PG_SAGE_LIVE_PROVISIONING=1`, `PG_SAGE_ENABLE_GCP_CLOUDSQL_RUNNER=1` |
| Databricks Lakebase | `PG_SAGE_LIVE_PROVISIONING=1`, `PG_SAGE_ENABLE_LAKEBASE_RUNNER=1` |

The live tests use separate test gates so they cannot run by accident:
`PG_SAGE_LIVE_AWS_RDS=1`, `PG_SAGE_LIVE_GCP_CLOUDSQL=1`,
`PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1`, and
`PG_SAGE_LIVE_AGENTDB_GAUNTLET=1`.

## Provider Settings UI

Use **Agent DBs -> Provider Settings** for non-secret provider policy:

- allowed regions, accounts, projects, or workspaces
- maximum TTL
- maximum estimated cost
- permitted instance classes or tiers
- default backup retention
- public IP policy

Use **Agent DBs -> Profiles** for reusable t-shirt sizes. Profiles can carry
provider parameters such as RDS instance class, Cloud SQL tier, Cloud SQL
edition, Lakebase mode, Lakebase project, and Lakebase source instance.

Use **Agent DBs -> Blueprints** when an operator or agent has an English
description of the desired database. The LLM converts intent into a typed
blueprint plus draft Terraform. The result still needs review before it can
provision.

Use **Agent DBs -> Terraform** when the team already has provider-specific
Terraform. Uploaded Terraform is static-scanned and stored as a review artifact;
it is not blindly applied.

## AWS RDS

### Credentials

pg_sage uses the AWS SDK default credential chain. Configure one of the normal
AWS identities outside pg_sage:

```powershell
aws sts get-caller-identity --output json
$env:AWS_PROFILE = "pg-sage-dev"
$env:AWS_REGION = "us-east-2"
$env:PG_SAGE_AWS_REGION = "us-east-2"
```

### Minimum IAM

- `rds:CreateDBInstance`
- `rds:DescribeDBInstances`
- `rds:DeleteDBInstance`
- `rds:AddTagsToResource`
- Secrets Manager read access only when using managed master password flows

### Required Shape Inputs

- `region`
- `db_instance_class`
- `allocated_storage`
- `backup_retention_days`
- TTL and budget

AgentDB tags disposable resources with `app=pg-sage` and
`pg_sage_deployment_id` so cleanup sweeps can find them.

## GCP Cloud SQL

### Credentials

Cloud SQL live execution currently requires an access token supplied at sidecar
startup. Use a short-lived token from a service account or ADC flow:

```powershell
gcloud auth application-default login
gcloud config set project <project-id>
$env:PG_SAGE_GCP_PROJECT = "<project-id>"
$env:PG_SAGE_GCP_REGION = "us-central1"
$env:PG_SAGE_GCP_ACCESS_TOKEN = (gcloud auth print-access-token)
```

For long-running sidecars, restart pg_sage with a fresh token before live
provisioning, or run the sidecar under a service account flow that refreshes the
token outside the UI.

### Minimum IAM

- `cloudsql.instances.create`
- `cloudsql.instances.get`
- `cloudsql.instances.delete`
- `cloudsql.operations.get`
- Secret Manager read access only if templates reference external secrets

### Required Shape Inputs

- `project`
- `region`
- `tier`
- `database_version`
- `edition`
- `storage_size`
- `ipv4_enabled`
- `require_ssl`

Use `db-f1-micro` or `db-custom-*` with Enterprise edition for low-cost tests.
Cloud SQL storage has a 10 GiB minimum.

## Databricks Lakebase

### Credentials

Use Databricks workspace auth outside pg_sage:

```powershell
databricks auth login --host <workspace-url>
databricks current-user me --output json
$env:PG_SAGE_DATABRICKS_HOST = "<workspace-url>"
$env:PG_SAGE_DATABRICKS_TOKEN = "<service-principal-token>"
$env:PG_SAGE_LIVE_LAKEBASE_PROJECT = "<lakebase-project>"
$env:PG_SAGE_LIVE_LAKEBASE_SOURCE_INSTANCE = "<source-instance-or-branch>"
```

`DATABRICKS_HOST` and `DATABRICKS_TOKEN` are also accepted by the runner.

### Permissions

- project or database instance read
- branch create
- branch delete
- service principal or workspace auth that can call Lakebase APIs

### Branches vs Instances

Prefer Lakebase branches for disposable agent workloads. Branches are cheaper
and faster to create than full instances, and they preserve an explicit source
instance or source branch. The AgentDB UI requires a source instance for branch
profiles and provisioning requests.

## Backup And Destroy Gates

Managed cloud providers can usually verify backup configuration, but backup
existence is not the same as restore proof. When `backup_required=true`, live
destroy is blocked until a restore-verified backup record exists.

Recommended production policy:

1. Daily provider backup check.
2. Periodic restore drill into a disposable target.
3. Operator marks restore verified in AgentDB.
4. Archive before delete.
5. Live destroy only after the restore gate passes.

## Live Validation Commands

Run live tests from `sidecar` in low-cost test accounts only:

```powershell
cd C:\Users\jmass\pg_sage\sidecar

$env:PG_SAGE_LIVE_AWS_RDS = "1"
go test -timeout 90m -count=1 ./internal/agentdb `
  -run TestAWSRDSLiveProvisioning -v

$env:PG_SAGE_LIVE_GCP_CLOUDSQL = "1"
$env:PG_SAGE_GCP_ACCESS_TOKEN = (gcloud auth print-access-token)
go test -timeout 90m -count=1 ./internal/agentdb `
  -run TestCloudSQLLiveProvisioning -v

$env:PG_SAGE_LIVE_DATABRICKS_LAKEBASE = "1"
go test -timeout 45m -count=1 ./internal/agentdb `
  -run TestLakebaseLiveProvisioning -v
```

Run the full product-chain gauntlet after individual providers pass:

```powershell
$env:PG_SAGE_LIVE_AGENTDB_GAUNTLET = "1"
go test -timeout 90m -count=1 ./internal/agentdb `
  -run TestAgentDBLiveGauntlet -v
```

## Cleanup Evidence

After every live run, confirm no disposable resources remain:

```powershell
aws rds describe-db-instances `
  --query "DBInstances[?contains(DBInstanceIdentifier, 'pgsage') || contains(DBInstanceIdentifier, 'pg-sage') || contains(DBInstanceIdentifier, 'agentdb')].[DBInstanceIdentifier,DBInstanceStatus,DBInstanceArn]" `
  --output table

gcloud sql instances list `
  --format="table(name,region,databaseVersion,state,settings.tier)" --quiet

databricks api get /api/2.0/database/instances
```

Live receipts are written to
`sidecar/test-output/agentdb-live-receipts.jsonl`. A release receipt should
include provider resource IDs, create/status/delete timing, backup status where
available, cost guard evidence, and `cleanup_confirmed=true`.
