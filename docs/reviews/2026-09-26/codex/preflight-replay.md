# Replaying the focused validation

These tests intentionally fail against b396595. Use the saved isolated clones or fresh
clones of that commit with the corresponding staged test patches. No command below is
intended for a live installation. The scripts create/drop/reset fixture state.

Run shell commands from the task workspace unless a working directory is specified.
The root fixture is the task-owned container pgsage-validate-runtime-20260926, with a
loopback mapping to port 55440. All four validation containers were removed after testing.
The local pg17-hintplan-clean image was already installed and reports PostgreSQL 17.9.
It was supplemented with pgvector 0.8.6 and HypoPG 1.4.3. Do not substitute an existing
application container. The other tracks' exact environments are in their reports.

## Root fixture

```powershell
docker run -d --name pgsage-validate-runtime-20260926 `
  -p 127.0.0.1:55440:5432 -e POSTGRES_PASSWORD=validation-local-only `
  pg17-hintplan-clean:latest `
  -c shared_preload_libraries=pg_stat_statements,pg_hint_plan `
  -c wal_level=logical -c max_connections=300
docker exec pgsage-validate-runtime-20260926 sh -c `
  'apt-get update -qq && apt-get install -y postgresql-17-hypopg postgresql-17-pgvector'
docker exec pgsage-validate-runtime-20260926 pg_isready -U postgres
```

The password above is solely a disposable fixture value, not a discovered credential.

## Clean full baseline and staged retention probes

Windows toolchain used: Go 1.26.1. In `audit-repo/sidecar`:

```powershell
$env:SAGE_TEST_DATABASE_URL = 'postgres://postgres:validation-local-only@127.0.0.1:55440/postgres?sslmode=disable'
$env:SAGE_DATABASE_URL = $env:SAGE_TEST_DATABASE_URL
go test -cover '-covermode=atomic' `
  '-coverprofile=../../reports/evidence/preflight-unit.cover' `
  '-count=1' -json '-timeout=300s' ./...
```

In `validation-runtime-repo/sidecar`, using the same explicit test DSN:

```powershell
go test -cover '-count=1' -json '-timeout=180s' `
  '-run=TestPreflightRetention' ./cmd/pg_sage_sidecar ./internal/autonomy
```

Captured stdout is the corresponding `preflight-unit.jsonl` or
`preflight-retention.jsonl`; stderr and actual exit codes have separate saved files.
PowerShell coverage-profile flags are quoted to avoid its argument splitting behavior.

## Instrumented Linux subprocess tests

The staged change to e2e/smoke_test.go adds `go build -cover -covermode=atomic
-coverpkg=./...` only when SAGE_E2E_COVERAGE_DIR is set. It does not change any assertion.
Both existing smoke and fleet launch helpers already pass this directory as GOCOVERDIR.
Linux SIGINT allows a graceful exit and coverage flush, unlike a forced Windows kill.

Use a golang:1.25 container with the **whole** validation-runtime-repo mounted as /repo,
reports/evidence mounted writable as /evidence, and working directory /repo/sidecar.
Set these container environment variables:

```text
SAGE_TEST_DATABASE_URL=postgres://postgres:validation-local-only@host.docker.internal:55440/postgres?sslmode=disable
SAGE_E2E_COVERAGE_DIR=/evidence/preflight-runtime-cov
```

Inside that container, the executed commands were:

```sh
go test -cover -count=1 -json -tags=e2e -run 'TestSmoke|TestFleet' -timeout 600s ./e2e
go tool covdata textfmt -i=/evidence/preflight-runtime-cov -o=/evidence/preflight-binary.cover
go tool covdata percent -i=/evidence/preflight-runtime-cov
```

Create the coverage directory before execution. Docker Desktop's host.docker.internal
is necessary for this environment. Full source mounting is necessary for repository
contract tests that read documentation outside sidecar. Runtime coverage files are
retained, and the container exits after completing the commands.

## Real Windows metadata fleet and browser

In validation-runtime-repo/sidecar:

```powershell
go build -cover '-covermode=atomic' '-coverpkg=./...' `
  -o ../../reports/evidence/preflight-service.exe ./cmd/pg_sage_sidecar
```

Prerequisites: Python 3.11, Node 24, audit-repo/sidecar/web dependencies installed with
its lockfile, Playwright Chromium installed, free loopback ports 18089 and 19189, and
a fresh root fixture. The real service binary embeds its committed dashboard.

From the task workspace:

```powershell
python validation-runtime-repo/scripts/preflight_runtime.py
go tool covdata textfmt '-i=reports/evidence/preflight-runtime-cov-windows' `
  '-o=reports/evidence/preflight-meta-binary.cover'
python reports/evidence/preflight_summary.py
```

The staged Python script owns process startup, local fixture databases, authenticated
HTTP checks, browser launch, restart and removal. Browser credentials pass through stdin;
initial passwords in captured startup logs are redacted. The browser script uses actual
requests, not route interception. The final expected result is fourteen PASS checks.

Use a fresh fixture for each full replay; database names are intentionally fixed and
creation fails if they already exist. The initial failed harness artifacts remain in
preflight-runtime-attempt1. Final per-package coverage normalizes only five inspected
cross-toolchain source endpoints; the explicit mapping is saved next to the summary.

## Companion tracks and cleanup

Use each companion report's exact test selector and fixture port; their patches target
different isolated clones. Do not combine runs against shared application schemas.
Retain every nonzero exit, skip and fixture correction. After stopping owned processes,
remove only the named disposable containers and verify their absence. Never remove a
pre-existing application database/container because its name contains pg_sage.

Historical-state diagnostics are different from these destructive regression fixtures:
`evidence/preflight-state-diagnostics.sql` is explicitly read-only, with timeouts and
optional-table checks. Select the authoritative deployment and capture those results
before its future remediation; no live installation result is supplied by this audit.
