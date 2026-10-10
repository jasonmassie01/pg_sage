# Agent DB Lifecycle Controller Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the next Agent DB slice: cloud status dry-runs, destroy dry-runs, and abandoned deployment reconciliation without live cloud mutation.

**Architecture:** Reuse the existing Agent DB deployment, provisioning plan, and provision attempts model. Cloud providers remain dry-run/manual-review only; lifecycle actions record durable attempts and update pg_sage metadata while protecting backup-required deployments from deletion planning until restore verification exists.

**Tech Stack:** Go, pgx/PostgreSQL, net/http, React, Vitest, Playwright.

---

## File Structure

- Modify `sidecar/internal/agentdb/types.go`
  - Add `LifecycleReconcileResult` for reconcile API responses.
- Modify `sidecar/internal/agentdb/providers.go`
  - Add provider-specific status and destroy command builders.
- Modify `sidecar/internal/agentdb/execution.go`
  - Add `CheckProvisionStatus`, `DestroyProvisionDryRun`, and helper recording logic.
- Modify `sidecar/internal/agentdb/operations.go`
  - Add `ReconcileAbandonedDeployments` that archives expired deployments and creates destroy dry-run attempts when backup policy allows.
- Modify `sidecar/internal/api/agent_db_execution_handlers.go`
  - Add status, destroy dry-run, and reconcile handlers.
- Modify `sidecar/internal/api/agent_db_handlers.go`
  - Wire `POST /api/v1/agent-dbs/reconcile`.
  - Wire `POST /api/v1/agent-dbs/{id}/provision/status`.
  - Wire `POST /api/v1/agent-dbs/{id}/provision/destroy-dry-run`.
- Modify `sidecar/web/src/pages/AgentDBsPage.jsx`
  - Add reconcile action and new provisioning lifecycle callbacks.
- Modify `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
  - Pass status and destroy handlers to the cloud panel.
- Modify `sidecar/web/src/pages/agentdb/CloudProvisioningPanel.jsx`
  - Add Check status and Destroy dry-run controls.
- Modify `sidecar/web/src/pages/AgentDBsPage.test.jsx`
  - Assert new controls call the new APIs.
- Modify `sidecar/web/e2e/agentdb-fixtures.ts`
  - Mock reconcile, status, and destroy dry-run endpoints.
- Modify `sidecar/web/e2e/agent-dbs.spec.ts`
  - Add browser coverage for status/destroy/reconcile flows.
- Modify `tasks/todo.md`
  - Add this slice and mark steps as implementation progresses.

## Task 1: Backend Lifecycle Domain Tests

**Files:**
- Modify: `sidecar/internal/agentdb/execution_test.go`

- [ ] Write failing tests:
  - `TestProvisionLifecycleStatusAndDestroyDryRun`:
    - Provision a Cloud SQL instance deployment.
    - Call `CheckProvisionStatus`.
    - Assert attempt kind `status_check`, status `succeeded`, command contains `gcloud sql instances describe`, and provisioning status becomes `status_checked`.
    - Call `DestroyProvisionDryRun`.
    - Assert `ErrRestoreRequired` because default backup policy requires restore verification.
    - Record a `restore_verified` backup.
    - Call `DestroyProvisionDryRun` again.
    - Assert attempt kind `destroy_dry_run`, status `succeeded`, command contains `gcloud sql instances delete`, and provisioning status becomes `destroy_dry_run_ready`.
  - `TestReconcileAbandonedDeploymentsPlansSafeCloudDestroy`:
    - Provision one expired Lakebase instance with backup required and restore-verified backup.
    - Provision one expired AWS instance without restore-verified backup.
    - Call `ReconcileAbandonedDeployments`.
    - Assert both are archived.
    - Assert one destroy dry-run is recorded.
    - Assert one deployment is blocked with reason `verified restore required`.

- [ ] Run:
  - `go test -count=1 ./internal/agentdb -run "TestProvisionLifecycle|TestReconcileAbandoned"`
  - Expected: fail because lifecycle functions do not exist.

## Task 2: Backend Lifecycle Implementation

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/providers.go`
- Modify: `sidecar/internal/agentdb/execution.go`
- Modify: `sidecar/internal/agentdb/operations.go`

