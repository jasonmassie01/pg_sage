# Agent DB Deployments

Agent DB deployments let pg_sage act as the control plane for databases created
for agent workloads. The module is built for temporary and production-like agent
runs that need a database, tuning context, cost tracking, backups, query
recommendations, and automatic cleanup.

Local Postgres schema and database provisioning can execute against the
configured Postgres connection. Cloud providers support instance-level
preflight, dry-run execution, and gated live execution when the sidecar has
provider credentials, provider allowlists, and live provisioning enabled. See
[AgentDB Cloud Provider Setup](runbooks/agentdb-cloud-provider-setup.md) before
creating real cloud resources.

## Provisioning Model

| Provider | Schema | Database | Instance |
| --- | --- | --- | --- |
| `local_postgres` | Executed | Executed | Not supported |
| `aws_rds` | Not supported | Not supported | Dry-run or gated live |
| `gcp_cloudsql` | Not supported | Not supported | Dry-run or gated live |
| `databricks_lakebase` | Not supported | Not supported | Dry-run or gated live branch |

Each deployment has:

- `tenant_id`, `agent_id`, and optional `run_id` for ownership.
- `provider` and `provisioning_level` for the deployment boundary.
- `size_profile_id` for custom t-shirt sizing.
- `budget_usd` and cost samples for budget state.
- `lease_expires_at` and pings for abandoned database cleanup.
- `backup_required` and backup records for archive/delete safety.
- `metadata.workload_types` and `metadata.extensions` for tuning hints.

## Databricks Lakebase Notes

Lakebase is treated as a first-class Agent DB provider for instance-level
planning, branch provisioning, provider readiness, custom size profiles,
lifecycle status checks, destroy dry-runs, backup checks, and restore-drill
dry-runs. Live Lakebase branch execution is gated by Databricks credentials,
provider policy, and an explicit source instance or source branch.

Current support assumptions to verify against the target workspace:

- Lakebase Autoscaling supports Postgres 16 and 17; older Lakebase Provisioned
  instances are documented separately and may differ.
- Lakebase supports many Postgres extensions through `CREATE EXTENSION`,
  including `vector`/pgvector, PostGIS family extensions, `pg_stat_statements`,
  `pg_hint_plan`, `pg_trgm`, `pgcrypto`, `pg_prewarm`, and `pgstattuple`.
- Extension-dependent recommendations must still verify `pg_available_extensions`
  and `pg_extension` on the actual Lakebase database. pg_sage should not assume
  that a tenant has installed an extension just because Lakebase offers it.
- Lakebase is managed Postgres. Features that require host OS access, normal
  Postgres `superuser`, direct filesystem access, tablespaces, or instance-level
  GUC changes should be reported as provider limitations rather than actions.
- Lakebase parameter tuning should prefer session, database, or role-level
  settings where the setting context allows it. Instance-level settings remain
  outside pg_sage's current automation boundary.
- Native Postgres logical replication is documented as unavailable for Lakebase
  at this point, so replication-based migration or CDC advice needs a provider
  specific path.

References checked on 2026-05-08:

- Databricks Lakebase Postgres extensions:
  https://docs.databricks.com/aws/en/oltp/projects/extensions
- Databricks Lakebase Postgres compatibility:
  https://docs.databricks.com/aws/en/oltp/projects/compatibility
- Databricks Lakebase Provisioned compatibility:
  https://docs.databricks.com/aws/en/oltp/instances/query/postgres-compatibility

## Agent API Flow

Agents can request and maintain deployments without using the UI.

1. Create or update the agent identity:

```bash
curl -b cookies.txt -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/v1/agent-dbs/identities \
  --data '{
    "agent_id": "agent_orders_builder",
    "tenant_id": "tenant_acme",
    "owner_id": "team-platform",
    "display_name": "Orders Builder"
  }'
```

2. Request a deployment policy decision:

```bash
curl -b cookies.txt -H "Content-Type: application/json" \
  -H "Idempotency-Key: agent-orders-run-001" \
  -X POST http://localhost:8080/api/v1/agent-dbs/requests \
  --data '{
    "tenant_id": "tenant_acme",
    "agent_id": "agent_orders_builder",
    "requested_isolation_type": "instance",
    "provider": "gcp_cloudsql",
    "database_name": "orders_agent_run",
    "budget_usd": 100,
    "region": "us-central1",
    "allowed_regions": ["us-central1", "us-east1"],
    "data_classification": "pii",
    "masking_policy_id": "mask_pii_default",
    "approval_sla_seconds": 3600
  }'
```

3. Register or provision the deployment after approval:

```bash
curl -b cookies.txt -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/v1/agent-dbs \
  --data '{
    "deployment_id": "adb_orders_run_001",
    "tenant_id": "tenant_acme",
    "agent_id": "agent_orders_builder",
    "provider": "local_postgres",
    "provisioning_level": "schema",
    "schema_name": "agent_orders_001",
    "lease_seconds": 7200,
    "budget_usd": 25,
    "metadata": {
      "workload_types": ["jsonb", "vector"],
      "extensions": ["pgvector"]
    }
  }'
```

