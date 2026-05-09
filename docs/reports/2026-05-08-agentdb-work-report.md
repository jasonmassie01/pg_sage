# pg_sage Agent DB Work Report

Date: Friday, May 8, 2026, America/Chicago

## Local Sidecar

The local sidecar is running from:

```text
C:\Users\jmass\pg_sage\sidecar\pg_sage_verify_sidecar.exe
```

Manual verification:

```text
URL:      http://127.0.0.1:8080
Email:    admin@pg-sage.local
Password: CodexVerify123!
Process:  pg_sage_verify_sidecar.exe
PID:      25868
```

API smoke after resetting/creating the local test admin:

```text
POST /api/v1/auth/login              200
GET  /api/v1/agent-dbs               200
GET  /api/v1/agent-dbs/providers     200
```

The Docker walkthrough fixture remains the preferred deterministic full-surface
fixture, but Docker Desktop's Linux engine was unavailable during today's smoke.
The no-Docker local sidecar path was used instead.

## Commits Created Today

```text
dba0a96 docs(roadmap): add pg_sage research and agentdb plans
d13fa55 feat(advisor): add agent workload tuning hints
b75f473 feat(agentdb): add deployment control plane api
59c972a feat(agentdb): add deployment dashboard
```

## Product And Research Work

- Reviewed the pg_sage repo and previous roadmap material before adding Agent
  DB implementation slices.
- Added roadmap/spec/plan artifacts for vector, JSONB, PostGIS, extension
  tuning, agent-created databases, Agent DB provisioning, lifecycle, backup,
  recommendation, audit, promotion, identity, and ping-token slices.
- Kept HNSW experiment planning out of this repo slice, per product direction.
- Preserved vector, PostGIS, JSONB, and extension tuning as advisory/hint
  surfaces instead of autonomous rebuild products.

## Advisor And Tuning Work

- Added JSONB workload classification and prompt hints for containment,
  existence, scalar extraction, JSON path, sort, and grouping patterns.
- Added vector workload classification and prompt hints for bounded ANN query
  shapes without taking ownership of HNSW experiment tuning.
- Added agent workload guardrails for attributed, unattributed, and ephemeral
  agent workloads.
- Added an advisor degraded-mode finding when the config advisor is enabled but
  LLM support is disabled.
- Normalized optimizer action metadata so recommendations can carry action level
  and action risk more explicitly.

## Agent DB Control Plane

Implemented `sidecar/internal/agentdb` as a first-class control-plane package:

- Agent DB requests with idempotency and policy decisions.
- Local Postgres schema and database provisioning.
- Cloud provider instance planning for AWS RDS, GCP Cloud SQL, and Databricks
  Lakebase.
- Custom size profiles for local and cloud provider deployments.
- Cost samples, budget state, and budget actions.
- Backup records, backup assurance checks, and restore-drill dry-runs.
- Lease pings, archive flow, expired cleanup, and reconcile.
- Recommendation publishing and feedback for agent-consumable query tuning.
- Audit event listing and JSONL export.
- Promotion deploy requests with review-only approvals.
- Agent identity records and deployment-bound ping tokens.

## Security And Enterprise Hardening

- Ping tokens are bearer secrets returned once; stored values are hashed.
- Token list responses are redacted.
- Tokens can be rotated and revoked.
- Failed token validation writes an audit event with only a hash prefix.
- Repeated failed token validation is rate-limited and returns HTTP 429.
- Agent ping bypasses session auth only on the token-backed endpoint.
- Enterprise policy gates now cover missing tenant/agent identity, sensitive
  data classification, masking-policy requirements, allowed-region checks,
  cloud budget review, and approval SLA reasons.

## API And UI Work

- Registered Agent DB routes under `/api/v1/agent-dbs`.
- Added the Agent DB dashboard route at `#/agent-dbs`.
- Added UI panels for deployments, requests, provider readiness, custom size
  profiles, cost, tuning hints, backups, cloud dry-run attempts, audit events,
  recommendations, promotion requests, cleanup, and reconcile.
- Added Playwright fixtures and browser coverage for the Agent DB page.
- Rebuilt embedded static assets under `sidecar/internal/api/dist`.

## Lakebase Support Sweep

Current codebase state:

- Lakebase provider key: `databricks_lakebase`.
- Provider readiness checks for the `databricks` CLI.
- Size profile support includes `lakebase_instance_s`.
- Cloud provider validation restricts Lakebase to instance-level provisioning.
- Provision plans produce reviewed Databricks CLI commands for branch or
  provisioned-instance creation.
- Lifecycle status, destroy dry-run, backup check, and restore-drill dry-run use
  Databricks database CLI commands.
- UI and e2e tests expose Lakebase as a provider and allow custom Lakebase size
  profile creation.

Added today after the sweep:

- Lakebase provision plans now include user-visible notes warning that the
  extension allowlist must be verified before workloads depend on pgvector,
  PostGIS, pg_hint_plan, or pg_stat_statements.
- Lakebase provision plans now warn that Lakebase is managed Postgres and that
  pg_sage should prefer session, database, or role-level parameter settings
  instead of instance-level GUC changes.
- `docs/agent-db-deployments.md` now has a Lakebase section covering current
  support, extension assumptions, managed-service limitations, and tomorrow's
  verification scope.

Databricks references checked:

- https://docs.databricks.com/aws/en/oltp/projects/extensions
- https://docs.databricks.com/aws/en/oltp/projects/compatibility
- https://docs.databricks.com/aws/en/oltp/instances/query/postgres-compatibility

Important Lakebase assumptions to verify tomorrow:

- Confirm whether the target workspace uses Lakebase Autoscaling or Provisioned.
- Confirm the Databricks CLI command group and arguments for the workspace.
- Query `pg_available_extensions` and `pg_extension` on a real Lakebase database.
- Test `vector`, `postgis`, `pg_stat_statements`, `pg_hint_plan`, and any
  extension-specific GUCs the tuning hints mention.
- Confirm which parameters can be set at session, database, and role level.
- Confirm backup/restore metadata exposed through the Databricks API or CLI.
- Confirm whether branch delete/status commands need project, branch, catalog,
  or workspace identifiers beyond the current dry-run resource name.

## Verification Evidence

Full verification from the Agent DB tranche:

```text
go test -cover -count=1 -p 1 ./...
SAGE_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./... -tags=integration -p 1 -count=1 -timeout 600s
go test -tags=e2e -count=1 -timeout 180s ./e2e
npm test -- --run
npm run lint
npm run build
npm run test:e2e
git diff --check
```

Results:

```text
Go coverage sweep: 34 packages passed, 0 failed, 0 skipped
Tagged integration sweep: passed, 0 failed, 0 skipped
Go e2e: passed, 0 failed, 0 skipped
Vitest: 30 passed, 0 failed
Playwright: 48 passed, 0 failed
Lint/build/diff-check: passed
```

Focused Lakebase follow-up verification:

```text
go test -count=1 ./internal/agentdb -run TestBuildProvisionPlanForManagedProviders
```

Result:

```text
PASS
```

## Files To Start From Tomorrow

- `docs/agent-db-deployments.md`
- `sidecar/internal/agentdb/providers.go`
- `sidecar/internal/agentdb/providers_test.go`
- `sidecar/internal/agentdb/tuning.go`
- `sidecar/internal/api/agent_db_provider_handlers.go`
- `sidecar/web/src/pages/agentdb/AgentDBProvisioningPanels.jsx`
- `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`

