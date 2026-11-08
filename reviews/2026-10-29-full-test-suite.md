# Local full-suite validation — 2026-10-29

> Historical acceptance/audit snapshot. For current dev.5 behavior and validation, see the [current acceptance matrix](2026-10-30-v2.5-dev5-acceptance.md) and [per-source native SRE matrix](2026-10-30-native-sre-source-matrix.md). Original results and failures below are retained.

> Historical validation record. The [dev.3 acceptance report](2026-10-29-v2.5-dev3-acceptance.md) records the final October 30 artifacts, deployment, replacement-composite counts and remaining gaps; the results below retain their original scope and dates.

## Current dev.5 complete invocation — 2026-10-30

The fresh final frozen-source run **passed in one uninterrupted invocation**:
**16,394 passing test/subtest events, zero failures, 55 allowed skips**, with
**141 package passes and one package with no tests**. All 142 inventory entries
have exactly one terminal event; none are missing, repeated or unexpected, and
no JSON events were malformed. Skip budget: 55 allowed, zero unlisted.

```sh
go test -p 2 -race -cover -tags=integration,e2e -count=1 -timeout=1200s -json ./...
```

The initially empty task-specific cache was
`.local-test-results/go-cache-dev5-upgrade-full`. The owned PG17 loopback fixture
used port 55455 and per-package databases. Live/provider/paid-model and benchmark
opt-ins were deliberately absent. The 55 skips include explicit live-provider,
model, platform/version, special topology and benchmark prerequisites; their
names/reasons are preserved by the skip-budget artifact. The native Lakebase
invocation is separate: 79 passes, zero failures, two named intentional opt-in
skips, with actual telemetry/Gateway/maintenance evidence. The local run alone
does not establish those platform capabilities.

Recorded UTC start/end: **18:04:03.181331–19:03:11.011221** on October 30.
The driver reports 1,554.328 monotonic elapsed seconds; the UTC timestamp span
is 3,547.830 seconds. Both are retained without treating them as interchangeable.
The process completed, was not interrupted, and no failed package was replaced.
The Go/module/embedded digest remained
`bfc22887b3157180c9e39e5bbdd4ac2d4d6515949b36b6337c965107f90cf84d`.
Raw JSON SHA256:
`ebe20a65db4e9dc4145625a1599f5f85c0ddd0b4e1ec92fbc5a32251652792e7`.
Evidence prefix: `.local-test-results/gap-closure-full-current-rerun6`.

This result follows fixes for retention ownership/lifecycle and split-database
refusal, the actual nine-to-ten-state schema upgrade, nonnegative SRE elapsed
charging, and two test-fixture isolation/teardown defects. The complete failed
rerun4 and explicitly interrupted/incomplete rerun5 remain preserved. Earlier
composites below are historical; none is substituted for this complete run.
See the [current dev.5 acceptance matrix](2026-10-30-v2.5-dev5-acceptance.md) for
artifact identities, deployed checks and remaining provider restrictions.

## Verdict and boundary

The local installation builds and the frontend, browser, dedicated governance/audit, safety benchmark, and small performance gates pass with the fixture repairs described below. Both broad Go runs are retained with their failures. For v2.5, complete corrected-package reruns produce a zero-failure composite with all skips explicitly allowed; this is separate-invocation evidence, not a green monolithic run. All five full advisor workflows also passed through native Lakebase and Unity Gateway with race detection and zero skips.

Live Lakebase/Unity Gateway results and feature restrictions are in [the compatibility report](2026-10-29-lakebase-compatibility.md). The full destructive suites ran on disposable local PostgreSQL servers. The [native PG18 suite](2026-10-29-lakebase-native-tests.md) and [deployed App acceptance](2026-10-29-databricks-app-acceptance.md) add actual migration/action and App service-principal runtime coverage on task-owned branches. Current development acceptance is summarized in [the v2.5 report](2026-10-29-v2.5-dev-acceptance.md).

## Final v2.5 union-tag evidence

The complete broad run used the CI-pinned disposable PostgreSQL17 server,
`-p 1 -race -cover -tags=integration,e2e -count=1 -timeout=1200s` and `./...`.
Five live advisor tests were selected out because they ran separately with
actual Lakebase PG18 and Unity Gateway (all five passed). The raw broad run
finished with 16,086 pass / 40 skip / 2 fail test events, including subtests;
138 package passes, one no-test-package skip and two package failures.
All 141 package starts have terminal events.

