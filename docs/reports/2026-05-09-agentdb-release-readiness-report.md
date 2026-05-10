# AgentDB Release Readiness Report - 2026-05-09

## Local Sidecar

- URL: http://127.0.0.1:8085/
- Metrics: http://127.0.0.1:9187/metrics
- Admin email: admin@pg-sage.local
- Admin password: pgSageQA!2026
- Process: `pg_sage_sidecar_qa.exe --config=..\local_config.yaml`
- Latest verified PID: 40896

## Product Work Completed Today

- AgentDB provisioning supports local/provider contract flows for AWS RDS,
  GCP Cloud SQL, and Databricks Lakebase branch mode.
- Lakebase branch requests now accept an explicit source instance or branch.
- AgentDB UI has compact tabs, per-tab descriptions, provider settings,
  Terraform upload/import, blueprint generation, and documented form tips.
- Overview now uses tabs for Databases, Provider Readiness, and Recent Recos.
  Databases is the default tab.
- Cases, Actions, and Fleet pages now explain that findings become cases, and
  actionable cases flow into Actions for approval, execution, and audit.
- GCP Cloud SQL has a real REST Admin API client and live-gated lifecycle test.
- AWS RDS and GCP Cloud SQL live tests now write JSONL receipts after verified
  cleanup.

## Live Cloud Validation Receipts

Receipt file: `sidecar/test-output/agentdb-live-receipts.jsonl`

AWS RDS:

- Provider: `aws_rds`
- Region: `us-east-2`
- Resource ID: `pgsage-live-aws-rds-20260509215816`
- Class/storage: `db.t4g.micro`, 20 GiB
- Backup retention: 1 day
- Create accepted, status reached available, delete verified by not-found
  readback.
- Cleanup confirmed: true

GCP Cloud SQL:

- Provider: `gcp_cloudsql`
- Project: `satty-488221`
- Region: `us-central1`
- Resource ID: `pgsage-live-gcp-cloudsql-20260509215818`
- Tier/storage: `db-f1-micro`, 20 GiB
- Backup enabled: true
- Public IPv4 enabled for the smoke test, authorized networks count is zero.
- Create accepted, status reached RUNNABLE/available, delete verified by
  not-found readback.
- Cleanup confirmed: true

Databricks Lakebase:

- Live-gated branch lifecycle test is implemented.
- Not run in this pass because the configured Databricks profile
  `dbc-a03bc092-e6ba` reports invalid CLI auth and no token is available in
  `DATABRICKS_TOKEN` or `PG_SAGE_DATABRICKS_TOKEN`.
- Required env for the live test:
  `PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1`,
  `PG_SAGE_DATABRICKS_HOST` or `DATABRICKS_HOST`,
  `PG_SAGE_DATABRICKS_TOKEN` or `DATABRICKS_TOKEN`,
  `PG_SAGE_LIVE_LAKEBASE_PROJECT`,
  `PG_SAGE_LIVE_LAKEBASE_SOURCE_INSTANCE`.

## Browser QA

Browser plugin path:

- Browser plugin was available, but the Node REPL transport closed before it
  could attach to the in-app browser. Fallback was Playwright against the live
  8085 sidecar.

Playwright live-8085 checks:

- CHECK-01: PASS - logged in with the admin account.
- CHECK-02: PASS - Overview defaults to Databases tab.
- CHECK-03: PASS - Overview Databases description rendered.
- CHECK-04: PASS - Overview Provider Readiness tab and description rendered.
- CHECK-05: PASS - Overview Recent Recos tab and description rendered.
- CHECK-06: PASS - Cases page description rendered.
- CHECK-07: PASS - Cases text mentions flow to Actions.
- CHECK-08: PASS - Actions page description rendered.
- CHECK-09: PASS - Actions text mentions approval queue and audit log.
- CHECK-10: PASS - Fleet page description rendered.
- CHECK-11: PASS - Fleet text mentions cases flowing to actions.
- CHECK-12: PASS - AgentDB Deployments tab description rendered.
- CHECK-13: PASS - AgentDB Provision tab description rendered.
- CHECK-14: PASS - AgentDB provision form exposes 12 tooltip triggers.
- CHECK-15: PASS - AgentDB Provider Settings tab description rendered.
- CHECK-16: PASS - AgentDB Terraform tab description rendered.
- CHECK-17: PASS - AgentDB Blueprints tab description rendered.