4. Mint a ping token. The bearer token is returned once.

```bash
curl -b cookies.txt -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/v1/agent-dbs/adb_orders_run_001/ping-tokens \
  --data '{"agent_id":"agent_orders_builder","expires_seconds":86400}'
```

5. Ping from the agent runtime. This route intentionally bypasses session auth
and relies on the bearer ping token.

```bash
curl -H "Authorization: Bearer $PING_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/v1/agent-dbs/adb_orders_run_001/agent-ping \
  --data '{"status":"active","metrics":{"tokens":12000,"queries":48}}'
```

## Token Lifecycle

Ping tokens are scoped to one deployment and one agent. pg_sage stores only a
hash, never the bearer secret.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/v1/agent-dbs/{id}/ping-tokens` | List redacted token metadata |
| `POST /api/v1/agent-dbs/{id}/ping-tokens` | Create a token and return the secret once |
| `POST /api/v1/agent-dbs/{id}/ping-tokens/{token_id}/rotate` | Mint a replacement and revoke the old token |
| `POST /api/v1/agent-dbs/{id}/ping-tokens/{token_id}/revoke` | Revoke a token |
| `POST /api/v1/agent-dbs/{id}/agent-ping` | Agent heartbeat with bearer token |

Failed token validation writes `ping_token_failed` audit events with only a hash
prefix. Repeated failures for the same deployment/token hash return HTTP `429`.

## Enterprise Policy Gates

Deployment requests are policy-scored before provisioning:

- Missing `tenant_id` or `agent_id` is denied.
- Restricted, production, PII, PHI, or PCI data without `masking_policy_id` is
  denied.
- Sensitive data with a masking policy is routed to review.
- A request outside `allowed_regions` is denied.
- Cloud instance requests without `budget_usd` are routed to review.
- `approval_sla_seconds` is preserved as a policy reason for review queues.

These gates are heuristics intended to be LLM-enriched later, but they are
deterministic enough to enforce enterprise guardrails in API and UI flows today.

## Tuning, Hints, and Query Recommendations

Use deployment metadata to publish tuning context:

```json
{
  "metadata": {
    "workload_types": ["vector", "postgis", "jsonb"],
    "extensions": ["pgvector", "postgis"]
  }
}
```

`GET /api/v1/agent-dbs/{id}/tuning-hints` returns extension, vector, PostGIS,
and JSONB tuning hints. `POST /api/v1/agent-dbs/{id}/recommendations` publishes
query tuning recommendations that an agent can consume and feed back through
`POST /api/v1/agent-dbs/{id}/recommendations/{recommendation_id}/feedback`.

## Backups, Cost, and Cleanup

- Managed Postgres deployments should record provider backup state through
  `POST /backups` and use `POST /backups/check` for daily assurance.
- Self-managed deployments should record backup and restore-drill outcomes.
- Archived deployments with `backup_required=true` cannot be deleted until a
  `restore_verified` backup exists.
- `POST /cost-samples` records usage or provider cost samples.
- `GET /cost` returns budget state and budget action.
- Expired leases are archived by `POST /api/v1/agent-dbs/cleanup`.

Live cloud setup, credential requirements, provider settings, and cleanup
commands are documented in
[AgentDB Cloud Provider Setup](runbooks/agentdb-cloud-provider-setup.md).

## Local UI Verification

The deterministic walkthrough fixture is the safest local smoke target:

```powershell
Set-Location C:\Users\jmass\pg_sage
docker compose -f .\docker-compose.test.yml up -d pg-target pg-target-2
powershell -ExecutionPolicy Bypass `
  -File .\test-fixtures\full_surface\run_walkthrough_fixture.ps1 `
  -SkipTests
```

It leaves the app running here:

```text
URL:      http://127.0.0.1:18085
Email:    admin@pg-sage.local
Password: CodexVerify123!
```

Manual API smoke:

```powershell
$body = @{
  email = "admin@pg-sage.local"
  password = "CodexVerify123!"
} | ConvertTo-Json

Invoke-WebRequest -UseBasicParsing `
  -Uri "http://127.0.0.1:18085/api/v1/auth/login" `
  -Method Post `
  -ContentType "application/json" `
  -Body $body `
  -SessionVariable session

Invoke-WebRequest -UseBasicParsing `
  -Uri "http://127.0.0.1:18085/api/v1/agent-dbs" `
  -WebSession $session
```

For Playwright against the fixture:

```powershell
Set-Location C:\Users\jmass\pg_sage\sidecar\web
$env:PG_SAGE_ADMIN_EMAIL = "admin@pg-sage.local"
$env:PG_SAGE_ADMIN_PASS = "CodexVerify123!"
$env:PG_SAGE_E2E_BASE_URL = "http://127.0.0.1:18085"
$env:PG_SAGE_E2E_FIXTURES = "1"
npm run test:e2e
```