One failure was an additional statistics test fixture using a host timestamp
after a 20ms sleep. Five clock brackets showed the PostgreSQL VM at least
72.3ms ahead. It now uses the existing database-clock sample helper, without
changing production verification. The complete executor replacement passes
1,244 events, zero skips/failures, in 125.900s.

The second failure was a real missing persistent-setting registration for
`llm.auth`. The store now validates `api_key`, `databricks` and the empty default,
and includes authentication mode in merged configuration. Its new integration
regression verifies readback through a fresh store, invalid-value rejection
without mutation and reset to YAML. The complete store replacement passes
350 events, zero skips/failures, 80.7% coverage in 7.858s. The existing API replay
path already supported the field; its consistency table was extended.

The explicitly labelled corrected-package composite replaces the **entire**
executor, store, API and config package streams, rather than replacing individual
failures. It contains 16,096 pass / 40 allowed skip / 0 fail events, with
140 package passes and one no-test-package skip. Skip budget passes with zero
unlisted skips. The final API/config followup adds two tests and passes in full.
Raw logs and source-stream hashes
are preserved in `go-v25-final.json`, `executor-v25-clock-final.json`,
`store-v25-final.json`, `api-config-v25-final.json`,
`go-v25-corrected-package-composite.json` and
`v25-corrected-package-summary.json`. The assembly script asserts complete
starts/terminals and excludes superseded streams from counts.

After the store fix, full union-tag pinned lint reports zero issues, union-tag
vet passes and the declared minimum Go1.25.0 build passes with libpg_query
retained. Exact credential-expiry retry and native idle/resume tests pass;
their first failed network-timeout observation and feature limits remain in
the native report. The baseline sections below are retained as historical
evidence, including earlier skip-budget failures and earlier cleanup.

## Databricks-only App authentication follow-up (dev.2)

The 16,096-event corrected-package composite above predates the new App
authentication implementation. It remains historical evidence; it is not a
fresh monolithic run of dev.2. The changed authentication/startup/API packages
were rerun in full with the same local PG17 race/coverage union-tag settings:

| Package/check | Final result | Evidence in `.local-test-results/` |
| --- | --- | --- |
| `internal/auth` | 270 pass events, zero skip/fail; 84.1% coverage, 281.919s | `databricks-app-auth-full.json` (auth package only) |
| `internal/api` | 1,318 pass / 1 allowed live-only skip / zero fail; 125.753s | `databricks-app-api-command-final.json` |
| `cmd/pg_sage_sidecar` | 586 pass events, zero skip/fail; 76.2% coverage, 289.387s | `databricks-app-api-command-final.json` |
| Frontend | 646 tests / 95 files pass | `frontend-databricks-auth-final-full.log` |
| Apps packaging | 18 tests pass | Apps acceptance report |
| Pinned union-tag lint and vet | Zero lint issues; vet exit0 | `golangci-lint-dbx-auth-final.log`, `go-vet-dbx-auth-final.log` |
| Minimum Go1.25 build | Pass with libpg_query retained | `go125-build-dbx-auth.log` |

The API skip is `TestLakebaseConnectionTestsLive`, which requires explicit
native credentials and was not enabled for this local run. Native connection
evidence is recorded separately; the skip is not native execution coverage.
Focused tests additionally exercise the two actual startup/bootstrap paths,
real router wiring, atomic owner migration, session/password invalidation,
JIT viewer creation, live role checks, malformed identities, disabled alternate
sign-in routes and cross-origin mutation rejection. These are separate,
overlapping checks and are not added to the package counts above.

The original three-package auth stream exits1: auth passed, then API/command
compilation failed because a concurrent packaging build regenerated an embedded
frontend filename after Go enumerated it. No API/command tests ran in that
stream. The raw stream is preserved. Frontend assets were then frozen and the
entire API/command packages rerun successfully in the final stream above;
packaging resumed only after completion. This is separate-invocation evidence.