Screenshots:

- `%TEMP%\pg_sage_8085_browser_qa\overview-tabs.png`
- `%TEMP%\pg_sage_8085_browser_qa\agentdb-tabs.png`

## Automated Verification

- `go test -count=1 ./internal/agentdb`: PASS
- `go test -count=1 ./internal/agentdb -run
  "TestAWSRDSLiveProvisioning|TestCloudSQLLiveProvisioning|TestLakebaseLiveProvisioning|TestCloudSQLHTTPClientCreateStatusDestroy" -v`:
  PASS with live-gated AWS/GCP/Lakebase tests skipped when flags are unset;
  Cloud SQL HTTP client unit test passed.
- `PG_SAGE_LIVE_AWS_RDS=1 go test -count=1 -p 1 ./internal/agentdb -run
  '^TestAWSRDSLiveProvisioning$' -v -timeout 60m`: PASS
- `PG_SAGE_LIVE_GCP_CLOUDSQL=1 go test -count=1 -p 1 ./internal/agentdb -run
  '^TestCloudSQLLiveProvisioning$' -v -timeout 60m`: PASS
- `go test -cover -count=1 -p 1 ./...`: PASS
- `go vet ./...`: PASS
- `npm --prefix sidecar/web test -- --run`: PASS, 44 tests
- `npm --prefix sidecar/web run lint`: PASS
- `npm --prefix sidecar/web run build`: PASS, with existing Vite chunk-size
  warning for the main JS bundle.
- `npm --prefix sidecar/web run test:e2e -- --workers=1`: PASS, 48 tests
- `wsl ... go test -race -count=1 ./internal/agentdb`: PASS after installing
  Go 1.24.13 in WSL and using WSL gcc.

## Coverage

Core business packages met the configured 70 percent floor in the latest
coverage run:

- `internal/agentdb`: 71.0%
- `internal/api`: 70.1%
- `internal/advisor`: 73.1%
- `internal/analyzer`: 85.0%
- `internal/cases`: 88.0%
- `internal/optimizer`: 73.8%
- `internal/tuner`: 86.2%

Low/no coverage packages are command entrypoints or generated/vendor-like
surfaces:

- `cmd/create_admin`: 0.0%
- `cmd/create_admin_alloydb`: 0.0%
- `cmd/pg_sage_sidecar`: 3.0%
- `cmd/reset_admin_for_test`: 0.0%
- `web/node_modules/flatted/golang/pkg/flatted`: 0.0%

## Worktree Classification

Keep as product code:

- `sidecar/internal/agentdb/*runner*.go`, provider policy/cost/redaction,
  Terraform, blueprint, and live receipt files.
- AgentDB API handler files under `sidecar/internal/api/`.
- AgentDB React components under `sidecar/web/src/pages/agentdb/`.
- Overview/Cases/Actions/Fleet UI page changes.
- Generated embedded web dist files under `sidecar/internal/api/dist/`.

Keep as tests:

- AgentDB provider/unit/live-gated tests under `sidecar/internal/agentdb/`.
- Web unit tests for AgentDB, Dashboard, Cases, Actions, Settings, Databases.
- Updated Playwright tests under `sidecar/web/e2e/`.
- Root `e2e/agentdb-live-provisioning.spec.ts` if we keep root-level live smoke
  coverage.

Keep as docs/runbooks/reports:

- `docs/superpowers/specs/2026-05-09-agentdb-live-provisioning-ga.md`
- `docs/superpowers/plans/2026-05-09-agentdb-live-provisioning-ga.md`
- `docs/runbooks/agentdb-live-provisioning.md`
- `docs/reports/2026-05-09-agentdb-release-readiness-report.md`
- Cloud provisioning and SQL hardening reports.

Temporary or manual QA artifacts to review before commit:

