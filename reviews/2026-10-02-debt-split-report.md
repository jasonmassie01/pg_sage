# Engineering debt: split main.go, executor.go and router.go

Date: 2026-10-02. Branch `claude/debt-split` (worktree `pg_sage-split`), based on
`claude/dogfood-lifeos-1` @ `c612e9a`. Not pushed. Test database: my PG17 `pgsage-ag5`
(:55475); shared matrix PG14 (:55414) and PG18 (:55418) for the touched packages.

The work is a refactor that keeps behavior the same. Three files over the 500-line limit,
with eight functions over the 50-line limit between them, were split along their real
responsibilities inside their own packages. No exported identifier, route, config key or
API changed, and no new package was created.

## Before / after

| File | Lines before | Funcs > 50 before | Lines after | Funcs > 50 after |
|---|---:|---|---:|---:|
| `cmd/pg_sage_sidecar/main.go` | 1546 | `main` 183, `startAPIServer` 81, `handleMetrics` 55, `updateInstanceFindings` 53 | 361 | 0 |
| `internal/executor/executor.go` | 938 | none (longest 39) | 315 | 0 |
| `internal/api/router.go` | 693 | `registerAPIRoutes` 150, `NewRouterFullRuntime` 132, `registerActionRoutes` 95, `registerConfigRoutesRuntime` 70, `registerNotificationRoutes` 63 | 271 | 0 |

New files and their sizes. Every file is ≤ 500 lines, every function is ≤ 50 lines and every
line is ≤ 100 characters. I measured function length with a small `go/ast` counter
(declaration line through closing brace):

| Package | File | Lines | Longest function |
|---|---|---:|---|
| sidecar | `main.go` | 361 | `initStandalone` 41 |
| sidecar | `config_controller.go` | 127 | `applyWatchedConfig` 33 |
| sidecar | `shutdown.go` | 116 | `shutdownProcess` 38 |
| sidecar | `mode_init.go` | 164 | `initFleetMultiDB` 29 |
| sidecar | `alert_routes.go` | 55 | `buildAlertRoutes` 33 |
| sidecar | `api_server.go` | 110 | `startAuthPoolServices` 21 |
| sidecar | `agentdb_reconciler.go` | 68 | `startAgentDBReconciler` 27 |
| sidecar | `fleet_status.go` | 130 | `updateInstanceFindings` 37 |
| sidecar | `prometheus.go` | 262 | `handleMetrics` 40 |
| sidecar | `rate_limiter.go` | 223 | `buildTrustedProxyNets` 35 |
| sidecar | `startup_probes.go` | 127 | `detectCloudEnv` 37 |
| sidecar | `logging.go` | 35 | `logStructuredWrapper` 8 |
| executor | `executor.go` | 315 | `New` 23 |
| executor | `finding_policy.go` | 126 | `standingPolicyDecision` 33 |
| executor | `approval_proposal.go` | 77 | `buildApprovalProposalMetadata` 25 |
| executor | `action_classify.go` | 89 | `actionTypeForProposalSQL` 39 |
| executor | `executor_events.go` | 50 | `notifyPostDDL` 14 |
| executor | `action_guards.go` | 160 | `exceedsMaxRetries` 39 |
| executor | `action_record.go` | 112 | `snapshotBeforeState` 34 |
| executor | `index_names.go` | 61 | `extractIndexName` 30 |
| api | `router.go` | 271 | `NewRouterFullRuntime` 39 |
| api | `router_core_routes.go` | 216 | `registerProcessControlRoutes` 41 |
| api | `router_auth_config_routes.go` | 169 | `registerConfigRoutesRuntime` 41 |
| api | `router_action_routes.go` | 133 | `registerPendingActionRoutes` 35 |
| api | `router_notification_routes.go` | 107 | `registerNotificationChannelRoutes` 32 |

## Commits (oldest first)