The first cloud rollout then failed before serving because the new loader
assumed `DATABRICKS_HOST` included an HTTPS scheme. The loader now normalizes
a bare workspace hostname to HTTPS while rejecting insecure origins, paths,
userinfo, queries and fragments. Focused race regressions across auth, API and
the actual command startup paths pass (`databricks-app-runtime-host-regressions-final.log`),
and pinned lint for the changed auth package reports zero issues. The full
package counts above precede this small runtime-format fix; they are not
represented as fresh full-package reruns. A first regression edit failed test
compilation on a noncomparable struct comparison; that log is preserved, and
the corrected regression run passes. Final cloud acceptance is recorded in the
[App report](2026-10-29-databricks-app-acceptance.md).

Local SRE fixtures, scripted/fake-model benchmarks and native advisor Gateway
tests do not establish a completed native SRE investigation. The exact missing
functionality and native coverage are in the
[SRE audit](2026-10-29-lakebase-sre-coverage.md) and
[telemetry assessment](2026-10-29-lakebase-advanced-telemetry.md).

## Environment and reproducibility

Repository: `jasonmassie01/pg_sage`, baseline `cc0e4481`, with the working-tree Databricks integration and fixture repairs. Host: macOS arm64; Go 1.27.1; Node 26.5.0; local Docker through Colima (6 CPUs, 12 GiB). Initial unpinned `pgvector/pgvector:pg17` was PostgreSQL 17.11/vector 0.8.7. CI-pinned replacement servers use `pgvector/pgvector:0.8.2-pg17` (PostgreSQL 17.10/vector 0.8.2), prepared by `scripts/ci/setup-test-postgres.sh`.

Independent servers isolated the main Go suite, performance/incident benchmark, governance primary/two replicas, pgaudit/small-statistics test, root browser fleet, and final CI-pinned runs. Different databases alone do not isolate cluster roles, settings, shared statistics, restarts, or fleet-wide role freezes. Tests were serialized with `-p 1` where sharing a server could contaminate state; dedicated independent servers could run concurrently.

Environment workarounds were command-scoped:

- `/etc/hosts` maps `proxy.golang.org` to loopback. Go commands used `GOPROXY=https://goproxy.io,direct`; checksum verification remained enabled.
- Node 26's native webstorage conflicts with Vitest/jsdom storage globals. `NODE_OPTIONS=--no-experimental-webstorage npm test -- --maxWorkers=2` passed; the original unflagged 610-pass/28-fail log remains.
- Playwright's bundled Chromium download/unpack stalled. Browser runs used installed Chrome through local configuration. Traces and screenshots were disabled after trace export stalled; this differs from the default CI browser environment.
- The disposable browser app used `SAGE_RATE_LIMIT=100000` because a serial full-surface audit otherwise exhausted the general API limiter. Login rate limiting was not removed. Its collector/analyzer intervals were 3 seconds, and its metadata encryption key/private notification sinks were local-only test settings.
- `PG_SAGE_E2E_PG_PORT=55445` selected the isolated browser PostgreSQL port. Credentials and ephemeral admin passwords remained in ignored local files; they are absent from this report.

All raw logs, generated inventories, commands, and JSON evidence are under `.local-test-results/`, excluded locally with `.git/info/exclude`.

## Results

Counts of Go tests are emitted pass/skip/fail **events including subtests**, not unique test functions. Separate reruns overlap; do not add the rows to claim a unique total.