- [ ] Implement provider lifecycle commands:
  - AWS status: `aws rds describe-db-instances --db-instance-identifier <resource>`
  - AWS destroy: `aws rds delete-db-instance --db-instance-identifier <resource> --skip-final-snapshot`
  - Cloud SQL status: `gcloud sql instances describe <resource>`
  - Cloud SQL destroy: `gcloud sql instances delete <resource> --quiet`
  - Lakebase status: `databricks database branches get <resource>`
  - Lakebase destroy: `databricks database branches delete <resource>`
- [ ] Implement `CheckProvisionStatus`.
- [ ] Implement `DestroyProvisionDryRun`.
- [ ] Implement `ReconcileAbandonedDeployments`.
- [ ] Run:
  - `go test -count=1 ./internal/agentdb -run "TestProvisionLifecycle|TestReconcileAbandoned"`
  - Expected: pass.

## Task 3: API Tests And Routes

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers_test.go`
- Modify: `sidecar/internal/api/agent_db_execution_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`

- [ ] Write failing API test `TestAgentDBProvisionLifecycleEndpoints`:
  - POST `/api/v1/agent-dbs/{id}/provision/status` returns a `status_check` attempt.
  - POST `/api/v1/agent-dbs/{id}/provision/destroy-dry-run` returns conflict before restore verification.
  - POST backup with `restore_verified`.
  - POST destroy dry-run returns a `destroy_dry_run` attempt.
  - POST `/api/v1/agent-dbs/reconcile` returns archived and destroy_dry_run arrays.
- [ ] Run:
  - `go test -count=1 ./internal/api -run TestAgentDBProvisionLifecycleEndpoints`
  - Expected: fail because routes do not exist.
- [ ] Add handlers and route cases.
- [ ] Run:
  - `go test -count=1 ./internal/api -run TestAgentDBProvisionLifecycleEndpoints`
  - Expected: pass.

## Task 4: UI Lifecycle Controls

**Files:**
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
- Modify: `sidecar/web/src/pages/agentdb/CloudProvisioningPanel.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/e2e/agentdb-fixtures.ts`
- Modify: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Write failing Vitest assertion:
  - `Check status` calls `/api/v1/agent-dbs/adb_ui/provision/status`.
  - `Destroy dry-run` calls `/api/v1/agent-dbs/adb_ui/provision/destroy-dry-run`.
  - `Reconcile abandoned` calls `/api/v1/agent-dbs/reconcile`.
- [ ] Write failing Playwright assertions for the same controls using mocked routes.
- [ ] Implement UI controls and messages.
- [ ] Run:
  - `npm test -- --run AgentDBsPage.test.jsx`
  - `npm run test:e2e -- agent-dbs.spec.ts`
  - Expected: pass.

## Task 5: Full Verification

**Files:**
- No new production files beyond Tasks 1-4.

- [ ] Run backend unit coverage:
  - `go test -cover -count=1 -p 1 ./...`
- [ ] Scan Go test output for `SKIP`, `TODO`, and `PENDING`.
- [ ] Run integration suite:
  - `SAGE_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./... -tags=integration -p 1 -count=1 -timeout 600s`
- [ ] Run Go e2e:
  - `go test -tags=e2e -count=1 -timeout 180s ./e2e`
- [ ] Run frontend checks:
  - `npm run lint`
  - `npm test -- --run`
  - `npm run build`
  - `npm run test:e2e`
- [ ] Run:
  - `git diff --check`
- [ ] Report results using the project test-results format with skipped tests and coverage gaps.
