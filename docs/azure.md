# Azure Database for PostgreSQL

pg_sage monitors and maintains Azure Database for PostgreSQL **flexible server**. Single
server is retired by Microsoft; pg_sage treats it as guidance-only.

Verified live (2026-09-27): flexible server PostgreSQL 14–18 on Burstable and General
Purpose tiers, and elastic clusters. PostgreSQL 11–13 can still be created on Azure but are
below pg_sage's minimum (14). Existing **Cosmos DB for PostgreSQL** clusters
(`*.postgres.cosmos.azure.com`) are detected as `azure-cosmos`: portable SQL actions run,
and server parameters are guidance-only. Azure no longer lets new Cosmos clusters be
created, so this path is covered by unit tests only.

## What pg_sage does on Azure

| Area | Behaviour |
|---|---|
| Detection | `<name>.postgres.database.azure.com` (including `privatelink`) hosts, or the `azure.extensions` server parameter, mark the target as `azure` |
| Portable actions | ANALYZE, VACUUM, CREATE/DROP/REINDEX INDEX CONCURRENTLY, per-table autovacuum settings, `ALTER DATABASE ... SET`, backend cancel/terminate, reviewed `ALTER TABLE` run as SQL through the standing policy gate like any provider |
| Instance parameters | `ALTER SYSTEM` is unavailable. Configuration changes use Azure **server parameters** through Azure Resource Manager when the `azure:` section is set; otherwise pg_sage reports the exact `az` command to run |
| Restart-bound parameters | Applied through ARM and reported as `applied_pending_restart` until the server restarts |
| Readiness | The fleet overview shows `azure_monitor` log access and the `azure.extensions` allow-list requirement for HypoPG, pg_hint_plan and auto_explain |

## Database setup

Run as the server admin (a member of `azure_pg_admin`):

```sql
CREATE USER sage_agent WITH PASSWORD 'YOUR_PASSWORD';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
```

Extensions must be allow-listed before `CREATE EXTENSION` works. In the portal (Server
parameters) or with the CLI, add them to `azure.extensions`, and add preload libraries to
`shared_preload_libraries` (a restart is required):

```bash
az postgres flexible-server parameter set -g RG -s SERVER --name azure.extensions --value "PG_STAT_STATEMENTS,HYPOPG,PG_HINT_PLAN"
az postgres flexible-server parameter set -g RG -s SERVER --name shared_preload_libraries --value "pg_stat_statements,pg_hint_plan"
az postgres flexible-server restart -g RG -n SERVER
```

## Letting pg_sage apply server parameters

```yaml
azure:
  subscription_id: 00000000-0000-0000-0000-000000000000
  resource_group: my-rg
  # server_name: only needed when the host is not <name>.postgres.database.azure.com
```

The same values can come from `SAGE_AZURE_SUBSCRIPTION_ID`, `SAGE_AZURE_RESOURCE_GROUP`
and `SAGE_AZURE_SERVER_NAME`. The server name from each database's host always wins, so a
fleet never applies one server's parameters to another.

Credentials are never read from the config file. pg_sage uses the Azure identity chain:
service-principal environment variables (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
`AZURE_CLIENT_SECRET`), workload identity, managed identity, then an `az login` session.
The identity needs `Microsoft.DBforPostgreSQL/flexibleServers/configurations/read` and
`.../write` on the server; the built-in Contributor role scoped to the server grants both.

At startup pg_sage logs either `azure server parameters apply through ARM for server NAME`
or why they stay guidance-only.

## Test server (live checklist)

`scripts/azure/` provisions a throwaway target and runs the checklist below:

```bash
az login
scripts/azure/provision-test-server.sh   # Burstable B1ms, ~USD 15-20/month while it exists
scripts/azure/live-checklist.sh          # CHECK-AZ-01/03/06/07/08; 02 comes from provisioning
scripts/azure/teardown-test-server.sh    # deletes the resource group
```

Provisioning creates resource group `pg-sage-test` (override with `AZ_RESOURCE_GROUP`,
`AZ_LOCATION`, `AZ_PG_VERSION`). It allow-lists HypoPG, pg_hint_plan and pg_stat_statements,
runs the setup SQL above, and gives the signed-in user Contributor on the server. Generated
credentials go only to `~/.pg_sage/azure-test.env` (mode 600) and are never printed.

## Verification checklist (first live run)

```
CHECK-AZ-01: sidecar logs "cloud environment: azure"
CHECK-AZ-02: the documented setup SQL runs as the server admin (record any refused GRANT)
CHECK-AZ-03: first collector snapshot appears in sage.snapshots within 2 minutes
CHECK-AZ-04: fleet readiness shows provider azure, log access azure_monitor
CHECK-AZ-05: an approved ANALYZE executes and verifies
CHECK-AZ-06: startup logs "azure server parameters apply through ARM" with azure: set
CHECK-AZ-07: PG_SAGE_LIVE_AZURE=1 go test ./internal/azure -run Live passes (sets and restores work_mem)
CHECK-AZ-08: PG_SAGE_LIVE_AZURE=1 go test ./internal/azure -run RestartBound passes (pending restart reported)
```