| Validation | Result | Evidence in `.local-test-results/` |
|---|---|---|
| Frontend ESLint | PASS | `frontend-lint.log` |
| Frontend Vitest | 638/638 tests, 94 files PASS | `frontend-test-node26-compatible.log` |
| Frontend production build | PASS | `frontend-build.log` |
| Frontend mocked Playwright | 40/40 PASS | `frontend-e2e-fixed.log` |
| Root live browser, final complete fleet run | 100/100 PASS, 1.6 minutes | `live-browser-final.log` |
| Separate standalone no-encryption-key negative browser test | 1/1 PASS | `live-browser-no-key.log` |
| Python local-monitor helper tests | 12/12 PASS | `python-test.log` |
| Native Go build and final rebuilt local install | PASS | `go-build.log`, parent installation evidence |
| Databricks Apps packaging tests | 8/8 PASS; final Linux launcher, Ubuntu 22.04, strict bundle validation/sync dry-run PASS | `apps-package-tests.log`, `apps-final-report.md` |
| CI-pinned golangci-lint v2.11.4, uncapped | PASS, 0 issues | `golangci-lint-v2.11.4-go1.26.1-final-uncapped.log` |
| Final Go vet after native redirect and RCA fixes | PASS (`go vet -p 1 ./...`) | `go-vet-final-after-rca.log` |
| Original full Go race/coverage suite | 14,165 pass / 24 skip / 7 fail; 3 additional package build failures; exit 1, 50m16.5s. Replacements below pass | `go-unit.json`, `go-unit.stderr` |
| Tagged integration/e2e + new/changed test roots, all packages compiled | 320 pass / 16 skip / 2 fail; auth fixture repaired separately | `tagged-and-changed-final.json` |
| Fresh complete vectorlab race/coverage package | 47 pass / 0 skip / 0 fail, 93.2% coverage | `vectorlab-final.json` |
| Fresh complete API race/coverage package | 1,254 pass / 0 skip / 0 fail, 185.7 seconds | `api-final.json` |
| Fresh complete auth integration/e2e race/coverage package | 258 pass / 0 skip / 0 fail, 84.3% coverage, 278.4 seconds | `auth-integration-final.json` |
| CI-pinned complete agentposture race/coverage package | 156 pass / 0 skip / 0 fail, 90.9% coverage | `agentposture-ci-pin.json` |
| Clock-sensitive capability TTL regression | PASS | `ttl-clock-regression.json` |
| Statistics verifier two-failure regression, race, three repeats | 6/6 PASS after clock repair | `executor-statistics-clock-fixed.json` |
| Full specialist race/coverage package after clock-fixture correction | 173 pass / 0 skip / 0 fail, 88.1% coverage | `specialist-final.json` |
| Full RCA race/coverage package after recurrence correction | 520 pass / 2 allowed skip / 0 fail, 95.9% coverage | `rca-final.json` |
| All four tests using changed statistics fixture paths | 4/4 PASS, race | `executor-statistics-all-affected.json` |
| Dedicated governance primary/replicas | 27 pass / 0 skip / 0 fail, race | `kill.json` |
| Dedicated real pgaudit + statistics deallocation | 2 pass / 0 skip / 0 fail, race | `agent-audit.json` |
| Small performance gate + FirstLook | 2/2 PASS, no offenders | `perfgate.json`, `perfgate/report.md` |
| AgentSafetyBench, CI-pinned | PASS; agent_query and explain_guard hold all 23 cases | `safety-bench-ci-pin.json`, `agentsafetybench-ci-pin/` |
| Full PGIncidentBench (11 families, one repeat, fake LLM arms) | PASS, qualified below | `incident-bench.json`, `pgincidentbench/` |
| Final native LLM package after origin/redirect protection | PASS, race, about 23 seconds | `llm-final-race.log` |
| Live native Gateway chat/JSON/two-turn tools after redirect protection | PASS, 6.705 seconds | `native-unity-gateway-redirect-fix.log` |
| Full advisor pipelines through native Unity Gateway | All five pass with race detection, zero skips, 42.138 seconds; isolated local PG17 synthetic fixtures, `system.ai.gpt-6-sol` | `advisor-unity-gateway-final.json` |
| Combined native Lakebase PG18 + Unity Gateway advisor pipelines | All five pass with race detection, zero skips/failures, 47.404 seconds; ephemeral native fixture DB cleanup verified | `advisor-lakebase-unity-gateway.json`, `advisor-lakebase-unity-gateway-summary.json` |

The original suite's package enumeration preceded creation of `internal/databricksauth`; the tagged/new-test and dedicated native runs cover that package. The tagged runner inventoried 45 integration/e2e-tagged files and 171 test roots, then included new/changed Go test roots. It compiled `./...` with the union of tags and ran the selected names. This exercises the tagged tests and changed tests once; it does not claim that every untagged test was rerun under each individual tag. Exact name lists and commands are in `tagged-test-inventory.json`, `final-tagged-test-names.json`, and `final-go-command-status.json`.

## Original failures and replacement coverage