- `QA_REPORT.md`, `QA_CHECKPOINT.md`
- `test-output/`
- `sidecar/test-output/` except `agentdb-live-receipts.jsonl` if we want to
  keep live validation evidence in-repo.
- `e2e/manual_qa.js`, `e2e/manual_network_errors.js`,
  `e2e/verify_auth_18085.js`, `e2e/verify_auth_routes_18085.js`,
  `e2e/verify_restore_8085.js`

## Remaining Release Items

1. Decide whether live receipt JSONL and screenshots should be committed or
   stored outside git as release evidence.
2. Split the large dirty worktree into logical commits before tagging or PR.
3. Consider code splitting for the web bundle after release; current Vite build
   passes but warns that the main chunk exceeds 500 kB.

## 2026-05-09 End-to-End Provisioning Gauntlet Update

Closed the release-blocking "proved parts, not chain" gaps for the local and
contract-tested path:

- Blueprint approve -> provision now links a deployment to the blueprint and
  generated Terraform template.
- Terraform template approve -> provision now creates a linked deployment.
- Agent API request -> approval -> provision now creates a linked deployment.
- UI/API live execute, live status, live backup assurance, and live destroy now
  route through the native runner registry when live provisioning is enabled.
- Runtime runner registration is env-gated, so the 8085 sidecar can stay safe by
  default and switch to AWS/GCP/Lakebase native runners only when explicitly
  enabled.
- Live reconcile now isolates bad or stale deployment rows as blocked items
  instead of failing the whole fleet reconcile sweep.
- AgentDB browser coverage now exercises live execute/destroy controls,
  Terraform approval/provision, blueprint approval/provision, and approved agent
  request provisioning.

Fresh verification receipts:

- `go test -v -cover -count=1 -p 1 ./internal/agentdb ./internal/api`: PASS
  - `internal/agentdb`: 71.0%
  - `internal/api`: 70.3%
  - Skipped live-gated tests: AWS RDS, GCP Cloud SQL, Lakebase.
- `go test -cover -count=1 -p 1 ./...`: PASS
- `go test -json -count=1 -p 1 ./...`: PASS, 5,723 passed, 0 failed,
  22 skipped; JSON receipt written to
  `test-output/go-test-json-20260509-agentdb-gauntlet.jsonl`.
- `npm --prefix sidecar/web test`: PASS, 48 tests.
- `npm --prefix sidecar/web run lint`: PASS.
- `npm --prefix sidecar/web run build`: PASS, with the existing Vite
  large-chunk warning.
- `npm --prefix sidecar/web run test:e2e -- --workers=1`: PASS, 52 tests.
- `PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 go test -count=1 -p 1
  ./internal/agentdb -run '^TestLakebaseLiveProvisioning$' -v -timeout 30m`:
  PASS, created/status-checked/deleted branch
  `pgsage-live-lakebase-20260510001443`; cleanup confirmed in
  `sidecar/test-output/agentdb-live-receipts.jsonl`.
- Rebuilt and restarted the local 8085 QA sidecar with
  `sidecar/pg_sage_sidecar_qa_next.exe --config=..\local_config.yaml`.
  - UI probe `GET http://127.0.0.1:8085/`: HTTP 200.
  - Login probe `POST /api/v1/auth/login`: HTTP 200 for
    `admin@pg-sage.local`.
  - Admin reset/create succeeded on reachable fleet DBs at ports 5432, 5433,
    5434, 5436, and 55432.
  - Configured DBs at ports 5435 and 5437 were unreachable during reset.

Remaining proof gap after this update:

- Run the new full chain against disposable live resources, not only the native
  runner contract tests. Prior live provider create/status/delete receipts exist
  for AWS RDS, GCP Cloud SQL, and Lakebase branch lifecycle, but the newest
  blueprint/template/request-to-live gauntlet still needs a live-cloud receipt
  once we intentionally enable cloud spend for that scenario.

## Skipped Test Closure Pass

Follow-up request: run the previously skipped tests and clean up the cloud
infrastructure afterwards.

Live cloud tests now run without skips:

