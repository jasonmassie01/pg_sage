# Cloud Provisioning Live Validation - 2026-05-09

## Scope

Validated actual cloud infrastructure creation for Agent DB provisioning using
the Cloud SQL Admin API. The pg_sage 8085 sidecar was used to create the Agent
DB deployment record and provider plan; the live cloud create/delete calls were
made through Google Cloud APIs because the current sidecar execute endpoint is
still intentionally wired to a dry-run runner.

## Provider Readiness

- GCP: authenticated with active account and ADC token for project
  `satty-488221`.
- AWS: CLI installed, but the session was expired and required reauth.
- Databricks: CLI profile was valid, but Lakebase live API shape still needs a
  dedicated validation pass.
- Terraform: not installed on this host.

## Live Cloud SQL Creation

- Project: `satty-488221`
- Region: `us-central1`
- Instance: `live-cloudsql-smoke-20260509121512`
- Database version: `POSTGRES_16`
- Tier: `db-f1-micro`
- Edition: `ENTERPRISE`
- Backup config: enabled
- Labels: `app=pg-sage`, `purpose=agentdb-live-smoke`, `owner=codex`,
  `ttl=1h`

The first attempted payload failed before creation because Cloud SQL defaulted
to Enterprise Plus, where `db-f1-micro` is invalid. The second failed before
creation because at least one connectivity path is required. The successful
payload explicitly set `edition=ENTERPRISE`, enabled public IPv4, required SSL,
and left authorized networks empty.

## Cleanup

- Delete operation: `384cedef-9763-4e7b-9922-452300000032`
- Final operation status: `DONE`
- Final instance check: not found / deleted

## Live AWS RDS Creation

- Profile/API: boto3 with the `jmass-admin` AWS profile.
- Region: `us-east-2`
- Instance: `pgsage-smoke-20260509174801`
- Engine: `postgres`
- Instance class: `db.t4g.micro`
- Allocated storage: 20 GiB
- Backups: enabled with one-day retention for the smoke test.
- Encryption: enabled.
- Tags: `app=pg-sage`, `purpose=agentdb-live-smoke`, `owner=codex`,
  `ttl=1h`

Result: the RDS create call was accepted, the instance reached `available`,
and the instance was deleted with automated backups removed.

The AWS default CLI login session was expired, but the `jmass-admin` SDK
profile was valid. No RDS credentials were written to disk or included in this
report.

## Live Databricks Lakebase Branch Creation

- Workspace: Databricks workspace `dbc-a03bc092-e6ba`
- API: Lakebase Postgres REST API under `/api/2.0/postgres/`
- Project: `demo`
- Source branch: `projects/demo/branches/br-empty-lake-d2ftn13l`
- Branch: `pgsage-smoke-20260509174724`
- Expiration: one-hour TTL
- Operation:
  `projects/demo/branches/pgsage-smoke-20260509174724/operations/CAIYASIkMzM3YzU3NzAtMDVmNy00ODI0LWI4MzItMjhmODdlMjM4YjNlKAA=`

Result: the Lakebase branch create call was accepted, the branch reached
`READY`, and deletion was verified by a final not-found readback.

## Product Changes Made

- Cloud SQL provision plans now include `edition`, `ipv4_enabled`, and
  `require_ssl` provider params.
- The default Cloud SQL size profile now carries the required live-create
  provider params.
- Agent DB size profile UI now exposes provider-specific cloud settings with
  mouseover documentation tips.
- Agent DB provisioning UI now exposes mouseover documentation tips on the
  provision form.
- Lakebase provisioning now supports selecting an autoscaling branch or a full
  provisioned instance from the provision form; the choice flows into
  `metadata.lakebase_mode` and overrides the Lakebase provider plan.
- Settings retention tab no longer renders the Shadow Mode report over the
  actual retention fields.

## UI Evidence

- `sidecar/test-output/settings-retention-fixed-8085.png`
- `sidecar/test-output/agentdb-cloud-settings-8085.png`