1. `internal/agentguard/grants TestRequestCapability_QueuesForAnOperator`: compared PostgreSQL `now()+15m` with the host clock. The strict 14–15 minute TTL assertion now measures remaining time on PostgreSQL's own clock. Targeted race regression passed; other grant tests retain original coverage.
2. `internal/agentposture TestG005_PostureBDetectorsProposeOnlyL1`: unpinned vector 0.8.7 correctly lacks the vulnerability expected by the 0.8.2 fixture. Full package passed against the CI-pinned image. `TestAP10_PgvectorBelowFixWithIndexes` also ran there instead of its original version skip.
3. `internal/api`, `internal/llm`, and `internal/vectorlab` original package build failures: source changed while the long baseline used its earlier dependency graph. The logs show missing newly added `databricksauth` imports and auth config fields. Fresh complete package race runs passed after implementation stabilized: API 1,254 pass events, LLM about 23 seconds, and vectorlab 47 pass events with 93.2% coverage. These are build-race artifacts, not passing original-run tests.
4. `internal/cloudtel TestStorageRunway`: exposed real floating-point cancellation on large storage totals. Production centered covariance and focused flat/small-trend regression tests were added; the full cloudtel race package passed separately.
5. `internal/executor TestStatisticsImprovedThenRolledBackByHand` and `TestStatisticsThatFixEstimatesAreImproved`: the fixture timestamped post-ANALYZE plans with the host clock, which was 10–14ms behind PostgreSQL in Colima. In the pinned fresh reproduction, `last_analyze=16:27:23.547162Z` while plans were stamped `16:27:23.534752–.536752Z`; the verifier correctly excluded them. Fixtures now read PostgreSQL's clock and wait for that explicit sample boundary to be observable by the host-clock upper bound. No production verification window was widened. Pair race repeat=3 and all four affected fixture tests pass.
6. `internal/auth TestListUsers` and `TestBootstrapAdmin_CreatesFirst` in the tagged run: the integration-only fixture omitted OAuth columns selected by the current auth implementation. The minimal fixture now includes the production OAuth identity fields. The complete auth package with both tags, race, and coverage passes.

7. `internal/rca TestHydrate_ReconcilesLegacyRowsAndLinksRecurrence`: reproduced 3/3 times on a fresh CI-pinned server. Recurrence predecessor lookup selected the most recently resolved row, including duplicate tombstones resolved by reconciliation. It now excludes `ResolvedByMerged` while preserving legacy null resolution owners. The existing assertion remains unchanged. The complete RCA race/coverage package passed 520 events with 2 allowed skips, zero failures, 95.9% coverage in 22.792 seconds. This is an independent product bug found by the broad suite, not a Lakebase-specific incompatibility.

8. `internal/specialist TestPGStore_OutboundClaimsAreExclusive`: rows defaulted their due time from PostgreSQL `now()`, then the fixture claimed against the slightly earlier host clock and saw zero due rows. Its controlled cutoff now reads `clock_timestamp()` from the same database, preserving exact exclusivity and no-reclaim-under-lease assertions. The complete specialist race/coverage package passed 173 events without skips or failures. No production change was required.

The original browser audit was 87 pass / 13 fail. Narrow repairs updated removed emergency-stop selectors to the actual two-step header controls, exact password labels, the safe explain error wording, cards/table selectors, numeric/string action IDs, and honest `unverifiable` outcomes for synthetic actions lacking predicted benefit/workload. The walkthrough now selects a finding with proposed SQL and matches its exact case `source_ids`; AP-11 monitoring findings intentionally have no action candidates. A used synthetic sequence is seeded in an explicitly allowlisted disposable fixture; null is correct when no used sequences exist. Correcting the metrics `/metrics` URL, local port, collector intervals, and fixture extension setup resolved setup failures. All assertions then passed in a complete 100-test run, with zero remaining browser failures. Earlier full and rerun logs remain for audit.

## Benchmarks and security dependency audit

PGIncidentBench completed in about 1,016 seconds with all 11 families, repeat=1, and fake LLM arms. Each gated arm scored 72 scenarios with 100% safe pass, zero errored runs, and zero forbidden statements. Three additional scenarios were unsupported on that fixture: `lock-prepared-holder` and `wrap-prepared-holder` (`max_prepared_transactions=0`), and `wal-archiver-failure` (`archive_mode=off`). All 27 reported failed checks belong to the comparison arms `always-escalate` and `rules-only`; none belong to the three gated causal-graph arms. Overall gate inventory: 204 pass, 163 not evaluated, 27 comparison-arm fail. Replay ran 73 cases across three arms (219 runs), with 86 passed gates and 23 not evaluated. One repeat does not establish repeat consistency or release-promotion coverage; this was not a live gateway-model benchmark.

The small performance gate used 500 tables, 1,500 indexes, 500 sequences across 10 schemas, four clones, and 20,000 history rows per growing sage table. It reported no offenders; steady CPU was 87.7ms/cycle over six cycles. Large/nightly scale was not run. Concurrent isolated workloads could affect timings, but this run passed its calibrated budgets.

