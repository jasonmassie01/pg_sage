# Managed clouds: RDS, Aurora and Cloud SQL

pg_sage reads the host telemetry that PostgreSQL cannot see on a managed service, and
turns setting changes that SQL cannot make into proposals with the exact provider command.

| Provider | Telemetry | Settings |
| --- | --- | --- |
| Amazon RDS for PostgreSQL | CloudWatch (CPU, freeable memory, free storage, IOPS, throughput, connections, replica lag), Performance Insights (DB load by wait type, OS memory total) | DB parameter group |
| Amazon Aurora PostgreSQL | CloudWatch (as RDS; `AuroraReplicaLagMaximum`), Performance Insights | DB parameter group of the writer |
| Google Cloud SQL for PostgreSQL | Cloud Monitoring (CPU, memory quota/usage, disk quota/usage, disk ops, backends, replica lag) | Database flags |

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
  --parameters "ParameterName=shared_buffers,ParameterValue=524288,ApplyMethod=pending-reboot"
aws rds reboot-db-instance --region us-east-1 --db-instance-identifier orders
```

pg_sage never applies these changes in this release. Approve one when you will run it; pg_sage
marks it applied once PostgreSQL runs the new value. A default parameter group (`default.*`)
cannot be modified, and a Cloud SQL `--database-flags` command replaces the whole flag list:
the proposal says so and carries the current flags.

## Identity and credentials

The resource is named by the connection host: an RDS instance endpoint, an Aurora cluster
(writer) endpoint, or a Cloud SQL IP address (matched through the Admin API). Reader endpoints,
RDS Proxy and custom DNS are not resolved; in standalone mode set
`cloud_telemetry.aws.db_instance_identifier` (or `db_cluster_identifier`) and `region`, or
`cloud_telemetry.gcp.instance`. Fleet databases are always identified from their hosts.

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
