# AgentDB End-to-End Provisioning Gauntlet Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the release-blocking gaps where AgentDB primitives work separately but do not drive a complete approved provisioning lifecycle.

**Architecture:** Keep the existing `agentdb.Store` and API route structure. Add small composition methods that convert blueprints, Terraform templates, and approved agent requests into normal deployments, then reuse the existing provider runner execution path. Live cloud execution stays env-gated and explicit.

**Tech Stack:** Go, pgx/Postgres, existing AgentDB store/API, React/Vitest, Playwright.

---

## File Structure

- Modify `sidecar/internal/agentdb/types.go`: add request structs for blueprint/template/request provisioning.
- Modify `sidecar/internal/agentdb/blueprint.go`: add blueprint lookup/approval/provision helpers.
- Modify `sidecar/internal/agentdb/terraform_templates.go`: add template lookup/provision helpers.
- Modify `sidecar/internal/agentdb/store.go`: add approved agent request to deployment helper.
- Modify `sidecar/internal/agentdb/execution.go`: use provider runners for live status/backup/destroy flows.
- Add `sidecar/internal/agentdb/runtime_registry.go`: env-gated AWS/GCP/Lakebase runtime runner registry.
- Modify `sidecar/internal/api/agent_db_handlers.go`: use runtime registry in production routes and add new route dispatch.
- Modify `sidecar/internal/api/agent_db_blueprint_handlers.go`: expose blueprint approval/provision.
- Modify `sidecar/internal/api/agent_db_terraform_template_handlers.go`: expose template provision.
- Modify `sidecar/internal/api/agent_db_deploy_request_handlers.go`: expose submit-for-review in UI/API flow.
- Modify `sidecar/web/src/pages/AgentDBsPage.jsx` and AgentDB subcomponents: add missing submit-review/approve/provision affordances.
- Add/modify tests under `sidecar/internal/agentdb`, `sidecar/internal/api`, and `sidecar/web/e2e`.

## Tasks

- [ ] Write failing store tests:
  - blueprint generated -> approved -> deployment created with `metadata.blueprint_id`, `metadata.terraform_template_id`, `provisioning_status=planned`
  - Terraform template approved -> deployment created with `metadata.terraform_template_id`
  - approved agent request -> deployment created and linked to `metadata.request_id`
  - live execute/status/backup/destroy use native `ProviderRunner`, not command dry-run runner

- [ ] Implement store composition helpers:
  - `GetBlueprint`
  - `ApproveBlueprint`
  - `ProvisionFromBlueprint`
  - `GetTerraformTemplate`
  - `ProvisionFromTerraformTemplate`
  - `ProvisionApprovedRequest`

- [ ] Add API routes and tests:
  - `POST /api/v1/agent-dbs/blueprints/{id}/approve`
  - `POST /api/v1/agent-dbs/blueprints/{id}/provision`
  - `POST /api/v1/agent-dbs/terraform-templates/{id}/provision`
  - `POST /api/v1/agent-dbs/requests/{id}/provision`
  - `POST /api/v1/agent-dbs/{deployment_id}/provision/status` uses native runner when deployment is live.

- [ ] Add env-gated production runner registry:
  - Only register native cloud runners when `PG_SAGE_LIVE_PROVISIONING=1`.
  - AWS RDS requires `PG_SAGE_AWS_REGION` or AWS region env.
  - GCP Cloud SQL requires project, region, and token env for the current HTTP client.
  - Lakebase requires Databricks host/token env.
  - If unset, preserve dry-run behavior.

- [ ] Add UI affordances:
  - Blueprint cards can approve and provision.
  - Template cards can approve and provision.
  - Deploy requests can submit for review, approve, and show why draft cannot approve directly.
  - Live execution controls call the existing guarded API body with explicit operator gates.

- [ ] Add E2E gauntlet:
  - Use fake runner registry for API tests.
  - Browser test covers create blueprint -> provision deployment -> preflight -> live execute -> status -> backup record/check -> cost sample -> ping -> archive/TTL -> destroy.
  - Live cloud tests remain gated and reuse provider-specific tests plus optional blueprint-created deployment inputs.

- [ ] Verify:
  - `go test -v -cover -count=1 -p 1 ./internal/agentdb ./internal/api`
  - focused web Vitest for AgentDB page
  - focused Playwright AgentDB e2e
  - live AWS/GCP/Lakebase gated tests only when explicitly enabled