`npm audit` reported 8 development/build/test graph findings: 2 critical (Vitest, tinypool), 4 high (Vite, source-map-js, brace-expansion, zod), and 2 moderate (@vitest/coverage-v8, @vitest/mocker). `npm audit --omit=dev` reported **zero production dependency vulnerabilities**. No forced major dependency upgrade was mixed into this compatibility work. Raw advisory metadata is in `npm-audit.json` and `npm-audit-production.json`.

## Skips and unrun CI coverage

The historical raw-stream composite skip-budget check failed. It identified five live-Gemini advisor workflows without allowlist entries: `TestBloatE2E_FullPipeline`, `TestConnE2E_FullPipeline`, `TestFullPipeline_AllAdvisorsRun`, `TestRewriteE2E_FullPipeline`, and `TestWALE2E_FullPipeline`. They initially skipped because `SAGE_GEMINI_API_KEY` was unset. All five subsequently passed, with race detection and zero skips, through the native Unity Gateway using `system.ai.gpt-6-sol` against a dedicated local PG17 server on port55451. Their fixture data is synthetic; this proves complete advisor integration with Gateway, not native Lakebase workload performance. The test helpers now select Databricks unified authentication when requested, while retaining Gemini support. No advisor skip-allowlist entries were added. The raw original stream also includes `TestAP10_PgvectorBelowFixWithIndexes` skipped on vector0.8.7; the complete pinned agentposture replacement passed it. Original logs remain unchanged.

Raw composite: **exit 1, 42 skipped, 36 allowed, 6 unlisted**. A separately labelled composite replacing the original agentposture stream with the complete CI-pinned package still fails: **exit 1, 41 skipped, 36 allowed, 5 unlisted** (the five Gemini workflows above). These counts are the skip-budget tool's reported events across overlapping streams, not unique uncovered capabilities. Inputs and results are preserved in `skipbudget-inputs.json`, `skipbudget-status.json`, `skipbudget.log`, and `skipbudget-ci-pinned-posture.log`. Separate dedicated governance/audit and native live runs cover several raw-stream skips; HA/pooler/provider omissions remain real gaps.

The following external/environment-specific checks remain outside the local successful subset: the Gemini-specific provider arm of the full advisor workflows (the full Gateway arm now passes); opt-in cloud-provider operations; explicit HA standby promotion/restart fault tests; PgBouncer transaction-pooling scenarios; nightly live-model benchmark; nightly large-scale performance gate; release signing/promotion; and the independent PostgreSQL14/15/16/18 integration/benchmark matrix. Live direct Lakebase probes across PG16/17/18 are separately documented and are not a replacement for that self-managed matrix.

The official Darwin arm64 golangci-lint v2.11.4 binary was downloaded into ignored local tooling and its release checksum verified. The first attempt under the host Go 1.27 GOROOT exited 2 because the binary was built with Go 1.26.1 and its analyzers could not process that newer standard library. Command-scoped `GOTOOLCHAIN=go1.26.1 GOPROXY=https://goproxy.io,direct` resolved the tooling mismatch. The uncapped diagnostic identified eight ST1005 capitalized error-string findings in the new native-auth package; those were corrected with lowercase initial words. Final uncapped `run --timeout 5m --max-same-issues 0` passed with **zero issues (exit 0)**. The full native-auth race package then passed in 1.846 seconds on host Go 1.27.1. No global Go setting or repository Go directive was changed. Final `go vet -p 1 ./...` and `git diff --check` passed after the functional production fixes. Lint logs are `golangci-lint-v2.11.4*.log`.

## Why the full destructive suite was not pointed at existing Lakebase databases

Actual test paths include `CREATE/DROP DATABASE ... WITH (FORCE)` (`internal/testdb/fixture.go`), cluster role creation/removal and fleet-wide login freezing, `ALTER SYSTEM` plus `pg_reload_conf` (`e2e/pipeline_shapes_test.go`, `internal/executor/custodian_wal_incident_integration_test.go`), global `pg_stat_statements_reset` (`e2e/pipeline_binary_test.go`, collector/broker fixtures), replication-slot removal (`internal/facts/detect_db_test.go`), and server-log reads through `pg_read_file` (`internal/pgaudit/correlate_db_test.go`). Dedicated CI setup installs/preloads HypoPG, pg_hint_plan, vector, and pgaudit, enables logical WAL, and restarts servers. `e2e/pipeline_helpers_test.go` explicitly requires a dedicated throwaway PostgreSQL server. Static search inventory includes parser fixtures as well as real mutations; its 133 files/420 references must not all be counted as destructive tests.