| Commit | What |
|---|---|
| `849064c` | test(api): route table and middleware chain characterization |
| `e79d345` | test(executor): persistent action guard characterization |
| `ab66e12` | test(sidecar): main wiring per mode (subprocess) and thin-coverage helpers |
| `5946c5c` | test(api): router table golden recorded from the unmodified router |
| `e952a91` | test: fix two characterization test errors found on the unmodified code |
| `eb6fdc9` | refactor(api): split router.go by route area |
| `887093d` | refactor(executor): split executor.go by responsibility |
| `b8b1652` | test(sidecar): wait for listeners to accept before probing the child |
| `8623088` | refactor(sidecar): split main.go by responsibility |
| `8398709` | style(sidecar): wrap lines over 100 chars moved out of main.go |
| (this) | docs: report and CHANGELOG |

Each refactor commit message lists what moved where.

## How "no behavior change" was checked

1. **Tests first.** I added characterization tests and committed them before touching any
   production file, then confirmed they pass on the unmodified code (see "Test changes
   after the first commit" for the two corrections and one harness fix). They are:
   - `internal/api/router_table_test.go` (+ `router_table_probes_test.go`,
     `testdata/router_table.golden`). The probes cover all 102 literal route patterns in
     the package, plus constant-built routes, root/SPA paths and unregistered
     paths/methods. Each probe runs against four router configurations (bare, standalone
     with MCP and autonomy, fleet read-only config, store-only actions). The test records
     whether the route exists (404/405) and which gate guards it (none, viewer, operator,
     admin), probing as no user, then viewer, then operator. The pool is unreachable, so no
     handler can touch a database. 448 rows.
   - `internal/api/router_middleware_char_test.go`. It pins the middleware order: the JSON
     check runs before caller middlewares, the deadline applies everywhere except
     `/api/v1/events`, security headers and CORS preflight behave as before, `/health` and
     the SPA sit outside the API chain, and chatops callbacks skip session middleware and
     are registered only when a pool is present.
   - `internal/executor/executor_guards_char_test.go`. It pins the anti-oscillation
     window, outcomes and limit, the retry ceiling across earlier findings for the same
     identity, and `markFindingActioned` resolve/link/log. Coverage of these was 50–64%.
   - `cmd/pg_sage_sidecar/main_process_char_test.go` + `main_modes_char_test.go`. The
     test binary re-runs itself as the sidecar process and checks each mode: `--version`,
     invalid mode (exit 1), unreachable standalone DB (exit 1), standalone (all components
     start; `/health`, 401 on the API, `/metrics`; supervised `POST /api/v1/restart` exits
     42 after "stopped"), fleet (per-DB runtime, fleet gauges, SIGTERM exits 0 after
     "stopped") and meta mode (meta DB initialized, `/health`).
   - `cmd/pg_sage_sidecar/main_wiring_char_test.go`. It covers moved helpers that had thin
     coverage: `startAPIServer(nil)` refuses, Prometheus server timeouts and route, the
     standalone metrics sections (previously 0%), the shutdown drains, trusted-proxy
     parsing and `envOrDefault`.
2. **Verbatim moves.** The executor split moves lines verbatim: a line-multiset comparison
   of old against new, ignoring import blocks, shows zero added and zero removed lines. In
   main.go the only differing lines are the rewritten `main` and the helpers extracted
   from the four long functions. The same statements run in the same order; in
   particular, the meta and monitored pool `Close` calls are still deferred in `main`, and
   the restart `os.Exit` still skips them as before.
3. **Mutation checks.** Each of these deliberate breaks made the new tests fail, and each
   was reverted afterwards:
   - emergency-stop gate changed from operator to admin (route table: 4 rows);
   - security-headers middleware dropped (2 chain tests);
   - the restart exit disabled (standalone process test: exit 0, want 42);
   - fleet init skipped (fleet process test);
   - `oscillationLimit` 3 → 4 (oscillation test).

## Product calls / decisions

- **`initStandalone` and `startAPIServer` stay in `main.go`.** The existing
  `integration_wiring_test.go` looks for them in `main.go` by file name. It also expects
  the `WireParams` literal inside `startAPIServer`. So `startAPIServer` keeps that literal
  and hands off to `configureAPIProcessHooks`, `startAuthPoolServices` and `serveAPI`
  (`api_server.go`). I did not edit that existing test.
- **One small visible change.** `handleMetrics` ignored the `fmt.Fprint` error on the
  `/metrics` response. That was hidden by the lint exclusion for `main.go`, and now that
  the code lives in `prometheus.go`, errcheck flags it. A failed write is now logged at WARN
  (`[prometheus] write /metrics response: ...`). This follows the rule against swallowing
  errors, and it is the only runtime difference. It is noted in the CHANGELOG.
- **Lines over 100 characters.** These came from `main.go`, which errcheck/lint never held
  to the limit. They were wrapped in a separate `style` commit so the move commit stays a
  pure move. Constant strings were split with `+`, so the values are identical, including
  the SQL text. Wrapping took `writeStandaloneMetrics` to 52 lines, so its LLM gauges moved
  verbatim into `writeLLMMetrics`.
- **Moved doc comment.** `resolveExecutionMode`'s doc comment had been attached to
  `silenceSelfStats`, and it now sits on its own function.

## Test changes after the first commit (all before or outside the refactor)

- `e952a91` (before any refactor). Two of the new tests were wrong against the unmodified
  code:
  - `sage.findings` allows only one open finding per identity (`idx_findings_dedup`), so
    the earlier finding is now inserted as resolved.
  - `drainRuntimeWorkers` shares one deadline across runtimes, so map order decides which
    other runtimes get reported. The test now asserts only the stuck runtime.
  - I also wrapped one test line that was over 100 characters.
- `b8b1652` (during the main split; harness only). The process test raced the sidecar:
  "listening on" is logged just before `ListenAndServe` binds, and one fleet run got
  connection refused. The harness now dials until the port accepts, and no assertions
  changed. I verified this commit against the unmodified `main.go` (1546 lines) three
  times in a temporary worktree, all green, then removed the worktree.

## Test Results

**Command:** `go build ./... && go vet ./... && go test -p 4 -timeout 30m -count=1 -cover -v ./...`
from `sidecar/` in `golang:1.25`, repo root mounted. The final run shared ag5's network
namespace (`--network container:pgsage-ag5`, DSN `127.0.0.1:5432`), because two earlier
runs through `host.docker.internal` hit dial and fixture-drop timeouts under host load
(details below).

**Total (final full run, PG17):** 10770 passed, 1 failed, 20 skipped. 80 packages ok,
1 failed. Build and vet are clean.

| Run | Touched packages | Result |
|---|---|---|
| PG17 full suite (final) | all | 10770 pass / 1 fail (`internal/rca`, untouched, see below) / 20 skip |
| PG14 (`--network container:pgsage-matrix-14`) | sidecar, api, executor | all ok, 0 skip |
| PG18 (`--network container:pgsage-matrix-18`) | sidecar, api, executor | all ok, 0 skip |
| `-race` on PG17 | sidecar, api, executor | all ok, no data race |
| golangci-lint (Windows) | `./...` | 0 issues |

**Coverage (touched packages, before → after):**

- `cmd/pg_sage_sidecar`: 75.2% → 81.2%
- `internal/api`: 76.1% → 77.6%
- `internal/executor`: 83.9% → 84.1%

All three are business packages and are above 70%. All packages meet their thresholds.

### Skipped Tests (20; none in touched packages; all pre-existing and gated by environment)

- agentdb (6): live AWS RDS, Cloud SQL and Lakebase provisioning, plus the 3 live gauntlet
  chains. They need `PG_SAGE_LIVE_*`.
- azure (2): live Azure parameter tests. They need `PG_SAGE_LIVE_AZURE`.
- llm (2) and the tier-2 Gemini test (1): live LLM tests. They need `PG_SAGE_LIVE_LLM`.
- ha (1) and the identity probes (2): need disposable standby or restartable servers
  (`SAGE_TEST_*_URL`).
- pooler (3): need PgBouncer URLs.
- logwatch (1): Windows absolute-path case on unix.
- rca (1): `TestRCAChildProcessFixture`, a child helper that runs only under its parent.
- bench (1): needs `SAGE_BENCH_RUN=1`.

### Failures

- `internal/rca` `TestReconcile_LargeBacklogInBatches`: the reconcile took 2m44s under
  full-suite host load (`lifeos_postgres` was at about 98% CPU). The package is untouched.
  Rerun alone: **ok** (10.6s).
- Earlier full runs (environment, not code):
  - Through `host.docker.internal`, 4 tests in `cmd/pg_sage_sidecar`, `internal/sre` and
    `internal/value` failed with `dial error: timeout` creating or connecting fixture
    databases. Under the netns run, one cmd package hit the 10-minute default timeout and
    two tests timed out dropping fixture databases.
  - Every one of those packages passed on rerun. The touched packages also passed in the
    final full run and on PG14 and PG18.

### Coverage Gaps

None below threshold. `main()`, `startPrometheusServer`'s listener goroutine and
`poolHealthCheck`'s ticker are covered only by the subprocess tests. Those tests do not
count toward `-cover`, because the child does not write a profile.

### Bugs Found This Session

1. [NOTE] `actionTypeForProposalSQL` (`action_classify.go`) has a duplicate
   `PG_CANCEL_BACKEND` case. The second case is dead. It was moved verbatim and not
   changed, because this is a refactor that keeps behavior the same.
2. [NOTE] The `/metrics` write error was ignored, hidden by the `main.go` errcheck
   exclusion. It is now logged (see Product calls).
3. [NOTE] The `.golangci.yml` exclusion for `cmd/pg_sage_sidecar/main.go` (errcheck,
   gosimple) now covers far less code. The coordinator may want to drop it.

### Post-test audit

- **Untested inputs:**
  - Route probes use `{}` bodies and placeholder path values, so handler-level validation
    is not part of the golden. That is on purpose, because only registration and gating
    are characterized.
  - Under OAuth enabled, the router config is not probed. Discovery needs a live issuer,
    and the new `newRouterOAuthProvider` keeps the same branches.
- **Assertions that could pass while broken:**
  - The golden records `open:<status>` for ungated routes. A change that swapped which
    handler serves an open route with the same status would not show.
  - Gated routes are the risk that matters, and the mutation checks show gate changes are
    caught.
- **Fakes hiding failures:**
  - The router tests use an unreachable pool by design, so no SQL runs.
  - The executor guard tests and the subprocess mode tests run against real PostgreSQL
    (fixture databases), including schema bootstrap, admin bootstrap, login and restart.

## Spec CHECKs

None. This is an engineering-debt refactor with no AI-SRE behavior change.

## Left for the coordinator

- The `.golangci.yml` errcheck/gosimple exclusion for `cmd/pg_sage_sidecar/main.go` can
  probably be removed, or narrowed, now that main.go is 361 lines.
- `integration_wiring_test.go` pins functions to `main.go` by file name.
  `productionFunction` could search the whole package so future moves do not need to
  keep functions in place.
- Merge-conflict risk: other agents add wiring to `main.go` and `router.go`. Their hunks
  will now conflict with the splits. When resolving, put the new code in the matching new
  file: route areas in `router_*_routes.go`, startup in `main.go` helpers, metrics in
  `prometheus.go`.
- On this host, running the full suite through `host.docker.internal` under load is
  flaky: dial timeouts happen. `--network container:<db>` avoided the dial failures.
