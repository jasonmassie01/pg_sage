# Managed clouds: RDS, Aurora, Cloud SQL, AlloyDB and Azure

pg_sage reads the host telemetry that PostgreSQL cannot see on a managed service, and
turns setting changes that SQL cannot make into proposals with the exact provider command.

| Provider | Telemetry | Settings |
| --- | --- | --- |
| Amazon RDS for PostgreSQL | CloudWatch (CPU, freeable memory, free storage, IOPS, throughput, connections, replica lag), Performance Insights (DB load by wait type, OS memory total) | DB parameter group |
| Amazon Aurora PostgreSQL | CloudWatch (as RDS; `AuroraReplicaLagMaximum`), Performance Insights | DB parameter group of the writer |
| Google Cloud SQL for PostgreSQL | Cloud Monitoring (CPU, memory quota/usage, disk quota/usage, disk ops, backends, replica lag) | Database flags |
| Google AlloyDB for PostgreSQL | Cloud Monitoring `alloydb.googleapis.com/Instance` (CPU, minimum available memory, vCPUs for the memory total, connections, worst read pool lag); storage auto-grows | Database flags: a fill-in template until pg_sage reads the instance's flags |
| Azure Database for PostgreSQL flexible server | Azure Monitor metrics over ARM (CPU, memory percent with the SKU's memory, storage used/free, connections, IOPS, each replica's lag) | Server parameters, read over ARM when the `azure:` section is set ([Azure](azure.md)) |
| CloudNativePG Cluster (`cnpg`) | None | `spec.postgresql.parameters`, read from the Cluster when `cnpg:` names it ([CloudNativePG](#cloudnativepg)) |

Cosmos DB for PostgreSQL (`azure-cosmos`) and the retired Azure single server stay
guidance-only: their findings name the command, with no proposal.

## Provider detection

pg_sage names the provider of each database from its host name or, failing that, from SQL
probes (`aurora_version()` and the `rds.*`, `alloydb.*`, `cloudsql.*` and `azure.*`
settings). The last probe statement also recognizes Neon (a `neon.tenant_id` or
`neon.timeline_id` setting) and Supabase (the `supabase_admin` role) behind a custom domain,
so detection stays at five statements. Each probe has its own 2 second timeout. "Function
does not exist" or an unset setting means "not this provider"; no match means `self-managed`.

A probe that fails (a timeout, a dropped connection, a permission error) makes the provider
`unknown`, never `self-managed`. At startup the probes retry after 1, 2 and 4 seconds. An
`unknown` provider matches no action's provider list, so provider-gated actions such as
`ALTER SYSTEM` stay blocked, and readiness shows `provider unknown`. The runtime then
re-probes with backoff from 1 minute to 30 minutes. When a probe succeeds, pg_sage logs the
change and rebuilds that database's runtime (YAML fleet and meta-db databases). In
standalone mode, and for the control database of a YAML fleet, it cannot rebuild one
runtime, so readiness shows `provider re-probe found <provider>; restart pg_sage to apply`
until you restart.

### Provider override

`postgres.provider` (standalone) and `databases[].provider` (fleet) name the provider
yourself: `rds`, `aurora`, `cloud-sql`, `alloydb`, `azure`, `azure-cosmos`, `lakebase`,
`neon`, `supabase` or `self-managed`. Any other value fails validation. The override can
only narrow what detection says:

- While the probes say `unknown`, the override is used. The re-probe keeps running; when it
  agrees, nothing changes, and when it disagrees, the runtime is rebuilt with the detected
  provider.
- When it agrees with detection, it changes nothing.
- When it disagrees, pg_sage keeps the detected provider and logs a warning. In particular
  `self-managed` never replaces a detected managed provider, so it cannot unlock
  `ALTER SYSTEM` on RDS, Cloud SQL or Azure.

Meta-db (store) databases have no override key; their provider always comes from detection.

## What the telemetry is used for

Telemetry is evidence only. It never raises a trust level or grants an action: every action
still goes through the policy gate and the trust ledger.

- **Host memory.** `shared_buffers` proposals need the host's RAM (PostgreSQL does not expose
  it). With telemetry they are grounded (at most 40% of RAM) and become managed proposals.
  `work_mem` is capped at 5% of RAM, and while available memory is under
  `cloud_telemetry.min_available_memory_pct` no memory setting may grow.
- **CPU headroom.** Provider CPU feeds index-build load admission, so autonomous builds can be
  admitted outside a maintenance window when CPU is under the ceiling (without CPU evidence they
  are admitted only inside a window).
- **Replica lag, storage runway, memory pressure.** Autonomous index builds wait while a replica
  lags more than `max_replica_lag_seconds`, bounded free storage (including the autoscaling
  headroom) is under `min_free_storage_pct`, storage at its recent rate runs out within
  `min_storage_runway_hours`, or available memory is under the floor. A
  `managed_storage_runway` finding opens a week ahead.
- **Retained WAL and the disk runway.** PostgreSQL cannot see the volume size on a managed
  provider, so the WAL custodian's `custodian.wal.retained_wal_disk_pct_ceiling` uses the
  provider's storage capacity as its denominator. A slot holding more than that share of the
  volume gets the cluster-wide `max_slot_wal_keep_size` bound proposal (applied through the
  managed-config adapter, still behind the policy gate), or a plan-only escalation when a safe
  bound would not fit on the volume. The proposal evidence carries `capacity_source`
  (`provider_telemetry`), `disk_capacity_bytes` and `disk_pct`. The runway monitor's disk
  forecast also projects against a fresh provider capacity before `forecaster.disk_capacity_bytes`,
  and the finding names its `capacity_source` (`provider_telemetry` or `declared`).
  Capacity only adds proposals. It never admits an action, and replication slots are never
  dropped. Without fresh telemetry (none configured, unavailable, or a stale sample), the
  custodian and the runway behave exactly as they do on self-managed PostgreSQL: the byte
  backstop and the declared capacity.
  Storage that grows without a bound has no capacity, so there is no disk percentage. That is
  Aurora (the cluster volume grows to 128 TiB) and Cloud SQL with automatic storage increase and
  no limit. A bound proposed there by the byte backstop records
  `capacity_source=capacity_unbounded`. A Cloud SQL auto-resize limit counts as the capacity.
  In fleet mode each database uses its own telemetry runtime.
- **Backup freshness.** The provider's last good backup opens `backup_stale` when it is older
  than `backup.max_age_hours` (26), and a retention under `backup.min_retention_days` (7) opens
  `backup_retention_below_policy`. RDS reports `LatestRestorableTime` in the instance
  description and Aurora in the cluster description, both already read, so no extra call is
  made. Cloud SQL's instance has the backup settings only: pg_sage lists the 5 newest backup
  runs (`backupRuns.list`, in `roles/cloudsql.viewer`) at most every 15 minutes and takes the
  latest successful one. A denied or failed list keeps the backup settings, marks freshness
  unavailable (logged once per reason) and leaves `backup_stale` as it was; unknown is never
  "ok". `pg_stat_archiver` is still read on managed servers, but providers often run their own
  archiving, so the provider's restorable time is the evidence to trust there. See
  [Backups and WAL archiving](findings.md#backups-and-wal-archiving).
- **Backup isolation.** A backup deleted in the same call that deletes the database is no
  backup against that call. pg_sage records whether any backup it can see outlives a delete
  of the instance, and how far back point-in-time recovery reaches:
  - RDS: one `DescribeDBSnapshots` (manual snapshots, 20 records) and one
    `DescribeDBInstanceAutomatedBackups` (the active backup's restore window) per 6 hours,
    15 minutes after a failed read. A manual snapshot no older than `backup.max_age_hours`,
    an AWS Backup recovery point or a cross-region backup replication (both in the instance
    description) outlives the instance. Automated backups alone do not: `DeleteDBInstance`
    deletes them unless the caller asks to retain them.
    When more than 20 manual snapshots exist and none of the first 20 is recent enough,
    isolation is unknown (the newest may be on a later page), not false.
  - Aurora, and an RDS Multi-AZ DB cluster member: one `DescribeDBClusterSnapshots`
    (manual) per 6 hours; the window is the cluster's latest minus earliest restorable
    time, and on Aurora deletion protection is the cluster's, all in the cluster
    description.
  - Cloud SQL: `settings.retainBackupsOnDelete` in the instance description; the window is
    the transaction log retention while point-in-time recovery is on.
  - AWS Backup vaults, exports and copies in other accounts are not read; name one in
    `backup.external_source` and `backup_shares_delete_path` resolves. Azure, AlloyDB, Neon
    and Lakebase report isolation unknown (no adapter reads it yet).

  Findings: `backup_shares_delete_path` (warning; critical on a production database with the
  agent gate on), `deletion_protection_off` (warning, production only) and
  `pitr_window_below_policy` (below `backup.min_pitr_hours`, 24). An unknown value evaluates
  nothing, so an open finding stays open; a denied read is logged once per reason and
  leaves the rest of the posture as read.
- **Parameter drift.** Values set in the parameter group or flags that PostgreSQL does not run
  (pending a reboot, overridden by a database/role setting, or not in effect) open
  `managed_parameter_drift` findings.

## Which settings run as SQL

On every managed provider `ALTER SYSTEM` is unavailable, so the configuration advisor
decides per setting, from the `context` column of `pg_settings` that pg_sage collects each
cycle, whether `ALTER DATABASE <db> SET` can carry the change:

| `pg_settings.context` | What pg_sage proposes |
|---|---|
| `user` (`work_mem`, `random_page_cost`, `maintenance_work_mem`, ...) | `ALTER DATABASE <db> SET ...`; the executor captures the prior database setting and rolls back to it |
| `superuser` | `ALTER DATABASE <db> SET ...` only when the connected role is a superuser or, on PostgreSQL 15+, holds `SET` on the parameter (`has_parameter_privilege`); otherwise no SQL. PostgreSQL 14 has no parameter privileges |
| `sighup` (`max_wal_size`, `min_wal_size`, `checkpoint_*`, the `autovacuum_*` instance knobs) and `postmaster` | No SQL: PostgreSQL refuses `ALTER DATABASE SET` for these. A managed proposal on RDS, Aurora, Cloud SQL, AlloyDB, Azure flexible server and CloudNativePG; guidance on Cosmos DB for PostgreSQL |
| Not collected or any other context | No SQL (fail closed), plus the managed proposal where the provider has one |

Before any `ALTER DATABASE ... SET` runs on a managed provider, the executor re-reads the
setting's context and the role's privilege and refuses one PostgreSQL would refuse, so a
finding written by an older pg_sage cannot reach the database.

## Managed proposals

A restart-required or server-level setting (for example `shared_buffers`,
`max_connections`, `max_wal_size` on RDS) becomes a proposal on the Actions page with the
parameter group (or flag list), the value in the provider's unit, whether a reboot is needed,
the rollback and the command, for example:

```
aws rds modify-db-parameter-group --region us-east-1 --db-parameter-group-name orders-pg16 \
  --parameters '[{"ParameterName":"shared_buffers","ParameterValue":"524288","ApplyMethod":"pending-reboot"}]'
aws rds reboot-db-instance --region us-east-1 --db-instance-identifier orders
```

pg_sage never applies these changes in this release. Approve one when you will run it; pg_sage
marks it applied once PostgreSQL runs the new value. A default parameter group (`default.*`)
cannot be modified, and a Cloud SQL `--database-flags` command replaces the whole flag list:
the proposal says so and carries the current flags.

On Azure flexible server the proposal (mechanism `server_parameter`) is an
`az postgres flexible-server parameter set` command, plus `az postgres flexible-server restart`
when the parameter is static. pg_sage reads the parameter over ARM for its unit, read-only flag,
restart need and prior value; a read-only parameter gives no proposal, and without the
`azure:` section the command is a fill-in template with a blocker. On AlloyDB the proposal is a
`gcloud alloydb instances update ... --database-flags=` change; pg_sage does not read AlloyDB
flags yet, so it is always a fill-in template with a blocker (the flag list is replaced
whole).

### Command and blocker rules

The commands are written for a POSIX shell (bash, zsh, Cloud Shell). pg_sage validates every
part before it builds one:

- The parameter name, the proposed value and the group, region, project and instance names
  must match strict patterns; anything else is refused and no proposal is made.
- Values read back from the provider (the current Cloud SQL flags, the current RDS value used
  by a rollback) are always single-quoted. They may hold letters, digits and
  `. _ + - , / : @ | ~ % { } *`, up to 256 characters. Any other character (whitespace, a
  quote, a backslash, `=`, `$`, `;`, a backtick, a control character and so on), or a flag
  name that is not a valid parameter name, gives a blocker naming the setting. For Cloud SQL
  both the command and the rollback become `#` comment lines, since each carries the whole
  flag list; for RDS and Aurora only the rollback does. Make that change in the console
  instead. A flag such as `log_line_prefix` with a space in its value blocks by design.
- RDS and Aurora `--parameters` is always the JSON form, so list values such as
  `pg_stat_statements,pg_hint_plan` stay one value.
- A Cloud SQL flag value with a comma (for example `pgaudit.log=read,write`) switches the flag
  list to gcloud's alternate delimiter syntax (see `gcloud topic escaping`):
  `--database-flags='^@^pgaudit.log=read,write@work_mem=65536'`. The delimiter is the first
  of `@ : | ~` that no value contains; when every one occurs, the proposal is blocked.
- When the current Cloud SQL flags are unknown, both the command and the rollback are
  `# fill in:` templates that name the `gcloud sql instances describe` call to run first.
  `--clear-database-flags` appears only in the rollback of an instance known to have had no
  flags.
- A proposal with any blocker cannot be approved on the Actions page (the button says why); it
  can still be rejected. Open proposals get rebuilt commands on the worker's next cycle.

### Proposal identity, outages and rejections

A proposal is identified by its provider, parameter and value. The parameter group or
instance it targets is refreshed on every cycle but is not part of its identity, so an
approval, its note and a rejection all survive a change of target and a failed lookup:

- When looking up the instance fails (a throttled `DescribeDBInstances`, an expired token),
  open proposals keep the target last resolved, with a note naming the error and when that
  target was resolved. If nothing was resolved since pg_sage started, a new proposal carries
  placeholders, but a stored proposal with a real command is never overwritten with
  placeholders. No proposal is superseded in a cycle whose lookup failed, and drift is not
  reported from a stale target.
- A rejected change is not proposed again for 7 days, whatever the target lookup returns.
- A proposal is superseded when its finding is gone or a newer value for the same parameter
  is proposed.
- Databases on one instance share its parameter group, so a change one of them holds (open,
  or rejected in the last 7 days) for the same parameter group and value is not proposed again
  by the others: approve or reject it in the database that holds it. A database that cannot
  look up the parameter group right now does not add a placeholder copy of a change a peer
  holds. The cycle counts these as
  `held_by_peer`, and `GET /api/v1/managed-changes` lists each database's
  `member_databases`. A different value is still proposed. If a peer database cannot be read,
  the change is proposed anyway (it may then be listed twice) and the failure is logged once.
  A database that already held its own copy before this release keeps refreshing it.

Upgrading from v2.5.1 or earlier recomputes the stored identities once; open proposals keep
their ids, status and decisions. Duplicates left by earlier outages (the same change open
twice under different targets) collapse to one, keeping the approved one, else the newest.

## CloudNativePG

A [CloudNativePG](https://cloudnative-pg.io) Cluster runs ordinary PostgreSQL, so detection
calls it `self-managed`. But the operator writes `postgresql.conf` from the Cluster's
`spec.postgresql.parameters` and disables `ALTER SYSTEM` (on PostgreSQL 17 and later it
shows as `allow_alter_system = off`), so a change pg_sage made the self-managed way would
fail or be lost on the next reconcile. Name the Cluster:

```yaml
postgres:              # standalone; in fleet mode set databases[].cnpg
  cnpg:
    cluster: pg-main   # the Cluster's metadata.name
    namespace: db      # its namespace
```

With the block the provider is `cnpg`: a refinement of `self-managed` (or of a not yet
known provider), never of a detected cloud. If detection names RDS, Cloud SQL or another
cloud, pg_sage keeps it, ignores the block and logs a warning. `cnpg` cannot be combined
with `provider:` (other than `self-managed`) or `cloud_resource`.

On a `cnpg` database:

- `ALTER SYSTEM` is never issued. `user` and `superuser` settings keep the
  `ALTER DATABASE <db> SET` rewrite (see [Which settings run as SQL](#which-settings-run-as-sql));
  `sighup` and `postmaster` settings become a managed proposal (mechanism `cluster_spec`).
- The proposal is a merge patch of the one parameter, in PostgreSQL's form, for example
  `kubectl patch cluster pg-main -n db --type merge -p '{"spec":{"postgresql":{"parameters":{"work_mem":"64MB"}}}}'`.
  Its rollback restores the prior value, or patches the key to `null` when the Cluster did not
  set it. It also carries the same change as a Cluster manifest fragment (`gitops`) for a pull
  request: when Argo CD or Flux owns the Cluster, commit that instead, or the next sync reverts
  the patch.
- A setting that needs a restart (`shared_buffers`, `max_connections`) makes the operator
  restart the replicas one at a time and then the primary. Under
  `primaryUpdateStrategy: unsupervised` (the default) it does so at once, restarting the
  primary in place or switching over to a replica (`primaryUpdateMethod`): a short write
  outage. Under `supervised` it waits for you. The proposal notes which strategy the Cluster
  uses.
- Parameters the operator owns (`listen_addresses`, `port`, `archive_command`,
  `full_page_writes`, `hot_standby`, `wal_log_hints`, the `ssl*`, logging-collector and
  recovery settings, and `shared_preload_libraries` and `synchronous_standby_names`, which
  have their own Cluster fields) give no proposal.
- pg_sage never patches the Cluster. It reads it: a GET of the one named Cluster with the
  pod's service-account token, bounded at 5 seconds, which fills the current values, the
  rollback, the "now running" check and [drift](#proposal-identity-outages-and-rejections).
  The [Helm chart](deployment.md#kubernetes) creates a Role that grants only `get` on the named
  `clusters.postgresql.cnpg.io`. Without it (a 403), or outside a pod, proposals carry
  `<cluster>`/`<namespace>` placeholders and a fill-in rollback, and no drift is reported.
- Databases of one fleet on the same Cluster share it: a change one of them holds is not
  proposed again by the others, and two different values for one parameter are both shown,
  each with a blocker that names the other database's value.

Without a `cnpg` block, a self-managed database whose `archive_command` runs the
CloudNativePG instance manager (`/controller/manager`), or whose `allow_alter_system` is
`off`, opens the finding `cnpg_target_unconfigured`; on PostgreSQL 16 and earlier
`allow_alter_system` does not exist and `archive_command` alone decides. The finding
resolves once the block names the Cluster.

## Maintenance privileges (VACUUM and autovacuum settings)

On managed services pg_sage usually connects as a role that does not own the application's
tables. PostgreSQL then skips a `VACUUM` with only a WARNING, and refuses `ALTER TABLE ... SET`.
Before either action pg_sage reads the catalog once and withholds the action when the role
cannot run it. The decision is recorded as `blocked` with reason `capability_missing`, and its
evidence (`maintenance_privilege.detail`) names the grant that fixes it:

| Action | Allowed for |
|---|---|
| `VACUUM` (`vacuum_table`) | the table owner (or a member of the owning role), a superuser, or the database owner; on PostgreSQL 17+ also `MAINTAIN` on the table or membership of `pg_maintain` |
| `ALTER TABLE ... SET (autovacuum_*)` (`set_table_autovacuum`) | the table owner or a superuser only (`MAINTAIN` does not cover `ALTER TABLE`) |

On PostgreSQL 17 and later, grant pg_sage's role one of these:

```sql
GRANT MAINTAIN ON TABLE app.orders TO sage_agent;  -- one table
GRANT pg_maintain TO sage_agent;                  -- every table in the database
```

PostgreSQL 14 to 16 have no `MAINTAIN` privilege: VACUUM needs ownership, the database owner or
a superuser. Providers grant `pg_maintain` differently (for example through their admin role);
pg_sage checks membership, so any route that makes the role a member counts. Autovacuum
settings always need the owner: run them as the owner, or leave them as proposals.

A failed privilege read withholds the action with reason `capability_unverified`; it never runs
unchecked. A statement naming more than one table (or none, as a database-wide `VACUUM`) is
withheld with `capability_missing`, because only one table can be checked: run one statement
per table.
If a VACUUM still returns without running (its `vacuum_count` did not advance and
the role lacks the privilege), the action is recorded as `failed` with "VACUUM skipped (no
privilege)", not as a neutral result.

## Identity and credentials

The resource is named by the connection host: an RDS instance endpoint, an Aurora cluster
(writer) endpoint, a Cloud SQL Auth Proxy socket (`/cloudsql/<project>:<region>:<instance>`),
or a Cloud SQL IP address (matched through the Admin API). A host that names the resource
always wins. When it does not, name the resource with `cloud_resource` on `postgres:`
(standalone) or on each `databases[]` entry (fleet). The older standalone-only
`cloud_telemetry.aws.*` and `cloud_telemetry.gcp.instance` names still work when
`cloud_resource` is empty.

```yaml
databases:
  - name: orders                 # RDS Proxy, an IP or custom DNS
    host: orders-proxy.proxy-abc123.us-east-1.rds.amazonaws.com
    provider: rds
    cloud_resource:
      aws:
        db_instance_identifier: orders-prod   # or cluster_identifier for Aurora
        region: us-east-1
  - name: billing                # Cloud SQL Auth Proxy on TCP
    host: 127.0.0.1
    provider: cloud-sql
    cloud_resource:
      gcp:
        project: my-project
        instance: billing-main
  - name: ledger                 # Azure flexible server behind private DNS
    host: ledger.db.internal
    provider: azure
    cloud_resource:
      azure:
        subscription: 00000000-0000-0000-0000-000000000000
        resource_group: rg-ledger
        server: ledger-flex
```

- **Cloud SQL Auth Proxy.** Through the unix socket (`host=/cloudsql/<project>:<region>:<instance>`)
  the socket names the instance: it is read directly and never listed. Through TCP on
  `127.0.0.1` (or any loopback address, or `localhost`) the address never matches an
  instance. Without `cloud_resource.gcp.instance` telemetry reports unavailable, names the
  Auth Proxy, and makes no Google API call.
- **RDS Proxy.** A proxy endpoint (`<proxy>.proxy-<id>.<region>.rds.amazonaws.com`) does not
  name the instance behind it. Set `cloud_resource.aws.db_instance_identifier` (or
  `cluster_identifier` for Aurora) and `region`. The SQL probes still see the real server, so
  the provider is detected as usual.
- **Custom DNS or an IP.** Set the `cloud_resource` block of the cloud. For Cloud SQL by IP the
  instance is found by its IP once through `instances.list`; naming it skips that call.
- **Azure.** `cloud_resource.azure` (`subscription`, `resource_group` and `server`, all three)
  wins over `azure.*` for that database. A `<name>.postgres.database.azure.com` host still
  names the server.

A `cloud_resource` block names one cloud, and when `provider` is set it must match it: `aws`
for `rds` or `aurora`, `gcp` for `cloud-sql`, `azure` for `azure`. The block identifies the
resource only; it never changes the detected provider.

An Aurora instance endpoint (`writer-1.abc.us-east-1.rds.amazonaws.com`) is Aurora, not
RDS, when PostgreSQL reports Aurora (`aurora_version()`) or `DescribeDBInstances` reports an
`aurora*` engine; this holds in fleet mode too, where each member may use its own instance
endpoint. The telemetry status (`GET /api/v1/cloud-telemetry`) then carries `cluster` and
`instance_role` (`writer`, `reader`, or `unknown` when `rds:DescribeDBClusters` is denied:
metrics are still collected). Managed proposals name the cluster; a reader's proposal adds
that its DB parameter group may differ from the writer's. When the detected platform and the
instance's engine disagree (PostgreSQL says Aurora, AWS says `postgres`, or the reverse), the
managed-change worker reports one target error naming the instance and both providers, and
its proposals keep placeholders instead of naming a group of the wrong kind of resource.

Credentials come only from the standard chains and are never stored, logged or put in errors:

- **AWS:** the default chain (environment, shared config/SSO, web identity, ECS task or EC2
  instance role). Read-only permissions: `cloudwatch:GetMetricData`, `pi:GetResourceMetrics`,
  `rds:DescribeDBInstances`, `rds:DescribeDBClusters`, `rds:DescribeDBParameters`, and for
  backup isolation `rds:DescribeDBSnapshots`, `rds:DescribeDBClusterSnapshots` and
  `rds:DescribeDBInstanceAutomatedBackups`.
- **Google Cloud:** Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS` with a
  service-account key or user credentials, `gcloud auth application-default login`, or the
  metadata server on GCE/GKE/Cloud Run). Roles: `roles/monitoring.viewer`,
  `roles/cloudsql.viewer` (it includes `cloudsql.backupRuns.list` for backup freshness). The
  project comes from the Auth Proxy socket,
  `cloud_resource.gcp.project`, `cloud_telemetry.gcp.project` or the credentials, in that
  order. AlloyDB needs only `roles/monitoring.viewer`; its project comes from
  `cloud_resource.gcp.project`, `cloud_telemetry.alloydb.project` or the credentials.
- **Azure:** azidentity's default chain (service principal variables, workload or managed
  identity, `az login`). Roles on the flexible server: **Reader** (server SKU, storage,
  replicas) and **Monitoring Reader** (metrics). See [Azure](azure.md#roles-for-host-telemetry).

A denied read names the missing role in the status reason. A throttled read (HTTP 429) is
retried twice inside the poll (0.5 s then 1 s, or the provider's `Retry-After` up to 5 s, never
past the poll's deadline) before the poll fails as throttled.

Without credentials telemetry reports `unavailable: <reason>` (see
`GET /api/v1/cloud-telemetry`) and pg_sage behaves as before.

Failed polls back off instead of retrying every interval. After *n* failures in a row the next
poll waits the poll interval × 2^(n-1): up to 30 minutes when credentials are missing, the
database identity is unknown or access is denied, and up to 10 minutes (±20 % jitter) for
throttling, provider, response, clock-skew and network errors. A ceiling never shortens a poll
interval that is already longer. One success returns to the poll interval. pg_sage keeps
retrying, so credentials added later are picked up within 30 minutes; reloading the
configuration rebuilds the runtime and polls at once. When other databases share the
instance's runtime, reloading one of them keeps the runtime (and its credentials chain) and
polls at once. The log gets one warning when a failure starts or its reason changes and one
line when telemetry recovers, not one per poll.
`GET /api/v1/cloud-telemetry` shows `next_poll_at` and `consecutive_failures` for each
database. A failed poll still drops the previous sample, so the guards never use stale
evidence.

## Instance discovery (RDS and Aurora)

A meta-db fleet can list tagged PostgreSQL instances with an `rds` source
under `fleet.discovery.sources` (see
[the fleet walkthrough](walkthrough-fleet.md#discover-databases-automatically)).
It pages `DescribeDBInstances` filtered to the `postgres` and
`aurora-postgresql` engines (at most 200 instances and 20 pages a cycle;
a repeated marker stops the walk) and keeps the instances carrying
`tag_key` (with `tag_value` when set), keyed by `DbiResourceId`. The AWS
credentials come from the default chain for `region` and need
`rds:DescribeDBInstances` only. RDS candidates are never enrolled
automatically (there is no IAM database authentication path); add the
ones you want by hand. An access-denied or other listing error keeps the
previous candidates and is shown on the fleet page's Discovered tab.

## Configuration

```yaml
cloud_telemetry:
  enabled: true                 # default
  poll_interval_seconds: 60     # 30-3600
  max_replica_lag_seconds: 30
  min_free_storage_pct: 10
  min_storage_runway_hours: 24
  min_available_memory_pct: 5
  aws:
    region: us-east-1                 # standalone, when the host is not an RDS endpoint
    db_instance_identifier: orders
  gcp:
    project: my-project
    instance: main                    # standalone, when connecting by DNS name
  alloydb:                            # standalone; AlloyDB hosts never name the instance
    project: my-project               # empty: the credentials' project
    cluster: orders
    instance: orders-primary
```

Fleet databases name their AlloyDB instance with `cloud_resource.gcp` (`project`, `cluster`,
`instance`) and their Azure flexible server with `cloud_resource.azure` (`subscription`,
`resource_group`, `server`); a standalone Azure server uses the `azure:` section, and a
`<name>.postgres.database.azure.com` host always names the server. Memory totals come from
dated tables: Azure by SKU name, AlloyDB by vCPU count (8 GiB per vCPU for 2-96 vCPUs); a SKU
or shape the table does not list leaves the memory total unknown with a reason in `missing`,
never a guess.

Environment overrides: `SAGE_CLOUD_TELEMETRY_ENABLED`, `SAGE_AWS_REGION`,
`SAGE_AWS_DB_INSTANCE_IDENTIFIER`, `SAGE_AWS_DB_CLUSTER_IDENTIFIER`, `SAGE_GCP_PROJECT`,
`SAGE_GCP_CLOUDSQL_INSTANCE`.

Cost: CloudWatch bills `GetMetricData` per metric requested (about 10 metrics per poll, plus
one per read replica); Performance Insights and Cloud Monitoring API reads have their own
pricing. Raise `poll_interval_seconds` to read less often. The cost is per instance, not per
database: databases of one fleet on the same instance (the same host and port, or the same
`cloud_resource`, with the same polling settings) share one poller, polled once per interval
from the first of them that starts until the last one stops. `GET /api/v1/cloud-telemetry`
lists such an instance once, with its member databases in `databases`.

## Major-version upgrades and blue/green prechecks

`GET /api/v1/upgrade-readiness?target_major=18&method=blue_green` (or the MCP tool
`upgrade_readiness`) runs the checks that stop managed upgrades midway. RDS and Aurora
blue/green deployments replicate logically, so a table with no primary key and no replica
identity fails the prechecks (U1 `block`); fix it with a primary key or
`REPLICA IDENTITY USING INDEX`/`FULL`. A managed in-place major upgrade runs `pg_upgrade`:
from below PostgreSQL 17 it drops logical slots (U3 `block`), and removed settings in the
parameter group stop the new server (U6, read from `pg_settings.source` because providers
hide `pg_file_settings`). U7 compares the source and target majors with the provider's
dated standard-support calendar (`rds` and `aurora` today, each date with its source URL
and the day pg_sage read it); other providers are `unknown`, never `pass`. The detected
`cloud_environment` selects the provider. In fleet mode each member is checked on its
own, and pg_sage opens a daily advisory case for members at or near end of life. pg_sage
never starts an upgrade.

## Provider advice

Managed services ship their own advisors. pg_sage reads them and says, for each piece of
advice, whether it would follow it, so you do not have to reconcile two sets of advice by hand.

| Provider | Source | Credential |
|---|---|---|
| Cloud SQL, AlloyDB | the `google_db_advisor_recommended_indexes` view (index advisor) | the database connection |
| Azure flexible server | index tuning (`az postgres flexible-server index-tuning list-recommendations`) | the ARM credential host telemetry uses (Reader) |
| Supabase | Management API performance advisors (index lints only) | `SAGE_SUPABASE_OBSERVABILITY_TOKEN` |
| Neon | none: Neon has no advisor API | - |

Every hour each database's advice is compared with pg_sage's open index findings
(`missing_index`, `missing_fk_index`, `unused_index`, `duplicate_index`) using the coverage rule
the optimizer and executor use (`CoveredBy`, `Subsumes`). Each recommendation gets a verdict:

- `agree`: pg_sage proposes a change that serves the provider's (for an index, pg_sage's index
  covers every lookup the provider's would; an INCLUDE column pg_sage's index does not carry
  is not agreement).
- `pgsage_only`: pg_sage proposes it; the provider does not. It is listed only when the
  provider gave index advice this run: an empty list or an absent advisor claims nothing.
- `provider_only`: the provider proposes it and pg_sage does not. The reason says whether pg_sage
  would act and why: an index to create goes through pg_sage's own HypoPG what-if on the
  table's hottest statements, and anything it cannot measure (no HypoPG, no workload, an
  existing index already serves it) is "would not act". An index to drop is "would not act"
  until pg_sage's own unused-index window and standby checks find it unused.
- `conflict`: the provider wants to drop an index that backs a constraint, or that pg_sage
  sees scanned on this server.
- `unparsed`: the provider's statement is not exactly one `CREATE INDEX`. It is kept as text
  (`payload.provider_text`) and never offered as SQL.
- `unknown`: advice of a shape pg_sage does not read (an unrecognized view shape or
  recommendation type). It is shown, not parsed.

Provider advice is evidence only. It never raises a finding's confidence, risk tier or
autonomy, and nothing in the executor reads it; anything it leads to is an ordinary pg_sage
candidate through the policy gate. Advice the provider stops giving is marked `resolved_at`
and ages out with `retention.findings_days`. A failed fetch (for example an ARM 403 or a
Supabase 401) is recorded as the source's `last_error` and leaves the earlier advice unchanged.

The view read is one read-only transaction bounded by `statement_timeout = 5s` and
`lock_timeout = 1s`, at most 100 rows; its columns are checked first, and a view without the
`index` column is reported as `unknown`.

### Parameter advice: no provider source

Provider advice is index advice only; there is no `parameter` kind, because no provider
publishes PostgreSQL parameter advice that pg_sage could read (checked 2026-11-13):

| Provider | What its advisors recommend | Parameter advice |
|---|---|---|
| Cloud SQL | Recommender API (`google.cloudsql.instance.*`): idle, over- and underprovisioned (machine size), out of disk, high availability, security (network, SSL, certificates, password policy, `ENABLE_DATABASE_AUDITING`) and performance (`POSTGRES_HIGH_TRANSACTION_ID_UTILIZATION_BEST_PRACTICE`, manual vacuum steps; `POSTGRES_OPTIMIZE_HIGH_MEMORY_QUERIES`, a machine type) | none: the only flag recommendation, `table_open_cache`, is MySQL only |
| AlloyDB | no recommender | none |
| Azure flexible server | index tuning (`CreateIndex`, `DropIndex`); other types are shown as `unknown` | none |
| Supabase | performance advisors (index lints) | none |
| Neon | no advisor API | none |

The database-auditing recommendation says to enable auditing and links to the pgAudit guide;
it carries no flag name and value, so it would be a provider instruction, not a parameter
change pg_sage could reconcile. pg_sage's own parameter advice (`managed_parameter_drift`
findings and the provider proposals above) is unaffected. If a provider starts publishing
PostgreSQL flag recommendations, a `parameter` kind can be added then.

`GET /api/v1/provider-advice?database=<name>` lists each database's advice and its sources'
last fetch (`?include_resolved=true` adds resolved advice). A finding case shows the advice
behind its finding in `provider_advice` (`provider_advice_unavailable` when it could not be
read). In fleet mode each member reads its own provider's advice.

```yaml
provider_advice:
  enabled: true   # default; a source runs only when its provider is detected and its credentials exist
```

## Aurora clone rehearsal

`clone.provider: aurora_clone` rehearses a migration or an index on an Aurora copy-on-write
clone of the monitored Aurora PostgreSQL cluster. A clone shares the source's storage
until either side writes, so it is ready in 10 to 20 minutes (AWS's figure) and is billed
for its one instance and the pages that change, not a full copy.

```yaml
clone:
  provider: aurora_clone
  aurora:
    region: us-east-1          # the monitored cluster's region (required)
    instance_class: ""         # default: the source writer's class (db.serverless for Serverless v2)
    ttl_minutes: 60            # 30-1440: the longest a clone lives; a rehearsal ends by then
```

What pg_sage does for each clone:

- The source is the cluster whose endpoint pg_sage connects to: an Aurora cluster endpoint
  (`<cluster>.cluster-<id>.<region>.rds.amazonaws.com`) or its `cluster-ro-` reader. An
  instance endpoint, a custom endpoint, RDS Proxy or custom DNS is refused with "not an
  Aurora cluster", as is an Aurora MySQL cluster or a cluster in another region.
- It restores cluster `sage-<purpose>-<8 hex>` with `RestoreDBClusterToPointInTime`, restore
  type `copy-on-write`, the latest restorable time, the source's subnet group, security
  groups and cluster parameter group, and deletion protection off. The cluster is tagged
  `pg-sage-receipt=<its name>` and `pg-sage-expires-at=<now + ttl_minutes>`.
- It adds one instance `sage-<purpose>-<8 hex>-1` of `clone.aurora.instance_class` (default:
  the writer's class) with the writer's parameter group and public accessibility, and
  waits until the cluster and the instance are available.
- The clone's DSN is the monitored DSN with the clone's endpoint in place of the source's:
  the same users and passwords (the data is a copy). Its `sslmode` is at least `require`.
  A DSN that sets `hostaddr`, `service` or more than one host is refused.
- Aurora makes at most 15 copy-on-write clones of one source (clones of clones included);
  the 16th would be a full copy at full price, so pg_sage refuses it and names the limit.
- Teardown deletes the instance, waits for it to go, then deletes the cluster with no final
  snapshot and no retained automated backups. Only a `sage-*` cluster tagged with its own
  name and without deletion protection is ever deleted.
- A failure after the cluster exists deletes it before the error returns. A restore whose
  outcome is unknown (a 5xx, a lost reply, or "already exists" when the SDK retried one RDS
  accepted) is looked for by tag and deleted; when it is not listed yet, or a create is cut
  short, the receipt stays `create_uncertain` and the reaper looks for the cluster by its
  tag and destroys it. Aurora has no server-side expiry, so the reaper
  is the backstop: check `aws rds describe-db-clusters` for `sage-*` clusters if a sidecar
  was down for long.

Credentials come from the AWS default chain (environment, shared config, web identity or
an instance role), as for telemetry. Without them nothing is created or deleted and
rehearsals are recommendation-only with reason `clone_unavailable`. The API cannot switch
`clone.provider` to `aurora_clone`; set it in YAML and restart.

The IAM policy the sidecar needs, on top of the telemetry reads. To narrow `Resource`, keep
the deletes on `arn:aws:rds:<region>:<account>:cluster:sage-*` and `...:db:sage-*`; the
restore and the instance create are also authorized against the source cluster, its subnet
group and its parameter groups, so their `Resource` must name those ARNs too, or the create
is refused (403) and the rehearsal is recommendation-only:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PgSageAuroraCloneRead",
      "Effect": "Allow",
      "Action": ["rds:DescribeDBClusters", "rds:DescribeDBInstances"],
      "Resource": "*"
    },
    {
      "Sid": "PgSageAuroraCloneWrite",
      "Effect": "Allow",
      "Action": [
        "rds:RestoreDBClusterToPointInTime",
        "rds:CreateDBInstance",
        "rds:DeleteDBInstance",
        "rds:DeleteDBCluster",
        "rds:AddTagsToResource"
      ],
      "Resource": "*"
    }
  ]
}
```

`RestoreDBClusterToPointInTime` also needs read access to the source cluster, its subnet
group, security groups and parameter groups, and `kms:CreateGrant` / `kms:DescribeKey` on
the source's KMS key when its storage is encrypted with a customer-managed key.

Cost: each receipt carries a cost label of instance-hours for its lifetime, priced only by
a `cost.rates` entry you supply (pg_sage ships no prices). The number that says it works:
Aurora rehearsals that got a clone, their median time to ready, clone instance-hours per
week from the receipts, and clones left after 24 hours (0).

## RDS restore rehearsal

> **Cost warning: read this before the first run.** RDS for PostgreSQL has no
> copy-on-write clone. Every `rds_restore` rehearsal restores a **whole new instance** of
> production's class and storage, billed at the **full instance price** (compute and the
> full allocated storage) from the restore until teardown: up to
> `clone.restore_deadline_minutes` of restore plus up to an hour of rehearsal. A restore
> takes tens of minutes to hours, growing with the volume and the WAL to replay. pg_sage
> creates nothing unless you set `clone.allow_full_copy: true`, logs this cost at every
> startup, and writes each clone's instance-hours on its receipt before RDS is asked for it.

`clone.provider: rds_restore` rehearses a migration or an index on a point-in-time restore of
the monitored RDS for PostgreSQL instance.

```yaml
clone:
  provider: rds_restore
  allow_full_copy: true          # required: without it startup fails
  restore_deadline_minutes: 120  # 30-720: the longest a restore may take to become available
  rds:
    region: us-east-1            # the monitored instance's region (required)
    instance_class: ""           # default: the source instance's class
```

What pg_sage does for each clone:

- The source is the instance whose endpoint pg_sage connects to
  (`<instance>.<id>.<region>.rds.amazonaws.com`). An Aurora endpoint, a Multi-AZ DB cluster
  member, RDS Proxy, custom DNS, another engine or another region is refused with "not an
  RDS for PostgreSQL instance" (Aurora uses `aurora_clone`). A source with automated backups
  off (retention 0) has no restorable time and is refused.
- It restores instance `sage-<purpose>-<8 hex>` with `RestoreDBInstanceToPointInTime` at the
  latest restorable time, in the source's subnet group, security groups, parameter group,
  storage type (and IOPS for `io1`/`io2`) and public accessibility, single-AZ and without
  deletion protection, tagged `pg-sage-receipt=<its name>` and `pg-sage-expires-at`. It waits
  until the instance is available, at most `clone.restore_deadline_minutes`.
- The clone's DSN is the monitored DSN on the restored instance's endpoint (same users and
  passwords; `sslmode` at least `require`). A restored instance that answers on production's
  host is refused and deleted.
- Teardown deletes the instance with no final snapshot and no retained automated backups.
  Only a `sage-*` instance tagged with its own name and without deletion protection is ever
  deleted.
- A restore RDS refused made nothing. A failed instance is deleted before the error
  returns. A restore whose outcome is unknown (a 5xx, a lost reply, "already exists") is
  looked up by name and tag and deleted. A restore not available by the deadline, or not
  described yet, leaves its receipt `create_uncertain`, and the reaper finds the instance
  by name and tag and destroys it. RDS has no server-side expiry, so the reaper is the
  backstop: check `aws rds describe-db-instances` for `sage-*` instances if a sidecar was
  down for long.
- The receipt and the rehearsal bound cover the restore: a rehearsal on a full copy may run
  `restore_deadline_minutes` plus one hour, and its receipt lives 15 minutes longer.
- A caller that ends before `restore_deadline_minutes` is refused before anything is
  created (reason `clone_unavailable`): a stdio MCP call (`mcp.call_timeout_seconds`, at most
  10 minutes) can never wait out a full copy, so `apply_migration` over stdio is
  recommendation-only with `rds_restore`. Use the HTTP transport or the sidecar's own
  rehearsals.

Credentials come from the AWS default chain, as for `aurora_clone`. Without them, or
without the monitored pool, nothing is created and rehearsals are recommendation-only with
reason `clone_unavailable`. The API cannot switch `clone.provider` to `rds_restore`; set it
in YAML and restart.

The IAM policy the sidecar needs, on top of the telemetry reads (narrow the delete to
`arn:aws:rds:<region>:<account>:db:sage-*`; the restore is also authorized against the
source instance, its subnet group and its parameter group):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PgSageRDSRestoreRead",
      "Effect": "Allow",
      "Action": ["rds:DescribeDBInstances"],
      "Resource": "*"
    },
    {
      "Sid": "PgSageRDSRestoreWrite",
      "Effect": "Allow",
      "Action": [
        "rds:RestoreDBInstanceToPointInTime",
        "rds:DeleteDBInstance",
        "rds:AddTagsToResource"
      ],
      "Resource": "*"
    }
  ]
}
```

A source encrypted with a customer-managed KMS key also needs `kms:CreateGrant` and
`kms:DescribeKey` on that key.

Cost: each receipt carries the instance-hours of one instance of the restore's class for the
receipt's lifetime (`restore_deadline_minutes` plus 75 minutes), priced only by a
`cost.rates` entry you supply for the source (pg_sage ships no prices; storage is not
priced). The number that says it works: restores that reach ready within their estimate,
their instance-hours, and instances left after 24 hours (0).

## Cloud SQL clone rehearsal

> **Cost warning: read this before the first run.** Cloud SQL has no copy-on-write clone.
> Every `cloudsql_clone` rehearsal clones production into a **whole new instance** of its
> tier and disk, billed at the **full instance price** (vCPUs, memory and the full disk) from
> the clone until teardown: up to `clone.restore_deadline_minutes` of cloning plus up to an
> hour of rehearsal. A clone takes minutes to hours, growing with the disk. pg_sage creates
> nothing unless you set `clone.allow_full_copy: true`, logs this cost at every startup, and
> writes each clone's instance-hours on its receipt before Cloud SQL is asked for it.

`clone.provider: cloudsql_clone` rehearses a migration or an index on a clone
(`instances.clone`) of the monitored Cloud SQL for PostgreSQL instance, in the same project.

```yaml
clone:
  provider: cloudsql_clone
  allow_full_copy: true          # required: without it startup fails
  restore_deadline_minutes: 120  # 30-720: the longest a clone may take to be ready
  cloudsql:
    project: acme-prod-1234      # the monitored instance's project (required)
    instance: shop               # its instance id (required)
```

What pg_sage does for each clone:

- The source is `clone.cloudsql.instance`. It must be a `RUNNABLE` PostgreSQL primary (not
  a read replica), and pg_sage must connect to one of its IPs, private or public. A DSN
  through the Cloud SQL Auth Proxy, a Unix socket or a DNS name is refused with "not the
  configured Cloud SQL for PostgreSQL instance", because pg_sage could not tell that it
  monitors that instance, nor reach the clone the same way.
- It asks for clone `sage-<purpose>-<8 hex>` (lowercase, at most 63 characters, the label
  limit) and polls the operation. When it is done, pg_sage labels the clone
  `pg-sage-receipt=<its name>` and `pg-sage-expires-at=<Unix seconds>`, keeps the source's
  other labels, lifts the deletion protection the clone copied from production and makes it
  zonal (no high-availability standby is billed). All of this happens within
  `clone.restore_deadline_minutes`.
- The clone's DSN is the monitored DSN on the clone's IP of the same kind (private for
  private, public for public): the same users and passwords, `sslmode` at least `require`.
  `verify-ca` and `verify-full` are kept, so they need the clone's server CA. A clone that
  answers on one of production's IPs is refused and deleted.
- Teardown deletes the instance. Only a `sage-*` instance labelled with its own name, or not
  labelled at all, is deleted. An unlabelled one is a clone whose labelling never finished:
  its name is the receipt's random one, so its copied deletion protection is lifted first.
  An instance labelled for another receipt, or protected again after pg_sage labelled it, is
  never deleted.
- A clone Google refused (4xx, including a name that already exists) made nothing. A failed
  clone operation or a failed labelling deletes the clone before the error returns. A lost
  reply or a 5xx is looked up by name and deleted. A clone not done by the deadline leaves
  its receipt `create_uncertain`, and the reaper finds the instance by name and destroys it.
  Cloud SQL has no server-side expiry, so the reaper is the backstop: check
  `gcloud sql instances list --filter='name:sage-*'` if a sidecar was down for long.
- The receipt and the rehearsal bound cover the clone: a rehearsal on it may run
  `restore_deadline_minutes` plus one hour, and its receipt lives 15 minutes longer.
- A caller that ends before `restore_deadline_minutes` is refused before the clone is asked
  for (reason `clone_unavailable`), so `apply_migration` over stdio MCP is
  recommendation-only with `cloudsql_clone`, as with `rds_restore`.
- Cloud SQL does not let you reuse an instance name for up to a week after deletion. Every
  receipt has a fresh random name, so this never blocks a rehearsal.

Credentials come from Google Application Default Credentials, the same chain as Cloud SQL
telemetry (`GOOGLE_APPLICATION_CREDENTIALS`, the gcloud ADC file, then the metadata server).
Without them, or without the monitored pool, nothing is created and rehearsals are
recommendation-only with reason `clone_unavailable`. A refused call names the permission it
needs. The API cannot switch `clone.provider` to `cloudsql_clone`; set it in YAML and
restart.

The permissions the sidecar's identity needs, on top of the telemetry reads (a custom role on
the project; `roles/cloudsql.admin` also covers them):

| Permission | Used for |
| --- | --- |
| `cloudsql.instances.get` | Read the source and the clone, and poll operations |
| `cloudsql.instances.clone` | Clone the source |
| `cloudsql.instances.update` | Label the clone, and lift its copied deletion protection and HA |
| `cloudsql.instances.delete` | Delete the clone |

An IAM condition on `resource.name` starting with
`projects/<project>/instances/sage-` can narrow `update` and `delete` to pg_sage's clones.

Cost: each receipt carries the instance-hours of one instance for the receipt's lifetime
(`restore_deadline_minutes` plus 75 minutes), priced only by a `cost.rates` entry you supply
for the source (pg_sage ships no prices; the disk is not priced). The number that says it
works: clones that are ready within their estimate, their instance-hours, and instances left
after 24 hours (0).

The live check (`SAGE_CLOUDSQL_LIVE=1` with `SAGE_CLOUDSQL_LIVE_DATABASE_URL`,
`SAGE_CLOUDSQL_PROJECT`, `SAGE_CLOUDSQL_INSTANCE` and `SAGE_TEST_DATABASE_URL`) runs the
clone contract on a real, funded instance; it is owner-run and never in CI.

## AlloyDB restore rehearsal

> **Cost warning: read this before the first run.** AlloyDB has no copy-on-write clone.
> Every `alloydb_restore` rehearsal restores production into a **whole new cluster** with
> its own primary instance of the source primary's machine size, billed at the **full
> price** (vCPUs, memory and the restored storage) from the restore until teardown: up to
> `clone.restore_deadline_minutes` of restoring plus up to an hour of rehearsal. A restore
> takes minutes to hours, growing with the data. pg_sage creates nothing unless you set
> `clone.allow_full_copy: true`, logs this cost at every startup, and writes each clone's
> instance-hours on its receipt before AlloyDB is asked for it.

`clone.provider: alloydb_restore` rehearses a migration or an index on a point-in-time
restore (`clusters:restore` from continuous backup) of the monitored AlloyDB cluster, in the
same project and region.

```yaml
clone:
  provider: alloydb_restore
  allow_full_copy: true          # required: without it startup fails
  restore_deadline_minutes: 120  # 30-720: the longest a restore may take to be ready
  alloydb:
    project: acme-prod-1234      # the monitored cluster's project (required)
    location: us-central1        # its region, not a zone (required)
    cluster: shop                # its cluster id (required)
```

What pg_sage does for each clone:

- The source is `clone.alloydb.cluster`. It must be a `READY` PostgreSQL primary cluster
  (not a secondary) with **continuous backup on**: a cluster with
  `continuousBackupConfig.enabled` false is refused with that setting named (turn it on with
  `gcloud alloydb clusters update <cluster> --region <region> --enable-continuous-backup`).
  pg_sage must connect to the private or public IP of the cluster's `READY` primary
  instance. A read pool, the AlloyDB Auth Proxy, a Unix socket or a DNS name is refused with
  "not the configured AlloyDB primary cluster".
- The point in time is five minutes ago (`AlloyDBRestoreLag`), so continuous backup already
  holds it. One before the cluster's earliest restorable time (continuous backup was turned
  on recently) is refused with reason `clone_unavailable`.
- It restores cluster `sage-<purpose>-<8 hex>` (lowercase, at most 63 characters) into the
  source's network (`networkConfig`, or `network`) under the source's encryption key
  (`encryptionConfig`, so a CMEK cluster stays CMEK), labelled `pg-sage-receipt=<its name>`
  and `pg-sage-expires-at=<Unix seconds>` as it is created, with the source's other labels
  kept, and with automated backups off (`automatedBackupPolicy.enabled: false`), so no
  backup of the clone, a copy of production, outlives it. When the restore is done, pg_sage adds a zonal primary instance (no
  high-availability standby is billed) with the source primary's `machineConfig` and
  `networkConfig` (public IP and authorized networks). All of this happens within
  `clone.restore_deadline_minutes`.
- The clone's DSN is the monitored DSN on the new primary's IP of the same kind (private for
  private, public for public): the same users and passwords, `sslmode` at least `require`.
  A clone that answers on one of production's IPs is refused and deleted.
- Teardown deletes the cluster with `force`, which deletes its instances too. Only a
  `sage-*` cluster labelled with its own name is deleted. The restore labels the cluster as
  it creates it, so an unlabelled cluster is never pg_sage's and is never deleted: if
  AlloyDB ever restores one without the labels, pg_sage reports it left behind and names the
  `gcloud alloydb clusters delete` command to run by hand. The reaper never closes the
  receipt of such a cluster as gone: its lookup reports the cluster left behind, so the
  receipt stays `create_uncertain` until the cluster is deleted.
- A restore Google refused (4xx, including an id that already exists) made nothing. A
  failed restore operation, an instance create that fails after the cluster exists, a failed
  instance operation or a primary that is not `READY` deletes the cluster before the error
  returns. A lost reply or a 5xx is looked up by name and deleted. A restore or instance not
  done by the deadline leaves its receipt `create_uncertain`, and the reaper finds the
  cluster by name and destroys it. AlloyDB has no server-side expiry, so the reaper is the
  backstop: check `gcloud alloydb clusters list --region <region>` for `sage-*` clusters if
  a sidecar was down for long.
- The receipt and the rehearsal bound cover the clone: a rehearsal on it may run
  `restore_deadline_minutes` plus one hour, and its receipt lives 15 minutes longer. Receipts
  are scoped `gcp/<project>/<region>`.
- A caller that ends before `restore_deadline_minutes` is refused before the restore is
  asked for (reason `clone_unavailable`), so `apply_migration` over stdio MCP is
  recommendation-only with `alloydb_restore`, as with `rds_restore` and `cloudsql_clone`.

Credentials come from Google Application Default Credentials, the same chain as Cloud SQL
and AlloyDB telemetry (`GOOGLE_APPLICATION_CREDENTIALS`, the gcloud ADC file, then the
metadata server). Without them, or without the monitored pool, nothing is created and
rehearsals are recommendation-only with reason `clone_unavailable`. A refused call names the
permission it needs. The API cannot switch `clone.provider` to `alloydb_restore`; set it in
YAML and restart.

The permissions the sidecar's identity needs, on top of the telemetry reads (a custom role on
the project; `roles/alloydb.admin` also covers them):

| Permission | Used for |
| --- | --- |
| `alloydb.clusters.get` | Read the source and the restored cluster |
| `alloydb.instances.list`, `alloydb.instances.get` | Find the source primary, read the new one |
| `alloydb.operations.get` | Poll the restore and instance operations |
| `alloydb.clusters.restore` | Restore the source's continuous backup to a new cluster |
| `alloydb.instances.create` | Add the restored cluster's primary instance |
| `alloydb.clusters.delete` | Delete the restored cluster and its instances |

An IAM condition on `resource.name` starting with
`projects/<project>/locations/<region>/clusters/sage-` can narrow `instances.create` and
`clusters.delete` to pg_sage's clones. The live check confirms the permission names, that
the restore applies the requested labels and accepts the automated backup policy.

Cost: each receipt carries the instance-hours of one primary instance for the receipt's
lifetime (`restore_deadline_minutes` plus 75 minutes), priced only by a `cost.rates` entry
you supply for the source (pg_sage ships no prices; storage is not priced). The number that
says it works: restores that are ready within their estimate, their instance-hours, and
clusters left after 24 hours (0).

The live check (`SAGE_ALLOYDB_LIVE=1` with `SAGE_ALLOYDB_LIVE_DATABASE_URL`,
`SAGE_ALLOYDB_PROJECT`, `SAGE_ALLOYDB_LOCATION`, `SAGE_ALLOYDB_CLUSTER` and
`SAGE_TEST_DATABASE_URL`) runs the clone contract on a real, funded cluster; it is
owner-run and never in CI.

## Azure restore rehearsal

`clone.provider: azure_restore` rehearses on a point-in-time restore of the monitored Azure
flexible server to a new server in the same region and major version: a full copy at the full
server price, which needs `clone.allow_full_copy: true`. See
[Azure: restore rehearsal](azure.md#restore-rehearsal) for the cost warning, the flow and the
RBAC actions.

## Restore drills

pg_sage checks that a database's latest backup actually restores (drill-01a). Every hour the
fleet leader starts the drills that are due on `clone.provider`, at most
`drill.max_concurrent_per_fleet` (2) at once and one per database, on top of
`clone.max_creates_per_hour` and the provider's backoff. Each database's cadence, kinds,
budget and marker query are in its standing policy document's
[drill block](policy-gate.md#restore-drills).

A drill:

1. Writes the clone's receipt (purpose `drill`, expiring an hour after the drill budget) and
   logs its cost label first (instance-hours; an amount only with an operator `cost.rates`
   entry, never a price table).
2. Asks the provider for a clone at the latest restorable point: a branch (Neon, Lakebase),
   an Aurora clone, a Database Lab thin clone, or a full restore (RDS, Cloud SQL, AlloyDB,
   Azure).
3. Connects only after the identity guard shows the clone is not the source
   (`target_is_production` otherwise).
4. Runs its checks read-only in one `READ ONLY` transaction with a statement timeout: the
   source's 20 largest tables exist with the same columns, row estimates within 5%, exact
   counts of the 3 smallest within 5%, a primary-key sample checksum, `SELECT 1` on each, and
   the newest committed marker. The only statements sent to the source are the same
   read-only, bounded catalog, count and sample queries.
5. Destroys the clone, always, even when a check failed or the drill timed out. A teardown
   that fails leaves the receipt `destroying` for the clone reaper, which also destroys a
   drill clone a killed sidecar left once its receipt expires.
6. Records the result in `sage.restore_drills` (kept 400 days): measured RTO (clone ready and
   checks done, from the request) and RPO (the restore point minus the newest marker on the
   clone; unknown, with the reason, when no marker exists).

| Platform | Drill |
|---|---|
| Neon, Lakebase, Aurora, Database Lab | On by default (cheap: branch, copy-on-write or thin clone) |
| RDS, Cloud SQL, AlloyDB, Azure | Opt-in: `clone.allow_full_copy: true` and `full_copy` in `drill.kinds`; a full instance bill for the drill |
| Self-managed, no clone provider | No drill: the info finding `recoverability_unknown` says what to configure |

A failed or errored drill opens the critical finding
[`restore_drill_failed`](findings.md#restore_drill_failed), which pages through the finding
alerts and resolves on the next pass. A drill held back by the create limit or the provider's
backoff is retried on the next pass and records nothing.

### Recoverability: the panel, the API and `restore_drills`

Each database is in one status: `passed` (its newest passed drill finished at most 35 days
ago; a pass exactly 35 days old counts), `failed` (no pass in the window and the newest drill
failed), `stale` (the newest drill passed, more than 35 days ago), `never_drilled` (a provider
can drill it, no drill has run) or `unknown` (no drill can run: `clone.provider` is none or no
kind is allowed, as `recoverability_unknown` says, or the history could not be read). A failed
drill after a pass in the window does not reset the pass, as D7 reads it; the panel still shows
the failure.

- The Database page's **Recoverability** panel shows the last drill (when, pass or fail, kind
  and provider), RPO and RTO in plain units (`RPO unknown` with the reason when no marker
  exists), whether backups survive a delete of the database (the [backup
  isolation](#what-the-telemetry-is-used-for) evidence, unknown without a cloud source), the cost label of
  the next full-copy drill, and, when unknown, what to configure and grant. The fleet overview
  shows one line: "7 of 9 databases passed a restore drill in the last 35 days".
- `GET /api/v1/databases/{db}/restore-drills?limit=20` (viewer and up) is that database's
  newest drills, newest first (`limit` 1 to 100, default 20; anything else is `400`), with
  `status`, `reason`, `advice`, `latest`, `last_pass`, `isolation` and
  `next_full_copy_cost_label`. An unknown database, or one your grants do not cover, is `404`;
  one not connected is `503`; an unreadable history is `500` naming the database.
- `GET /api/v1/fleet/recoverability` (viewer and up) counts the databases you may read by
  status, with `median_rto_seconds` over the passed databases' newest passes. A database
  that could not be read counts as `unknown` and is named in `errors`; it never counts as
  passed.
- The MCP tool [`restore_drills`](mcp.md#restore_drills) answers the same count, with each
  database's newest drill and newest pass, for the databases the token may read (or the one
  named). It only reads; no API route or tool starts a drill.

Drill rows are read where the drill pass records them: the governance control database when
one is set (meta mode, or `agents.control_database`), else the database's own `sage` schema.

## Live check (owner-run, never in CI)

CI uses httptest fakes only. To check against a real, disposable instance (read-only calls):

```
# AWS (credentials from your default chain)
SAGE_TEST_AWS_REGION=us-east-1 SAGE_TEST_RDS_INSTANCE=my-test-db \
  go test -tags cloudlive -count=1 -run TestLiveAWSTelemetry -v ./internal/cloudtel/
# Aurora: SAGE_TEST_RDS_IS_CLUSTER=1 with the cluster identifier

# Google Cloud (Application Default Credentials)
SAGE_TEST_GCP_PROJECT=my-project SAGE_TEST_CLOUDSQL_INSTANCE=my-test-instance \
  go test -tags cloudlive -count=1 -run TestLiveGCPTelemetry -v ./internal/cloudtel/

# Azure flexible server (az login; scripts/azure/provision-test-server.sh writes these)
SAGE_AZURE_SUBSCRIPTION_ID=... SAGE_AZURE_RESOURCE_GROUP=pg-sage-test SAGE_AZURE_SERVER_NAME=... \
  go test -tags cloudlive -count=1 -run TestLiveAzureTelemetry -v ./internal/cloudtel/

# AlloyDB (Application Default Credentials with roles/monitoring.viewer)
SAGE_TEST_GCP_PROJECT=my-project SAGE_TEST_ALLOYDB_CLUSTER=c SAGE_TEST_ALLOYDB_INSTANCE=i \
  go test -tags cloudlive -count=1 -run TestLiveAlloyDBTelemetry -v ./internal/cloudtel/
```

Each prints `CHECK-CLOUD-0n: PASS` lines with the sample and the parameter group or flags.
The Azure and AlloyDB checks skip, naming the variables, until a disposable resource exists.

Cloud SQL and AlloyDB identity and readiness run against the database itself, connected as the
`sage_agent` role through the Auth Proxy or a private IP. The session is read-only with statement
and lock timeouts; nothing on the instance changes:

```
SAGE_TEST_CLOUDSQL_DATABASE_URL=postgres://sage_agent@127.0.0.1:5432/postgres \
SAGE_TEST_ALLOYDB_DATABASE_URL=postgres://sage_agent@127.0.0.1:5433/postgres \
  go test -tags cloudlive -count=1 -run TestLiveGoogleManagedIdentityAndReadiness -v \
  ./cmd/pg_sage_sidecar/
```

`CHECK-GCP-01` passes only when the product's provider probes name the expected service, so a
DSN that reaches the wrong server fails. `CHECK-GCP-02` collects readiness without a standing
policy and passes only when no action family would execute and every family the provider adapter
refuses is unsupported. A subtest whose variable is unset is skipped and says it was not verified.
CI compiles this root (and every other tagged suite) but never runs it.
