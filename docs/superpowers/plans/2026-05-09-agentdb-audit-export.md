# Agent DB Audit Export Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add first-class audit event listing and JSONL export for agent-provisioned database lifecycle, tuning, backup, and recommendation actions.

**Architecture:** The store already writes append-only audit rows into `sage.agent_db_audit`; this slice exposes that ledger as typed API responses and newline-delimited JSON export. The UI consumes the same endpoint in the selected deployment detail view without expanding the already-large Agent DB section file.

**Tech Stack:** Go, pgx, net/http, React, Vitest, Playwright.

---

### Task 1: Store Audit Contract

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Create: `sidecar/internal/agentdb/audit.go`
- Test: `sidecar/internal/agentdb/store_test.go`

- [ ] Write `TestAuditEventsListAndExportJSONL` before implementation. It registers a deployment, performs ping, recommendation feedback, and backup writes, then asserts ordered audit rows and JSONL lines contain parseable `deployment_id`, `event`, `detail`, and `created_at`.
- [ ] Run `go test -count=1 ./internal/agentdb -run TestAuditEventsListAndExportJSONL` and confirm it fails because `AuditEvents` does not exist.
- [ ] Add `AuditEvent` to `types.go` with JSON fields `audit_id`, `deployment_id`, `event`, `detail`, and `created_at`.
- [ ] Implement `AuditEvents(ctx,id)` and `AuditJSONL(ctx,id)` in `audit.go`.
- [ ] Run the targeted store test and confirm it passes.

### Task 2: Audit API

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Create: `sidecar/internal/api/agent_db_audit_handlers.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] Write `TestAgentDBAuditEndpointsExposeEventsAndJSONL` before implementation. It seeds a deployment and recommendation feedback, then calls `/audit` and `/audit/export`.
- [ ] Run `go test -count=1 ./internal/api -run TestAgentDBAuditEndpointsExposeEventsAndJSONL` and confirm it fails because the route is missing.
- [ ] Route `GET /api/v1/agent-dbs/{id}/audit` to JSON `{ "audit_events": [...] }`.
- [ ] Route `GET /api/v1/agent-dbs/{id}/audit/export` to `application/x-ndjson`.
- [ ] Run the targeted API test and confirm it passes.

### Task 3: Audit UI

**Files:**
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Create: `sidecar/web/src/pages/agentdb/AuditEventsPanel.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/e2e/agentdb-fixtures.ts`
- Modify: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Add mocked `/audit` response to the React unit test before UI implementation and assert `register` plus `recommendation_feedback` render.
- [ ] Run `npm test -- --run AgentDBsPage.test.jsx` and confirm it fails because audit events are not rendered.
- [ ] Add `AuditEventsPanel` as a focused component with stable compact rows.
- [ ] Fetch `/api/v1/agent-dbs/{id}/audit` in `useAgentDBDetail` and pass `auditEvents` to `DeploymentDetail`.
- [ ] Add Playwright fixture and assertion for audit events.
- [ ] Run Vitest and Playwright targeted tests.

### Task 4: Verification

**Files:**
- Modify: `tasks/todo.md`

- [ ] Run `gofmt` on changed Go files.
- [ ] Run targeted Go tests for `internal/agentdb` and `internal/api`.
- [ ] Run targeted Vitest and Playwright tests.
- [ ] Run `git diff --check`.
- [ ] Update `tasks/todo.md` checklist.