- `PG_SAGE_LIVE_AWS_RDS=1 go test -count=1 -p 1 ./internal/agentdb -run
  '^TestAWSRDSLiveProvisioning$' -v -timeout 60m`: PASS.
  - Resource: `pgsage-live-aws-rds-20260510012619`
  - Cleanup receipt: delete verified by not-found readback; independent prefix
    sweep found no `pgsage-live-*` RDS leftovers in `us-east-2`.
- `PG_SAGE_LIVE_GCP_CLOUDSQL=1 go test -count=1 -p 1 ./internal/agentdb -run
  '^TestCloudSQLLiveProvisioning$' -v -timeout 60m`: PASS.
  - Resource: `pgsage-live-gcp-cloudsql-20260510013508`
  - Cleanup receipt: delete verified by not-found readback; independent prefix
    sweep found no `pgsage-live-*` Cloud SQL leftovers in project
    `satty-488221`.
- `PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 go test -count=1 -p 1
  ./internal/agentdb -run '^TestLakebaseLiveProvisioning$' -v -timeout 30m`:
  PASS.
  - Resource: `pgsage-live-lakebase-20260510015201`
  - Cleanup receipt: delete verified by not-found readback; independent
    Databricks Lakebase branch prefix sweep found no `pgsage-live-*` branches
    in project `demo`.

Local prerequisite tests now run without skips:

- `go test -count=1 -p 1 ./internal/startup -run
  'TestRunChecks_LivePG|TestCheckConnectivity_LivePG|TestCheckPGVersion_LivePG'
  -v`: PASS against local Postgres on port `55432`.
- `go test -count=1 -p 1 ./internal/alerting -run
  '^TestPhase2_LogAlert_Integration$' -v`: PASS. The fixture now inserts a
  real finding row instead of treating a foreign-key mismatch as a skip.
- `PG_TEST_DSN=postgres://postgres:postgres@127.0.0.1:5437/postgres
  go test -count=1 -p 1 ./internal/collector -run
  '^TestPhase2_CollectReplication_WithSlot$' -v`: PASS after starting the
  local logical-replication test container.
- `PG_TEST_DSN=postgres://postgres:postgres@127.0.0.1:5435/hint_test
  go test -count=1 -p 1 ./test/hint_verify -v`: PASS after starting the local
  `pg_hint_plan` test container.
- `wsl ... GOTOOLCHAIN=local go test -count=1 -p 1 ./internal/logwatch -run
  '^TestResolveLogDir_AbsoluteUnix$' -v`: PASS on WSL/Linux, where the Unix path
  test is applicable.
- `GEMINI_API_KEY=<env> go test -count=1 -p 1 ./internal/rca -run
  '^TestTier2Live_RealGemini$' -v -timeout 5m`: PASS with the key supplied only
  through environment variables.

Local state adjustments:

- Started `pg-sage-logical-test` and `pg-hint-test` Docker containers because
  they are configured 8085 fleet targets and were required for the collector and
  hint verification tests.
- Reset or created the standard local 8085 admin account on those now-reachable
  local DBs where applicable.

Only non-executable package entries and intentionally platform-inapplicable
cases remain as skips when the whole suite is run without live/provider flags.

Final verification after closing skips:

- `go test -cover -count=1 -p 1 ./...`: PASS.
- `go test -json -count=1 -p 1 ./...`: PASS; receipt written to
  `test-output/go-test-json-20260509-skipped-closure.jsonl`.
  - Passed test events: 5,736
  - Failed test events: 0
  - Skip events: 13. These are package entries with no tests plus live/provider
    or platform-gated tests that were each executed explicitly above.

Bugs found and fixed in this closure pass:

- `internal/alerting` log-alert integration was silently skipping a real
  foreign-key mismatch. The test now seeds an actual finding row and fails on
  insert errors.
- `internal/api` findings-list integration depended on a globally empty shared
  test database. The test now inserts a unique category, filters by that
  category, and asserts the returned row content.

## Full Live Product-Chain Gauntlet - 2026-05-10

