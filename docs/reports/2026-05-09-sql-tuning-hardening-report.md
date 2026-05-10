# SQL Tuning Hardening Report

Date: 2026-05-09

## Scope

Reviewed pg_sage-owned SQL and schema paths for large PostgreSQL fleets:

- Runtime schema/bootstrap DDL and Agent DB schema DDL.
- Snapshot history, Cases, action queue, notification, auth/session, and Agent DB store query paths.
- Static review reports from parallel explorers:
  - `C:\Users\jmass\pg_sage\tasks\sql_callsite_review.md`
  - `C:\Users\jmass\pg_sage\tasks\sql_schema_index_review.md`
  - `C:\Users\jmass\pg_sage\tasks\sql_tuning_test_plan.md`

## Changes Made

- Capped snapshot history responses at 500 newest points, returned in chronological order, so long date ranges cannot stream every raw JSON snapshot into the API/UI.
- Added fleet-scale operational indexes to fresh bootstrap and existing-schema migrations:
  - `idx_sessions_expires`
  - `idx_sessions_user`
  - `idx_notification_rules_event_enabled`
  - `idx_notification_log_sent_at`
  - `idx_notification_log_channel_sent`
  - `idx_action_queue_database_pending`
  - `idx_action_queue_expiry`
  - `idx_action_log_outcome_time`
- Added Agent DB per-deployment/time indexes for high-volume child tables:
  - `idx_agent_db_deployments_lease_expiry`
  - `idx_agent_db_pings_deployment_time`
  - `idx_agent_db_cost_samples_deployment_time`
  - `idx_agent_db_backups_deployment_time`
  - `idx_agent_db_tuning_hints_deployment_order`
  - `idx_agent_db_audit_deployment_time`
- Changed `/api/v1/cases` action timeline enrichment to batch queued actions and action logs per database instead of running two per-finding queries.
- Added regression tests for snapshot payload caps and schema/index contracts.

## Verification

CHECK-01: PASS - Focused red/green tests for snapshot cap, bootstrap indexes, and Agent DB indexes passed.

CHECK-02: PASS - `go test -count=1 ./internal/store ./internal/api ./internal/schema ./internal/agentdb` passed.

CHECK-03: PASS - Full Go coverage passed on isolated PostgreSQL 17 with `pg_stat_statements` preloaded:

```powershell
cd C:\Users\jmass\pg_sage\sidecar
$env:SAGE_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55555/postgres?sslmode=disable'
go test -cover -count=1 -p 1 ./...
```

Log: `C:\Users\jmass\pg_sage\test-output\sql-tuning-go-cover-rerun.log`

CHECK-04: PASS - Skip audit found no `SKIP`, `TODO`, or `PENDING` markers in the successful full Go log.

CHECK-05: MANUAL/BLOCKED - Browser smoke against the live 8085 sidecar was blocked by auth rate limiting after an initial run used default test credentials. Logs:

- `C:\Users\jmass\pg_sage\test-output\sql-tuning-playwright-cases.log`
- `C:\Users\jmass\pg_sage\test-output\sql-tuning-playwright-dashboard-local-creds.log`

## Coverage Notes

Business packages in the successful full Go run met the 70% floor:

- `internal/api`: 70.2%
- `internal/agentdb`: 75.0%
- `internal/analyzer`: 84.9%
- `internal/forecaster`: 87.1%
- `internal/schema`: 79.7%
- `internal/store`: 75.6%
- `internal/tuner`: 86.0%

Below-threshold generated/command surfaces remain:

- `cmd/create_admin`: 0.0%
- `cmd/create_admin_alloydb`: 0.0%
- `cmd/pg_sage_sidecar`: 3.4%
- `cmd/reset_admin_for_test`: 0.0%
- `sidecar/web/node_modules/flatted/golang/pkg/flatted`: 0.0%

## Remaining Fleet-Scale Work

- Design a migration-safe fleet-scope key for centralized `sage.findings` and `sage.query_hints` before changing dedupe uniqueness. The static review found current per-database stores are okay, but centralizing those tables would need `database_name` in uniqueness keys.
- Bring extension SQL files into parity with runtime bootstrap DDL. The runtime sidecar schema is richer than `sql/pg_sage--0.5.0.sql`.
- Replace repeated `jsonb_array_elements` forecast aggregation with collector-time rollups or normalized daily aggregate tables.
- Add first-class pagination contracts for Agent DB list endpoints and fleet aggregation endpoints beyond Cases.
- Add retention/partitioning strategy for append-only high-volume tables such as snapshots, health history, size history, and Agent DB pings.
