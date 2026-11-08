# Managed clouds: RDS, Aurora and Cloud SQL

pg_sage reads the host telemetry that PostgreSQL cannot see on a managed service, and
turns setting changes that SQL cannot make into proposals with the exact provider command.

| Provider | Telemetry | Settings |
| --- | --- | --- |
| Amazon RDS for PostgreSQL | CloudWatch (CPU, freeable memory, free storage, IOPS, throughput, connections, replica lag), Performance Insights (DB load by wait type, OS memory total) | DB parameter group |
| Amazon Aurora PostgreSQL | CloudWatch (as RDS; `AuroraReplicaLagMaximum`), Performance Insights | DB parameter group of the writer |
| Google Cloud SQL for PostgreSQL | Cloud Monitoring (CPU, memory quota/usage, disk quota/usage, disk ops, backends, replica lag) | Database flags |

## Provider detection

pg_sage names the provider of each database from its host name or, failing that, from SQL
probes (`aurora_version()` and the `rds.*`, `alloydb.*`, `cloudsql.*` and `azure.*`
settings). Each probe has its own 2 second timeout. "Function does not exist" or an unset
setting means "not this provider"; no match means `self-managed`.

A probe that fails (a timeout, a dropped connection, a permission error) makes the provider
`unknown`, never `self-managed`. At startup the probes retry after 1, 2 and 4 seconds. An
`unknown` provider matches no action's provider list, so provider-gated actions such as
`ALTER SYSTEM` stay blocked, and readiness shows `provider unknown`. The runtime then
re-probes with backoff from 1 minute to 30 minutes. When a probe succeeds, pg_sage logs the
change and rebuilds that database's runtime (YAML fleet and meta-db databases). In
standalone mode, and for the control database of a YAML fleet, it cannot rebuild one
runtime, so readiness shows `provider re-probe found <provider>; restart pg_sage to apply`
until you restart.

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
- **Parameter drift.** Values set in the parameter group or flags that PostgreSQL does not run
  (pending a reboot, overridden by a database/role setting, or not in effect) open
  `managed_parameter_drift` findings.

## Managed proposals

A restart-required or provider-restricted setting (for example `shared_buffers`,
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

Upgrading from v2.5.1 or earlier recomputes the stored identities once; open proposals keep
their ids, status and decisions. Duplicates left by earlier outages (the same change open
twice under different targets) collapse to one, keeping the approved one, else the newest.

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
(writer) endpoint, or a Cloud SQL IP address (matched through the Admin API). Reader endpoints,
RDS Proxy and custom DNS are not resolved; in standalone mode set
`cloud_telemetry.aws.db_instance_identifier` (or `db_cluster_identifier`) and `region`, or
`cloud_telemetry.gcp.instance`. Fleet databases are always identified from their hosts.

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
  `rds:DescribeDBInstances`, `rds:DescribeDBClusters`, `rds:DescribeDBParameters`.
- **Google Cloud:** Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS` with a
  service-account key or user credentials, `gcloud auth application-default login`, or the
  metadata server on GCE/GKE/Cloud Run). Roles: `roles/monitoring.viewer`,
  `roles/cloudsql.viewer`. The project comes from `cloud_telemetry.gcp.project` or the
  credentials.

Without credentials telemetry reports `unavailable: <reason>` (see
`GET /api/v1/cloud-telemetry`) and pg_sage behaves as before.

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
```

Environment overrides: `SAGE_CLOUD_TELEMETRY_ENABLED`, `SAGE_AWS_REGION`,
`SAGE_AWS_DB_INSTANCE_IDENTIFIER`, `SAGE_AWS_DB_CLUSTER_IDENTIFIER`, `SAGE_GCP_PROJECT`,
`SAGE_GCP_CLOUDSQL_INSTANCE`.

Cost: CloudWatch bills `GetMetricData` per metric requested (about 10 metrics per poll, plus
one per read replica); Performance Insights and Cloud Monitoring API reads have their own
pricing. Raise `poll_interval_seconds` to read less often.

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
```

Each prints `CHECK-CLOUD-0n: PASS` lines with the sample and the parameter group or flags.
