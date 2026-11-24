# Azure Database for PostgreSQL

pg_sage monitors and maintains Azure Database for PostgreSQL **flexible server**. Single
server is retired by Microsoft; pg_sage treats it as guidance-only.

Verified live (2026-09-27): flexible server PostgreSQL 14–18 on Burstable and General
Purpose tiers, and elastic clusters. PostgreSQL 11–13 can still be created on Azure but are
below pg_sage's minimum (14). Existing **Cosmos DB for PostgreSQL** clusters
(`*.postgres.cosmos.azure.com`) are detected as `azure-cosmos`: portable SQL actions run,
and server parameters are guidance-only. Azure no longer lets new Cosmos clusters be
created, so this path is covered by unit tests only.

### Cosmos DB for PostgreSQL

`azure-cosmos` is a managed provider in its own right, not a flexible server:

- **Configuration is guidance-only.** pg_sage has no adapter for Cosmos server parameters.
  Configuration findings never carry `ALTER SYSTEM` (or any executable SQL); they name the
  command to run, `az cosmosdb postgres configuration coordinator update --name <param>
  --value <value>`. The `azure:` ARM section does not apply.
- **Readiness has its own adapter.** The fleet overview shows `azure-cosmos` with
  `azure_monitor` log access, the `azure.extensions` allow-list, and the limitation
  "server parameters are guidance-only (Citus coordinator)". It does not advertise
  `ALTER DATABASE ... SET` changes, and the standing policy gate blocks them there
  (`provider_unsupported`).
- **Agent governance** (agent role contracts) covers Cosmos like the other managed
  providers.
- First-look preload steps for `pg_stat_statements` and `auto_explain` name the Cosmos
  coordinator command, not `az postgres flexible-server parameter set`.

## What pg_sage does on Azure

