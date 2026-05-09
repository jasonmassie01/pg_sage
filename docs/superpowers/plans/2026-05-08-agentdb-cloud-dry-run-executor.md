# Agent DB Cloud Dry Run Executor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add approved dry-run execution for stored cloud instance provisioning plans without creating live cloud resources.

**Architecture:** Agent DB deployments already store provider command plans. This slice adds preflight and dry-run execution attempts, persists command output, exposes API endpoints, and adds UI controls to run preflight/execute safely.

**Tech Stack:** Go, pgx, PostgreSQL metadata tables, React, Vitest, Playwright.

---

### Task 1: Execution Model

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Create: `sidecar/internal/agentdb/execution.go`
- Test: `sidecar/internal/agentdb/execution_test.go`

- [ ] Add `agent_db_provision_attempts` storage for preflight and dry-run execution.
- [ ] Add `PreflightProvision`, `ExecuteProvision`, and `ExecutionAttempts`.
- [ ] Reject local deployments, cloud non-instance deployments, missing plans, duplicate execution, and failed preflight.

### Task 2: API

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Create: `sidecar/internal/api/agent_db_execution_handlers.go`
- Test: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] Add `POST /api/v1/agent-dbs/{id}/provision/preflight`.
- [ ] Add `POST /api/v1/agent-dbs/{id}/provision/execute`.
- [ ] Add `GET /api/v1/agent-dbs/{id}/provision/attempts`.

### Task 3: UI

**Files:**
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
- Modify: `sidecar/web/e2e/fixtures.ts`
- Test: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Test: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Show cloud execution controls only for cloud deployments.
- [ ] Add preflight and dry-run execute buttons.
- [ ] Show latest execution attempts and output.

### Task 4: Verification

**Files:**
- Modify: `tasks/todo.md`

- [ ] Run Go package tests for `internal/agentdb` and `internal/api`.
- [ ] Run frontend unit tests.
- [ ] Run full Go coverage.
- [ ] Run integration tests, Go e2e, frontend lint, build, and Playwright.