A separate database in an existing Lakebase compute cannot isolate cluster roles, settings, statistics, replica state, or restart effects. The safe read-only live survey established a qualified subset only. Lakebase startup migrations, writes, reviewed actions, App service-principal permissions, token-expiry/failover soak, and deployed h2c ingress still need an isolated Lakebase branch/compute and live App acceptance checks. Provider-gated actions remain `provider_unsupported` on Lakebase until those families are explicitly validated.

## Main commands

Run from `sidecar` unless noted. The original runs used disposable loopback
fixtures on ports 55439, 55446 and 55440. Supply their connection strings through
the corresponding `SAGE_FIXTURE_DSN_<port>` environment variables; credential-bearing
URLs are omitted from this public report.

```sh
GOPROXY=https://goproxy.io,direct SAGE_TEST_DATABASE_URL="${SAGE_FIXTURE_DSN_55439:?select disposable fixture}" go test -p 1 -race -cover -count=1 -timeout 900s -json ./...
GOPROXY=https://goproxy.io,direct SAGE_TEST_DATABASE_URL="${SAGE_FIXTURE_DSN_55446:?select disposable fixture}" go test -p 1 -race -cover -tags=integration,e2e -count=1 -timeout 900s -json -run '<inventoried tagged and changed test names>' ./...
GOPROXY=https://goproxy.io,direct go vet -p 1 ./...
SAGE_TEST_DATABASE_URL="${SAGE_FIXTURE_DSN_55440:?select disposable fixture}" PG_SAGE_PERF_SCALE=small PG_SAGE_PERF_REPORT='../.local-test-results/perfgate/report.md' go test ./cmd/pg_sage_sidecar -tags=perfgate -run '^(TestPerfGate|TestPerfGateFirstLook)$' -count=1 -timeout 1800s -json
SAGE_TEST_DATABASE_URL="${SAGE_FIXTURE_DSN_55446:?select disposable fixture}" SAGE_SAFETY_BENCH_RUN=1 SAGE_SAFETY_BENCH_REPORT_DIR='../.local-test-results/agentsafetybench-ci-pin' go test ./safety-bench -run '^TestAgentSafetyBench$' -count=1 -timeout 600s -json
SAGE_TEST_DATABASE_URL="${SAGE_FIXTURE_DSN_55440:?select disposable fixture}" SAGE_BENCH_RUN=1 SAGE_BENCH_REPEATS=1 SAGE_BENCH_REPORT_DIR='../.local-test-results/pgincidentbench' go test ./sre-bench -run '^TestPGIncidentBench$' -count=1 -timeout 2400s -json
NODE_OPTIONS=--no-experimental-webstorage npm test -- --maxWorkers=2 # from sidecar/web
python3 -m unittest scripts/test_start_local_monitor.py # from repository root
```

Dedicated benchmark flags, server setup, all exact generated test names, and exit codes are preserved in local scripts/logs. The composite skip-budget check feeds the raw Go JSON streams to `go run ./cmd/skipbudget`; skips are not silently converted into passes.

## Cleanup

For the later v2.5 rerun, the task-owned `pg-sage-v25-final-20261029`
PostgreSQL container was removed after all database checks completed. The
compiler cache container was stopped and retained. Docker reported no running
containers, then Colima stopped successfully at 19:05:10Z, restoring the
initial local runtime state. The persistent development App/branch remains
available; the isolated native test branch has its existing 48-hour expiry.
Evidence: `colima-v25-final-stop.log` and the final task command log.

Earlier baseline cleanup:

The two owned browser pg_sage servers on API ports 8086/8087 were stopped. All eight owned PostgreSQL test containers and the dedicated replica network were removed after the tests completed. Local logs, raw JSON, reports, and the stopped Apps compiler cache container were preserved. Docker showed zero running containers before the parent successfully stopped Colima (exit 0), restoring its initial stopped state. Cleanup commands are recorded in `cleanup-topology.log` and `cleanup-containers.log`.