| Area | Behaviour |
|---|---|
| Detection | `<name>.postgres.database.azure.com` (including `privatelink`) hosts, or the `azure.extensions` server parameter, mark the target as `azure` |
| Portable actions | ANALYZE, VACUUM, CREATE/DROP/REINDEX INDEX CONCURRENTLY, per-table autovacuum settings, `ALTER DATABASE ... SET`, backend cancel/terminate, reviewed `ALTER TABLE` run as SQL through the standing policy gate like any provider |
| Database-scope settings | `ALTER DATABASE ... SET` is proposed only for settings whose `pg_settings.context` is `user`, or `superuser` when the role holds `SET` on the parameter (PostgreSQL 15+). `sighup` settings such as `max_wal_size`, `checkpoint_completion_target` and the `autovacuum_*` instance knobs, and `postmaster` settings, become advisory findings naming the server parameter; the executor refuses a stale `ALTER DATABASE` for them before it reaches PostgreSQL |
| Instance parameters | `ALTER SYSTEM` is unavailable. A setting SQL cannot change becomes a managed-change **proposal** (mechanism `server_parameter`) on the Actions page: the value in Azure's unit, whether a restart is needed, the rollback and the `az postgres flexible-server parameter set` command. An operator approves and runs it; pg_sage never writes these parameters |
| Restart-bound parameters | The proposal is marked `reboot_required` and adds `az postgres flexible-server restart`; pg_sage observes the running value afterwards and reports drift |
| ARM writes | Only the WAL-slot custodian's `max_slot_wal_keep_size` cap is written through Azure Resource Manager, and only when the `azure:` section is set; with `clone.provider: azure_restore` pg_sage also creates and deletes its own `sage-*` rehearsal servers (see [Restore rehearsal](#restore-rehearsal)) |
| Host telemetry | With `azure.subscription_id` and `azure.resource_group` (or a database's `cloud_resource.azure`), pg_sage reads the flexible server over ARM: its SKU (memory, from a dated table; an unknown SKU leaves memory unknown), storage auto-grow, its read replicas, and Azure Monitor metrics `cpu_percent`, `memory_percent`, `storage_used`, `storage_free`, `active_connections`, `read_iops`, `write_iops` and each replica's `physical_replication_delay_in_seconds`. They feed the memory gates, CPU index-build admission and the replica-lag and storage guards exactly as on RDS and Cloud SQL (see [managed-clouds.md](managed-clouds.md)). Reads only; a stale or missing point is unknown, never 0% |
| Readiness | The fleet overview shows `azure_monitor` log access. The `azure.extensions` allow-list requirement for HypoPG, pg_hint_plan and auto_explain appears as a separate `extension_hints` field (shown as "hint:" in the dashboard); the `extensions` field is what the catalog shows is installed. ANALYZE permission is `ok` for members of `azure_pg_admin`, otherwise `limited` (checked per table at execution). Capabilities are recollected every 15 minutes and after a config reload |

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

## Server parameters: proposals and the WAL-slot cap

pg_sage reads server parameters over Azure Resource Manager to fill each proposal (unit,
read-only flag, whether a restart is needed, the prior value for the rollback). The only
parameter it writes is `max_slot_wal_keep_size`, which the WAL-slot custodian sets to cap an
inactive replication slot's retained WAL. Every other change is an operator-approved
proposal, for example:

```bash
az postgres flexible-server parameter set --subscription SUB --resource-group 'RG' \
  --server-name SERVER --name shared_buffers --value 524288
az postgres flexible-server restart --subscription SUB --resource-group 'RG' --name SERVER
```

The rollback restores a user-set prior value, or returns the parameter to its default with
`--source system-default`. A read-only parameter gives no proposal. Without the `azure:`
section (or `cloud_resource.azure` for a fleet database) the proposal is a fill-in template
with `<resource-group>` and `<server>` placeholders and a blocker, never a runnable command.

```yaml
azure:
  subscription_id: 00000000-0000-0000-0000-000000000000
  resource_group: my-rg
  # server_name: only needed when the host is not <name>.postgres.database.azure.com
```

The same values can come from `SAGE_AZURE_SUBSCRIPTION_ID`, `SAGE_AZURE_RESOURCE_GROUP`
and `SAGE_AZURE_SERVER_NAME`. The server name from each database's host always wins, so a
fleet never reads or writes one server's parameters for another.

Credentials are never read from the config file. pg_sage uses the Azure identity chain:
service-principal environment variables (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
`AZURE_CLIENT_SECRET`), workload identity, managed identity, then an `az login` session.
Proposals need only `Microsoft.DBforPostgreSQL/flexibleServers/configurations/read` (the
Reader role grants it); a 403 names that permission in the managed-change worker's target
error. The WAL-slot cap also needs `.../configurations/write`; the built-in Contributor role
scoped to the server grants both.

At startup pg_sage logs either `azure server NAME: the WAL-slot cap (max_slot_wal_keep_size)
applies through ARM; other server parameters become operator-approved proposals with the az
command`, or why the cap stays guidance-only.

## Restore rehearsal

> **Cost warning: read this before the first run.** Azure flexible server has no
> copy-on-write clone. Every `azure_restore` rehearsal restores production into a **whole
> new flexible server** of the source's SKU and storage, billed at the **full price** from
> the restore until teardown: up to `clone.restore_deadline_minutes` of restoring plus up to
> an hour of rehearsal. A restore takes minutes to hours, growing with the data. pg_sage
> creates nothing unless you set `clone.allow_full_copy: true`, logs this cost at every
> startup, and writes each clone's instance-hours on its receipt before Azure is asked for it.

`clone.provider: azure_restore` rehearses a migration or an index on a point-in-time restore
of the monitored flexible server. Azure always restores to a **new server in the same region
and the same major version** as the source; pg_sage also keeps it in the same resource group.

```yaml
clone:
  provider: azure_restore
  allow_full_copy: true          # required: without it startup fails
  restore_deadline_minutes: 120  # 30-720: the longest a restore may take to be ready
  azure:
    subscription_id: 0f1e2d3c-4b5a-4697-8877-665544332211  # its subscription (required)
    resource_group: rg-shop-prod # its resource group (required)
    server: shop                 # its server name, not the FQDN (required)
```

What pg_sage does for each clone:

- The source is `clone.azure.server`. It must be a `Ready` primary (a read replica is
  refused), and pg_sage must connect to its FQDN, `<server>.postgres.database.azure.com`
  (an IP or another name is refused with "not the configured Azure flexible server"), so the
  clone is reached the same way.
- The point in time is five minutes ago (`AzureRestoreLag`). One before the server's
  `earliestRestoreDate` (a server created recently) is refused with reason
  `clone_unavailable`.
- It first checks that no server named `sage-<purpose>-<8 hex>` (lowercase, 3 to 63
  characters) exists in the resource group, because an ARM `PUT` on an existing server
  updates it; the `PUT` also sends `If-None-Match: *`. The `PUT` asks for
  `createMode: PointInTimeRestore` with the source's `sourceServerResourceId` and
  `pointInTimeUTC`, in the source's region, with the source's tags plus
  `pg-sage-receipt=<its name>` and `pg-sage-expires-at=<Unix seconds>`. A source in a
  delegated subnet is restored into the same subnet and private DNS zone; a source encrypted
  with a customer key keeps that key and its user-assigned identity, so a clone is never
  silently moved to service-managed keys. The clone has no high-availability standby and
  the shortest backup retention Azure allows (7 days). Azure keeps backups even for a
  rehearsal server, and may keep them for a while after it is deleted; pg_sage cannot turn
  that off.
- pg_sage polls the `Azure-AsyncOperation` URL (or `Location`) only when it is an operation
  of the configured subscription on the ARM endpoint; any other URL is never called (the
  bearer token goes with it), and the restored server is deleted.
- The clone's DSN is the monitored DSN on the new server's FQDN: the same users and
  passwords (a restore keeps the administrator login and its password at that point in
  time), `sslmode` at least `require`, port 5432 (the clone's built-in PgBouncer is not
  used). A clone that answers on production's FQDN is refused and deleted.
- Teardown deletes the server with an ARM `DELETE`. Only a `sage-*` server tagged with its
  own name is deleted. The restore tags the server as it creates it, so an untagged server
  is never pg_sage's: if Azure ever restores one without the tags, pg_sage reports it left
  behind and names the `az postgres flexible-server delete` command to run by hand, and the
  reaper keeps its receipt `create_uncertain` instead of closing it.
- A `PUT` Azure refused (4xx, including `409 ServerNameAlreadyExists`, since server names
  are global DNS names) made nothing. A 5xx or lost reply is looked up by name and deleted;
  a failed or canceled operation, or a server not `Ready` afterwards, is deleted before the
  error returns. A restore not done by the deadline leaves its receipt `create_uncertain`,
  and the reaper finds the server by name and destroys it. Azure has no server-side expiry,
  so the reaper is the backstop: check
  `az postgres flexible-server list --resource-group <group>` for `sage-*` servers if a
  sidecar was down for long.
- The receipt and the rehearsal bound cover the clone: a rehearsal may run
  `restore_deadline_minutes` plus one hour, and its receipt lives 15 minutes longer.
  Receipts are scoped `azure/<subscription>/<resource group>`.
- A caller that ends before `restore_deadline_minutes` is refused before the restore is
  asked for (reason `clone_unavailable`), so `apply_migration` over stdio MCP is
  recommendation-only with `azure_restore`, as with the other full-copy providers.

Credentials come from the same DefaultAzureCredential chain as server parameters and host
telemetry (service principal variables, workload or managed identity, then `az login`), built
on the first restore or teardown. Without them, or without the monitored pool, nothing is
created and rehearsals are recommendation-only with reason `clone_unavailable`. The API cannot
switch `clone.provider` to `azure_restore`; set it in YAML and restart.

The RBAC actions the identity needs on the resource group, on top of the telemetry reads (a
custom role; the built-in Contributor role on the resource group also covers them):

| Action | Used for |
| --- | --- |
| `Microsoft.DBforPostgreSQL/flexibleServers/read` | Read the source and the restored server |
| `Microsoft.DBforPostgreSQL/flexibleServers/write` | The point-in-time restore (`PUT`) |
| `Microsoft.DBforPostgreSQL/flexibleServers/delete` | Delete the restored server |
| `Microsoft.DBforPostgreSQL/locations/azureAsyncOperation/read`, `.../locations/operationResults/read` | Poll the restore |
| `Microsoft.ManagedIdentity/userAssignedIdentities/assign/action` | Only for a customer-key source: give the clone the source's identity |
| `Microsoft.Network/virtualNetworks/subnets/join/action` | Only for a delegated-subnet source |

A 401 or 403 names the action and the resource group. Restores land in the source's resource
group, so the role cannot be narrowed to the clones by scope alone; an Azure Policy or ABAC
condition on the `sage-` name prefix can narrow `write` and `delete`.

Cost: each receipt carries the instance-hours of one server for the receipt's lifetime
(`restore_deadline_minutes` plus 75 minutes), priced only by a `cost.rates` entry you supply
for the source (pg_sage ships no prices; storage and backups are not priced). The number that
says it works: restores that are ready within their estimate, their instance-hours, and
servers left after 24 hours (0).

The live check (`SAGE_AZURE_LIVE=1` with `SAGE_AZURE_LIVE_DATABASE_URL`,
`SAGE_AZURE_SUBSCRIPTION_ID`, `SAGE_AZURE_RESOURCE_GROUP`, `SAGE_AZURE_SERVER` and
`SAGE_TEST_DATABASE_URL`) runs the clone contract on a real, funded server; it is owner-run
and never in CI. It also confirms what the fakes assume: that the restore accepts the tags,
`If-None-Match: *`, `highAvailability`, `backup.backupRetentionDays`, the copied `network`,
`dataEncryption` and `identity`, and that a public-access clone keeps the source's firewall
rules (otherwise pg_sage cannot reach it and the rehearsal is recommendation-only).

## Test server (live checklist)

`scripts/azure/` provisions a throwaway target and runs the checklist below:

```bash
az login
scripts/azure/provision-test-server.sh   # Burstable B1ms, ~USD 15-20/month while it exists
scripts/azure/live-checklist.sh          # CHECK-AZ-01/03/06/07/08/10; 02 comes from provisioning
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
CHECK-AZ-06: startup logs "the WAL-slot cap (max_slot_wal_keep_size) applies through ARM" with azure: set
CHECK-AZ-07: PG_SAGE_LIVE_AZURE=1 go test ./internal/azure -run Live passes (sets and restores work_mem)
CHECK-AZ-08: PG_SAGE_LIVE_AZURE=1 go test ./internal/azure -run RestartBound passes (pending restart reported)
CHECK-AZ-09: MANUAL a server-parameter proposal's az command runs and parameter show reads the value back
CHECK-AZ-10: go test -tags cloudlive ./internal/cloudtel -run TestLiveAzureTelemetry reads fresh CPU and memory
CHECK-AZ-11: MANUAL GET /api/v1/provider-advice?database=<db> lists the server's index tuning recommendations (confirms the pinned api-version)
```

### Index tuning recommendations

With index tuning on (`index_tuning.mode = REPORT`), pg_sage reads the server's index
recommendations hourly (the ARM list behind `az postgres flexible-server index-tuning
list-recommendations`, api-version `2024-11-01-preview`, following at most 5 pages on the ARM
host) with the same credential and **Reader** role, keeps the ones for the monitored database
and gives each pg_sage's verdict (see [Provider advice](managed-clouds.md#provider-advice)).
`CreateIndex` and `DropIndex` are read; other recommendation types are shown as `unknown`. A
403 is recorded as the source's error and leaves earlier advice unchanged. The api-version is
pinned until the live check below confirms it (CHECK-AZ-11).

### Roles for host telemetry

The credential (azidentity's default chain: service principal variables, workload or managed
identity, then `az login`) needs, on the flexible server or its resource group:

- **Reader** (`Microsoft.DBforPostgreSQL/flexibleServers/read`): the server's SKU, storage and
  replicas.
- **Monitoring Reader** (`Microsoft.Insights/metrics/read`): the metrics.

Without them the telemetry status reads `unavailable: cloud telemetry access denied ... grant the
credential the Monitoring Reader (on the flexible server) role` and every guard keeps its
"unknown" behaviour; polling backs off and recovers by itself once the role is granted. A
throttled read (HTTP 429) is retried twice inside the poll before the poll fails.
