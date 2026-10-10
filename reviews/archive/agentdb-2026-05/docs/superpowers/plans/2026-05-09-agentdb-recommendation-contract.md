# Agent DB Recommendation Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Agent DB query tuning recommendations machine-readable enough for an agent to safely act on them and report the outcome.

**Architecture:** Extend the existing Agent DB recommendations endpoint instead of creating a parallel API. Keep backward compatibility by retaining `payload`, `kind`, `title`, `detail`, and `status`, while promoting action metadata to first-class fields.

**Tech Stack:** Go, pgx/PostgreSQL, net/http, React, Vitest, Playwright.

---

## File Structure

- Modify `sidecar/internal/agentdb/types.go`
  - Add action fields to `Recommendation` and `RecommendationCreate`.
  - Extend `FeedbackRequest`.
- Modify `sidecar/internal/agentdb/schema.go`
  - Add schema migrations for recommendation action fields.
- Modify `sidecar/internal/agentdb/operations.go`
  - Persist and return action fields, payload, and feedback metadata.
- Modify `sidecar/internal/api/agent_db_handlers.go`
  - Parse action fields and feedback outcome fields.
- Modify `sidecar/internal/agentdb/store_test.go`
  - Add store-level tests for actionable recommendation contract and feedback.
- Modify `sidecar/internal/api/agent_db_handlers_test.go`
  - Add API tests for creating/listing/feedback on actionable recommendations.
- Modify `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
  - Display action metadata in the recommendations list.
- Modify `sidecar/web/src/pages/AgentDBsPage.test.jsx`
  - Add Vitest assertions for action metadata.
- Modify `sidecar/web/e2e/agentdb-fixtures.ts`
  - Mock action metadata in recommendations.
- Modify `sidecar/web/e2e/agent-dbs.spec.ts`
  - Add Playwright assertion that action metadata appears.
- Modify `tasks/todo.md`
  - Add slice checklist.

## Task 1: Backend Contract Tests

**Files:**
- Modify: `sidecar/internal/agentdb/store_test.go`

- [ ] Write failing test `TestRecommendationContractStoresActionMetadataAndFeedback`:
  - Register a deployment.
  - Upsert a recommendation with:
    - `action_type=query_rewrite`
    - `action_risk=safe`
    - `confidence=0.82`
    - `agent_instructions={"expected_change":"add LIMIT"}`
    - `payload={"sql_before":"select * from items","sql_after":"select * from items limit 10"}`
  - List recommendations.
  - Assert action fields, payload, and status are returned.
  - Submit feedback with `decision=accepted`, `applied=true`, `result=rewrote query`.
  - List again and assert status is `accepted` and feedback fields are present.
- [ ] Run:
  - `go test -count=1 ./internal/agentdb -run TestRecommendationContractStoresActionMetadataAndFeedback`
  - Expected: fail because fields are missing.

## Task 2: Backend Implementation

**Files:**
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Modify: `sidecar/internal/agentdb/operations.go`

- [ ] Add fields:
  - `action_type text not null default ''`
  - `action_risk text not null default 'review'`
  - `confidence double precision not null default 0`
  - `agent_instructions jsonb not null default '{}'::jsonb`
- [ ] Update insert/upsert/list scans.
- [ ] Update feedback to store `decision`, `comment`, `applied`, `result`, and `error`.
- [ ] Run targeted backend test.

## Task 3: API Contract Tests And Parsing

**Files:**
- Modify: `sidecar/internal/api/agent_db_handlers_test.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`

- [ ] Write failing API test `TestAgentDBRecommendationContractEndpoints`:
  - POST recommendation with action fields and payload.
  - GET recommendations and assert fields are present.
  - POST feedback with `decision=accepted`, `applied=true`, `result=rewrote query`.
  - GET recommendations and assert status is `accepted` and feedback includes result.
- [ ] Run:
  - `go test -count=1 ./internal/api -run TestAgentDBRecommendationContractEndpoints`
  - Expected: fail until handlers parse/return fields.
- [ ] Add parsing and rerun.

## Task 4: UI Action Metadata

**Files:**
- Modify: `sidecar/web/src/pages/agentdb/AgentDBSections.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/e2e/agentdb-fixtures.ts`
- Modify: `sidecar/web/e2e/agent-dbs.spec.ts`

- [ ] Display recommendation action metadata:
  - action type
  - action risk
  - confidence as percentage
  - concise agent instruction summary when present
- [ ] Add Vitest and Playwright coverage.
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