Goal: prove the complete AgentDB product chain, not only provider contract
tests. The gauntlet starts from a product artifact, creates a deployment record,
runs the native provider runner, waits for availability, records ping, cost, and
backup assurance metadata, destroys the resource, and verifies provider cleanup.

Product-chain paths exercised:

- Blueprint -> deployment -> live AWS RDS create/status/backup/ping/destroy.
- Terraform template -> deployment -> live GCP Cloud SQL create/status/backup/
  ping/destroy.
- Agent API request -> approved request -> deployment -> live Databricks
  Lakebase branch create/status/backup/ping/destroy.

Implementation fixes added during this pass:

- Blueprint, Terraform template, and approved agent request provisioning now
  preserve `provider_params` in deployment metadata so native cloud runners can
  use product/UI/API-supplied provider details at execution time.
- The approved-request API now accepts `provider_params`.
- Provider state transitions now allow `status_unknown -> provisioning`, which
  covers transient AWS RDS statuses that are still in-progress rather than
  terminal failures.
- Regression tests now assert provider params persist across blueprint,
  Terraform template, and approved-request provisioning paths.

Live resource receipts:

- AWS RDS blueprint chain: `pgsage-chain-aws-bp-20260510122813`
  - Region: `us-east-2`
  - Create wait: 513.1s
  - Available wait: 420.0s
  - Delete wait: 90.6s
  - Cleanup: confirmed by not-found readback and independent prefix sweep.
- GCP Cloud SQL Terraform-template chain:
  `pgsage-chain-gcp-tf-20260510120910`
  - Project: `satty-488221`
  - Region: `us-central1`
  - Create wait: 874.7s
  - Available wait: 870.0s
  - Delete wait: 0.7s
  - Cleanup: confirmed by not-found readback and independent prefix sweep.
- Databricks Lakebase agent-request branch chain:
  `pgsage-chain-lake-req-20260510122345`
  - Project: `demo`
  - Source instance: `projects/demo/branches/br-empty-lake-d2ftn13l`
  - Create wait: 32.1s
  - Available wait: 30.0s
  - Delete wait: 0.4s
  - Cleanup: confirmed by not-found readback and independent prefix sweep.

Receipt file:

- `sidecar/test-output/agentdb-live-receipts.jsonl`

Independent cleanup sweep result:

- AWS leftovers: none.
- GCP leftovers: none.
- Lakebase leftovers: none.

8085 sidecar verification:

- URL: `http://127.0.0.1:8085`
- Admin: `admin@pg-sage.local`
- Password: stored only for local QA handoff; do not commit credentials.
- Running process: `pg_sage_sidecar_qa_live.exe`, PID `94040`.
- Probe result: login plus AgentDB API readback passed.
- Browser/API probe receipt:
  `sidecar/test-output/agentdb-ui-backend-probe-mozrvx8i.png`.

Final verification:

- `go test -cover -count=1 -p 1 ./...`: PASS.
- `go test -json -count=1 -p 1 ./...`: PASS.
  - Passed test events: 5,737.
  - Failed test events: 0.
  - Skip events: 17.
  - Remaining skips are no-test package entries, provider/live-gated tests when
    the full suite is run without spend flags, and platform-gated tests. The
    AWS, GCP, Lakebase, Gemini, startup, replication, hint, and WSL-only paths
    were each executed explicitly during the closure passes.
- `npm --prefix sidecar/web test -- --run`: PASS, 48 tests.
- `npm --prefix sidecar/web run lint`: PASS.
- `npm --prefix sidecar/web run build`: PASS, with existing Vite large-chunk
  warning.
- `npm --prefix sidecar/web run test:e2e -- --workers=1`: PASS, 52 tests.

Remaining release risks:

- Live cloud execution is proven but should stay opt-in and cost-gated for the
  default 8085 sidecar.
- Worktree cleanup and commit slicing are still needed before release. Keep
  product code, tests, docs, runbooks, and durable receipts; remove temporary
  manual probes unless intentionally retained as QA artifacts.
- Race tests still need a Linux/WSL or MinGW/gcc path if we want them in the
  release gate.
