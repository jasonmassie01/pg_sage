# Agent DB Backup Assurance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give agents and operators a safe backup assurance path before cleanup or destroy planning.

**Architecture:** Reuse the existing deployment, backup, and provision-attempt tables. Managed providers get dry-run CLI checks for backup configuration; self-managed/local deployments get `pg_dump` dry-run command planning. Restore drills stay dry-run/planned and must not falsely mark a deployment as restore-verified.

**Tech Stack:** Go, pgx/PostgreSQL, net/http, React, Vitest, Playwright.

---

## File Structure

- Create `sidecar/internal/agentdb/lifecycle.go`
  - Move `ReconcileAbandonedDeployments` out of `store.go`.
- Create `sidecar/internal/agentdb/backup_assurance.go`
  - Add `CheckBackupAssurance` and `PlanRestoreDrillDryRun`.
- Modify `sidecar/internal/agentdb/providers.go`
  - Add provider backup-check and restore-drill command planners.
- Modify `sidecar/internal/agentdb/types.go`
  - Add `BackupAssurance` response type.
- Modify `sidecar/internal/api/agent_db_execution_handlers.go`
  - Add backup assurance handlers.
- Modify `sidecar/internal/api/agent_db_handlers.go`
  - Wire backup assurance routes.
- Modify `sidecar/internal/agentdb/execution_test.go`
  - Add backend tests for backup check and restore drill dry-run.
- Modify `sidecar/internal/api/agent_db_handlers_test.go`
  - Add API tests for new backup assurance endpoints.
- Modify `sidecar/web/src/pages/AgentDBsPage.jsx`
  - Add callback wiring for backup check and restore drill.
- Modify `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
  - Pass backup callbacks to detail panels.
- Create `sidecar/web/src/pages/agentdb/BackupAssurancePanel.jsx`
  - Keep AgentDBSections focused and under file limits.
- Modify `sidecar/web/src/pages/AgentDBsPage.test.jsx`
  - Add Vitest coverage for backup assurance controls.
- Modify `sidecar/web/e2e/agentdb-fixtures.ts`
  - Mock backup assurance routes.
- Modify `sidecar/web/e2e/agent-dbs.spec.ts`
  - Add Playwright coverage for backup assurance controls.
- Modify `tasks/todo.md`
  - Add slice checklist and track completion.

## Task 1: Backend Backup Assurance Tests

**Files:**
- Modify: `sidecar/internal/agentdb/execution_test.go`

- [ ] Write failing tests:
  - `TestBackupAssuranceManagedProviderRecordsVerifiedCheck`
    - Provision an AWS RDS instance.
    - Call `CheckBackupAssurance`.
    - Assert attempt kind `backup_check`, command contains `aws rds describe-db-instances`, status `succeeded`.
    - Assert returned assurance says `managed_provider`, `safe_for_destroy=false`, `backup_status=verified`, and records a backup with status `verified`.
  - `TestRestoreDrillDryRunDoesNotBypassVerifiedRestoreGate`
    - Provision a local schema deployment.
    - Call `PlanRestoreDrillDryRun`.
    - Assert attempt kind `restore_drill_dry_run`, command contains `pg_restore --list`.
    - Assert cleanup/delete is still blocked with `ErrRestoreRequired`.

- [ ] Run:
  - `go test -count=1 ./internal/agentdb -run "TestBackupAssurance|TestRestoreDrill"`
  - Expected: fail because methods do not exist.

## Task 2: Backend Backup Assurance Implementation

**Files:**
- Create: `sidecar/internal/agentdb/lifecycle.go`
- Create: `sidecar/internal/agentdb/backup_assurance.go`
- Modify: `sidecar/internal/agentdb/providers.go`
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/store.go`

- [ ] Move `ReconcileAbandonedDeployments` into `lifecycle.go`.
- [ ] Add `BackupAssurance`.
- [ ] Add backup check commands:
  - AWS: `aws rds describe-db-instances --db-instance-identifier <id>`
  - Cloud SQL: `gcloud sql instances describe <id> --format=json(settings.backupConfiguration)`
  - Lakebase: `databricks database branches get <id>`
  - Local schema/database: `pg_dump --schema-only --schema <schema>` or `pg_dump --schema-only --dbname <database>`
- [ ] Add restore drill dry-run commands:
  - Managed providers: status/get command plus detail that restore drill requires provider snapshot clone.
  - Local/self-managed: `pg_restore --list <archive-uri-or-placeholder>`.
- [ ] Run:
  - `go test -count=1 ./internal/agentdb -run "TestBackupAssurance|TestRestoreDrill|TestReconcileAbandoned"`
  - Expected: pass.

## Task 3: API Routes And Tests

**Files:**
- Modify: `sidecar/internal/api/agent_db_execution_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] Write failing API test:
  - POST `/api/v1/agent-dbs/{id}/backups/check` returns an assurance with `attempt.kind=backup_check`.
  - POST `/api/v1/agent-dbs/{id}/backups/restore-drill-dry-run` returns an attempt with kind `restore_drill_dry_run`.
- [ ] Run:
  - `go test -count=1 ./internal/api -run TestAgentDBBackupAssuranceEndpoints`
  - Expected: fail with 404/missing handlers.
- [ ] Add handlers and routes.
- [ ] Rerun targeted API test.

## Task 4: UI Backup Assurance Controls

**Files:**
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
- Create: `sidecar/web/src/pages/agentdb/BackupAssurancePanel.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/e2e/agentdb-fixtures.ts`
- Modify: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Add UI controls:
  - `Check backups`
  - `Plan restore drill`
- [ ] Show latest backup statuses already returned by `GET /backups`.
- [ ] Add Vitest assertions for new endpoint calls.
- [ ] Add Playwright assertions for new endpoint calls.
- [ ] Run:
  - `npm test -- --run AgentDBsPage.test.jsx`
  - `npm run test:e2e -- agent-dbs.spec.ts`

## Task 5: Full Verification

- [ ] Run `go test -cover -count=1 -p 1 ./...`.
- [ ] Scan Go coverage log for `SKIP`, `TODO`, and `PENDING`.
- [ ] Run integration: `SAGE_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./... -tags=integration -p 1 -count=1 -timeout 600s`.
- [ ] Run Go e2e: `go test -tags=e2e -count=1 -timeout 180s ./e2e`.
- [ ] Run `npm run lint`.
- [ ] Run `npm test -- --run`.
- [ ] Run `npm run build`.
- [ ] Run `npm run test:e2e`.
- [ ] Run `git diff --check`.
- [ ] Report results with coverage gaps and skipped tests.
