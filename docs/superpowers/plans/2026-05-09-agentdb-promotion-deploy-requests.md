# Agent DB Promotion Deploy Requests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add review-only promotion deploy requests for agent-created database workspaces.

**Architecture:** Deploy requests are deployment-scoped records that preserve migration SQL, verification SQL, rollback SQL or forward-fix notes, risk, gates, and review transitions. This slice deliberately does not execute production DDL or create provider branch merges.

**Tech Stack:** Go, pgx, net/http, React, Vitest, Playwright.

---

### Task 1: Store Contract

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Create: `sidecar/internal/agentdb/deploy_requests.go`
- Create: `sidecar/internal/agentdb/deploy_requests_test.go`

- [ ] Write failing tests for create/list/review/approve/deny.
- [ ] Add deploy request types and schema.
- [ ] Implement create/list/get/request-review/approve/deny.
- [ ] Record audit events for each transition.
- [ ] Run targeted store tests.

### Task 2: API Contract

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Create: `sidecar/internal/api/agent_db_deploy_request_handlers.go`
- Modify: `sidecar/internal/api/agent_db_handlers_test.go`

- [ ] Write failing API route test for deployment-scoped deploy requests.
- [ ] Add deployment-scoped routes.
- [ ] Parse create/review request bodies and return store errors through existing mapping.
- [ ] Run targeted API tests.

### Task 3: UI Contract

**Files:**
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Create: `sidecar/web/src/pages/agentdb/PromotionPanel.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/e2e/agentdb-fixtures.ts`
- Modify: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Write UI tests for promotion panel rendering and review transitions.
- [ ] Add fetch/action wiring in detail state.
- [ ] Add compact promotion panel with create/request-review/approve/deny.
- [ ] Add Playwright fixture and browser test.

### Task 4: Verification

**Files:**
- Modify: `tasks/todo.md`

- [ ] Run targeted Go, Vitest, Playwright, and diff checks.
- [ ] Run full verification after all remaining slices.
