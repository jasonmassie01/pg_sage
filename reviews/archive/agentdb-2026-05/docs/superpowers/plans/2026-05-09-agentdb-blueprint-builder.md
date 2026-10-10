# AgentDB Blueprint Builder Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an AgentDB Blueprint Builder that converts English deployment
intent into a typed blueprint plus draft Terraform template.

**Architecture:** Add a focused AgentDB blueprint domain in Go with a generator
interface, deterministic fallback, Terraform rendering, store persistence, API
routes, and an AgentDB UI tab. Generated Terraform remains draft-only and flows
through existing template policy and approval gates.

**Tech Stack:** Go, pgx, existing AgentDB store/API patterns, existing LLM
client interface, React/Vitest, existing Terraform template validator.

---

## Files

- Create: `sidecar/internal/agentdb/blueprint.go`
- Create: `sidecar/internal/agentdb/blueprint_test.go`
- Create: `sidecar/internal/api/agent_db_blueprint_handlers.go`
- Modify: `sidecar/internal/agentdb/types.go`
- Modify: `sidecar/internal/agentdb/schema.go`
- Modify: `sidecar/internal/api/agent_db_handlers.go`
- Modify: `sidecar/internal/api/router.go`
- Modify: `sidecar/internal/api/agent_db_handlers_test.go`
- Modify: `sidecar/web/src/pages/AgentDBsPage.jsx`
- Modify: `sidecar/web/src/pages/AgentDBsPage.test.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBWorkspace.jsx`
- Modify: `sidecar/web/src/pages/agentdb/AgentDBWorkspaceTabs.jsx`
- Modify: `sidecar/web/src/pages/agentdb/TerraformTemplatePanel.jsx`
- Create: `sidecar/web/src/pages/agentdb/BlueprintBuilderPanel.jsx`

## Tasks

### Task 1: Domain and Tests

- [ ] Write failing tests for deterministic blueprint extraction, policy
  findings, Terraform rendering, and store persistence.
- [ ] Add blueprint request/spec/generation/types.
- [ ] Implement deterministic extraction and Terraform rendering.
- [ ] Add `sage.agent_db_blueprints` schema.
- [ ] Add store create/list methods that also create a draft Terraform template.
- [ ] Run targeted `go test -count=1 ./internal/agentdb -run TestBlueprint`.

### Task 2: API

- [ ] Write failing API tests for POST/GET `/api/v1/agent-dbs/blueprints`.
- [ ] Add blueprint handlers.
- [ ] Wire optional LLM generator into AgentDB routes.
- [ ] Preserve deterministic fallback for tests and local deployments without LLM.
- [ ] Run targeted `go test -count=1 ./internal/api -run TestAgentDBBlueprint`.

### Task 3: UI

- [ ] Write failing Vitest coverage for the Blueprints tab and Terraform upload
  file body shape.
- [ ] Add `Blueprints` tab and panel.
- [ ] Fetch/list blueprints and submit English intent.
- [ ] Fix Terraform upload to send `body`.
- [ ] Run `npm --prefix sidecar/web test -- --run src/pages/AgentDBsPage.test.jsx`.

### Task 4: Verification and 8085

- [ ] Run `gofmt`.
- [ ] Run focused Go tests for AgentDB/API.
- [ ] Run `go test -cover -count=1 -p 1 ./...`.
- [ ] Run `go vet ./...`.
- [ ] Run `npm --prefix sidecar/web test`.
- [ ] Run `npm --prefix sidecar/web run lint`.
- [ ] Run `npm --prefix sidecar/web run build`.
- [ ] Run `git diff --check`.
- [ ] Rebuild and restart the 8085 sidecar.
- [ ] Verify root and metrics return HTTP 200.

## Self Review

- Scope is one subsystem: blueprint generation and review, not live apply.
- Generated Terraform is draft-only.
- No secrets are accepted in generated Terraform files.
- The LLM boundary is testable and optional.
