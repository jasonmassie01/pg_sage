# AgentDB Release Hardening Report - 2026-05-10

Local sidecar:
- URL: http://127.0.0.1:8085/
- Admin email: admin@pg-sage.local
- Admin password: pgSageQA!2026
- Config: `C:\Users\jmass\pg_sage\local_config.yaml`

## Worktree Classification

Product code to keep:
- AgentDB lifecycle, blueprints, Terraform templates, provider configs, live provider runners, provider policy, naming, redaction, cost, backup/restore, and runtime registry.
- API handlers for AgentDB execution, blueprints, provider config, Terraform templates, and provisioning controls.
- UI pages/components for AgentDB workspace, compact tabs, provisioning, provider settings, Terraform upload/import, blueprint builder, deployments, and tooltips.
- Tuning additions for JSON, vector, PostGIS, cases, actions, and SQL/index tuning surfaces.

Tests to keep:
- Go unit/integration tests for AgentDB store, lifecycle, runners, policies, blueprints, Terraform, provider config, naming, redaction, cost, and live product-chain gates.
- Root Playwright tests for the 8085 sidecar.
- Web Vitest and Playwright tests for AgentDB UI flows.
- WSL race-test receipt for `internal/agentdb` and `internal/api`.

Docs/runbooks/reports to keep:
- AgentDB provider provisioning specs and implementation plans.
- `docs/runbooks/agentdb-live-provisioning.md`
- Release/readiness/cloud validation reports under `docs/reports`.

Temporary artifacts:
- Raw logs, screenshots, receipts, and one-off QA scripts are local artifacts.
- `.gitignore` now ignores `test-output/`, `sidecar/test-output/`, and `QA_*.md`.

## Test Results

Command: `go test -cover -count=1 -p 1 ./...`
Result: pass.
Coverage floors: business packages met the 70% floor.

Notable coverage:
- `internal/agentdb`: 71.2%
- `internal/api`: 70.5%
- `internal/advisor`: 73.1%
- `internal/analyzer`: 84.9%
- `internal/cases`: 88.0%
- `internal/optimizer`: 73.8%
- `internal/tuner`: 86.2%

Low/no-coverage packages:
- `cmd/create_admin`: no tests, command helper.
- `cmd/create_admin_alloydb`: no tests, command helper.
- `cmd/reset_admin_for_test`: no tests, local QA helper.
- `cmd/pg_sage_sidecar`: low coverage, main wiring package.
- `web/node_modules/flatted/golang/pkg/flatted`: vendored node module noise.

Command: `go test -json -count=1 -p 1 ./...`
Result: 5748 test passes, 0 failures, 12 test skips, 4 no-test package skips.

Skipped tests:
- AWS/GCP/Lakebase live provider tests and live AgentDB gauntlets when provider flags are not set.
- Collector replication slot live test.
- Unix log directory test on Windows.
- Real Gemini RCA live test.
- Startup live Postgres checks.

Command: `go test -tags=e2e -count=1 ./e2e -v`
Result: pass.

Command: `npm --prefix sidecar/web test`
Result: 49 tests passed.

Command: `npm --prefix sidecar/web run lint`
Result: pass.

Command: `npm --prefix sidecar/web run build`
Result: pass. Vite emitted the known large bundle warning.

Command: `npm test -- --workers=1` from `e2e/`
Result: 47 passed, 136 skipped, 0 failed against http://127.0.0.1:8085.
The skipped root Playwright tests are explicitly gated/stale walkthrough checks.

Command: `npm --prefix sidecar/web run test:e2e -- --workers=1`
Result: 52 passed, including AgentDB deployments, provisioning request path,
cloud preflight/dry-run/live controls, destroy controls, status/reconcile,
backup/restore drill, Lakebase size profile, promotions, Terraform approval,
blueprint approval, approved agent API request provisioning, and tooltips.

Command: WSL `go1.24.0 test -race -count=1 ./internal/agentdb ./internal/api`
Result: pass.

## Live Cloud Validation

GCP live validation was run with explicit 90-minute timeout:

```powershell
$env:PG_SAGE_LIVE_GCP_CLOUDSQL='1'
$env:PG_SAGE_LIVE_AGENTDB_GAUNTLET='1'
$env:PG_SAGE_GCP_PROJECT='satty-488221'
$env:PG_SAGE_GCP_REGION='us-central1'
$env:PG_SAGE_GCP_ACCESS_TOKEN=(gcloud auth print-access-token)
go test -timeout 90m -count=1 ./internal/agentdb -run 'TestCloudSQLLiveProvisioning|TestAgentDBLiveGauntletTerraformTemplateToCloudSQL' -v
```

Result:
- `TestCloudSQLLiveProvisioning`: pass, 422.49s.
- `TestAgentDBLiveGauntletTerraformTemplateToCloudSQL`: pass, 902.60s.

Important bug found:
- First GCP live run used Go's default ten-minute timeout and panicked while
  Cloud SQL was still `PENDING_CREATE`.
- Cleanup was executed immediately and confirmed deletion of
  `pgsage-live-gcp-cloudsql-20260510142318`.
- Runbook now requires explicit `-timeout` for live cloud tests.

Post-run GCP cleanup sweep:
- Only pre-existing `sage-test` remained in Cloud SQL.
- Disposable `pgsage-*` resources from the live test were deleted.

AWS:
- Earlier receipts show AWS RDS live provider and blueprint gauntlet resources
  were created and deleted with `cleanup_confirmed=true`.
- Current AWS sweep could not run because the AWS session is expired and
  requires `aws login`.

Databricks:
- Earlier receipts show Lakebase branch and agent-request gauntlet resources
  were created and deleted with `cleanup_confirmed=true`.
- Current API sweep returned `{}` for database instances. The CLI also emits a
  local version-selection warning because both old and new Databricks CLIs are
  on PATH.

## Bugs Found and Fixed

1. The 8085 sidecar catalog can be stale after destructive integration tests
   mutate `sage.databases`. Restarting the sidecar re-registers YAML fleet
   databases before browser tests.
2. Live cloud delete could leave AgentDB deployments at `destroying` after the
   provider resource was already gone. `CheckProvisionStatusLive` now maps
   provider `not_found` during `destroying` to terminal `destroyed`, and the
   product-chain live test records that state.

## Release Notes

The release is functionally strong for AgentDB local, UI, API, dry-run, GCP live
Cloud SQL, and GCP product-chain provisioning. Remaining release caveats:
- Re-authenticate AWS before the final AWS orphan sweep.
- Provide an explicit Lakebase source instance/branch and token env vars before
  rerunning Lakebase live tests.
- Root Playwright still contains many intentionally skipped legacy walkthrough
  checks; the newer web AgentDB E2E suite covers the active AgentDB UI actions.

## Final Local Sweep

Final local checks completed after commit slicing:
- `git status --short`: clean.
- 8085 sidecar running and accepting `admin@pg-sage.local` / `pgSageQA!2026`.
- `/api/v1/databases`, `/api/v1/agent-dbs`, `/api/v1/agent-dbs/blueprints`,
  and `/api/v1/agent-dbs/provider-configs` returned HTTP 200.
- GCP Cloud SQL sweep showed only the pre-existing `sage-test` instance.
- Databricks database instances API returned `{}`.

Commit slices:
- `feat(agentdb): add live provider provisioning`
- `feat(tuning): enrich cases and extension tuning`
- `feat(ui): improve AgentDB provisioning workspace`
- `test(e2e): harden browser coverage`
- `docs(agentdb): add live provisioning release runbooks`

No GitHub push, tag, or PR was performed in this pass.
