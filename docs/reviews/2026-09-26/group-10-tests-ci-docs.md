# Group 10 — Test suite quality, CI, release, docs truthfulness, repo hygiene

## Scope (packages/files reviewed, LOC)

| Area | What was reviewed | Size |
|---|---|---|
| Go tests | all `*_test.go` under `sidecar/` (AST scan + targeted reads) | 539 files, 158,254 LOC, 5,328 `Test*` funcs |
| Prod code (for ratios) | non-test `.go` under `sidecar/` | 91,057 LOC (tests : prod = 1.74 : 1) |
| Build tags | `integration` (20 files), `e2e` (20), `integration \|\| e2e` (1), `providerlive` (10) | |
| Test harness | `sidecar/internal/testdb/{fixture,live}.go`, per-package `TestMain` (32 packages) | |
| CI | `.github/workflows/{ci,test,docs,traffic-snapshot}.yml` + last master run log (#34176764259) | |
| Release | `.goreleaser.yml`, `sidecar/Dockerfile`, root `Dockerfile`, `Makefile`, `META.json`, `pg_sage.control`, git tags, GH releases | |
| Compose / e2e | `docker-compose{,.test,.e2e}.yml`, root `e2e/` (Playwright, 23 specs), `sidecar/web/e2e` (9 specs), `sidecar/e2e`, `sidecar/test/hint_verify` | |
| Docs | project `CLAUDE.md` (untracked, main checkout), `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, both `config.example.yaml`, `mkdocs.yml` | |
| Hygiene | `git ls-files`, `git count-objects -vH`, ignored files in main checkout | 1,408 tracked files, 9.23 MiB pack |

Headline: CI is in better shape than the brief feared (real PG17 service, per-package fixture DBs,
integration + sidecar e2e actually run, only 37 skips in the last master run, all explainable).
The real problems are (1) the **documented quick-start path runs no pipeline** (default mode is
still the C-extension mode), (2) the **config example shipped in release archives does not load**,
(3) ~30% of tests are coverage-driven with a measurable tail of zero-assertion tests, (4) no race
detector / no PG version matrix / no pg_hint_plan / no Playwright in CI, and (5) heavy docs drift.

---

## A. Bugs

| ID | Sev | Conf | file:line | summary |
|---|---|---|---|---|
| G10-B01 | P1 | CONFIRMED (read) | `sidecar/internal/config/defaults.go:7`, `config/fleet.go:109-111`, `cmd/pg_sage_sidecar/main.go:231-237,1089` | Default mode is `extension`; README/installation/walkthrough quick start (`--pg-url` or `docker run -e SAGE_DATABASE_URL`) starts API only — no collector/analyzer/executor, no instance registered |
| G10-B02 | P1 | CONFIRMED (executed) | `config.example.yaml:31,45,54`; `.goreleaser.yml:35` | Root `config.example.yaml` — the one packaged into every release tarball — fails strict YAML load (3 unknown keys) |
| G10-B03 | P2 | CONFIRMED | `sidecar/Dockerfile:16`; `.github/workflows/ci.yml:84-104` | Docker image has no version ldflags (`--version`/`pg_sage_info` say `dev`); image only tagged `latest`+sha on every master push, never on release tags; README tells users to run `:latest` |
| G10-B04 | P2 | CONFIRMED | `SECURITY.md:8-12` | Supported-versions table lists only `0.8.x`; current release is v1.5.0 → policy literally says the shipping release gets no security fixes |
| G10-B05 | P2 | CONFIRMED | `.github/workflows/traffic-snapshot.yml` | Scheduled workflow has failed every day (≥15 consecutive runs, TRAFFIC_TOKEN unset) — permanent red noise on master masks real failures |
| G10-B06 | P2 | CONFIRMED (executed) | `sidecar/cmd/pg_sage_sidecar/*_test.go`, `internal/agentdb/schema_concurrency_test.go:59-84` | Without `SAGE_TEST_DATABASE_URL`, the CONTRIBUTING test command fails: 14 cmd tests hit 15 s meta-db retry timeouts (212 s package), 3 agentdb tests hard-fail — while 30 other packages skip. Inconsistent skip/fail policy |
| G10-B07 | P3 | CONFIRMED (baseline) | `sidecar/internal/schema/schema_test.go:403-423` | `TestPersistTrustRampStart_ZeroConfigUsesNow` compares DB `now()` against host clock ±2 s; fails when the DB host clock is skewed (observed 2.6 s on Docker Desktop in `raw-baseline-unit-db.txt`) |
| G10-B08 | P3 | CONFIRMED | `sidecar/internal/executor/coverage_boost_test.go:1900-1969` | Known flake `TestCoverage_ExecuteManual_ConcurrentlyPath`: root cause diagnosed (shared-DB CIC snapshot wait vs 5 s lock_timeout); mitigated by per-package fixture DBs, but the test still asserts only `err == nil` and carries a stale "actionID may be 0" comment |
| G10-B09 | P3 | CONFIRMED (executed) | `sidecar/internal/config/config.go:1002-1017` | `warnUnexpandedEnvVars` scans YAML comments (false "not set" warnings for `${VAR_NAME}`, `${WEBHOOK_TOKEN}`, `${DLE_TOKEN}` from the shipped example) and hard-codes `"config.yaml"` as the file name; its 6 tests pass with the body deleted |
| G10-B10 | P3 | CONFIRMED (read) | `sidecar/internal/config/config.go:528` | `_ = fs.Parse(args)` discards flag errors; a typo such as `--pg-ulr` prints usage and the sidecar continues on defaults (localhost). Cross-ref config group |
| G10-B11 | P3 | CONFIRMED (read) | `sidecar/cmd/pg_sage_sidecar/main.go:2235-2240` | `pg_sage_mode` gauge documents `0=extension, 1=standalone`; fleet mode reports `0` ("extension") |
| G10-B12 | P3 | CONFIRMED | `test-fixtures/full_surface/walkthrough_sidecar.err.log`, `local_monitor_config.yaml` | Tracked logs include a generated initial admin password and a 3.3 MB log; a personal always-on monitor config (LifeOS hosts/DB names) is tracked in a public repo |

Test-suite quality and CI gaps (sections A2/A3 below) carry IDs G10-B13..B22.

### G10-B01 — Default mode `extension` makes the documented quick start a no-op (P1)

- **Failure scenario:** user follows README Quick Start: `./pg_sage --pg-url postgres://…` (also
  `docs/installation.md:73,145`, `docs/walkthrough-windows.md:47`, `CONTRIBUTING.md`), or
  `docker run -e SAGE_DATABASE_URL=… ghcr.io/…:latest`. No config file, no `--mode`, no `SAGE_MODE`.
  `Load` → `newDefaults()` sets `Mode: DefaultMode` = `"extension"` (`defaults.go:7`;
  `normalize()` also maps `""` → `"extension"`, `fleet.go:109-111`). Startup logs
  `mode: SIDECAR — no extension, using catalog queries` (`main.go:215`), but the mode switch at
  `main.go:231-237` only runs `initStandalone()` for `IsStandalone()` (`Mode == "standalone"`,
  `config.go:1174`) and `initFleetMultiDB()` for fleet. `initFleetAndAPI` only registers an
  instance when `IsStandalone()` (`main.go:1089`). Result: dashboard and API up, zero databases,
  no findings, no cases, forever — with a log line that says the sidecar is working.
- **Why tests miss it:** every e2e/binary test sets the mode explicitly
  (`sidecar/e2e/smoke_test.go:231`, `pipeline_binary_test.go:175,237`, `fleet_test.go:177`), and
  `internal/config/config_test.go:25` / `config_roundtrip_test.go:614` *assert* the default is
  `"extension"`, cementing the bug.
- **Root cause:** C-extension era default never flipped when the extension was frozen.
- **Fix:** `DefaultMode = "standalone"` and `normalize()` maps `""` → `"standalone"` (or: infer
  standalone when a DSN/host is supplied and no `databases:` list). Delete `extension` mode with
  G10-D02. Update the two tests that enshrine the old default.
- **Test-to-add:** `TestLoad_NoConfigWithPgURL_RunsStandalone` (config: `Load([]{"--pg-url", x})`
  → `Mode == "standalone"`), and an e2e smoke that launches the binary with only `--pg-url`
  (no `SAGE_MODE`) and asserts `/api/v1/databases` returns 1 instance and a snapshot within 30 s.

### G10-B02 — Release tarball ships a config example that refuses to load (P1)

- **Failure scenario:** `.goreleaser.yml:35` packages the *root* `config.example.yaml`. The loader
  uses `decoder.KnownFields(true)` (`config.go:958-959`). Loading the root example via an overlay
  test (no repo writes) returns:
  `line 31: field executor not found in type config.Config`,
  `line 45: field max_output_tokens not found in type config.LLMConfig`,
  `line 54: field advisor not found in type config.LLMConfig`. A user who copies it to
  `config.yaml` gets a startup error. The `sidecar/config.example.yaml` loads cleanly — the wrong
  file is shipped.
- **Also:** root example documents only 26 of 253 leaf keys; `sidecar/` example omits 111
  (entire `rca`, `runaway`, `explain`, `logwatch`, `schema_lint`, `migration`, `agentdb` sections,
  `llm.json_mode`, `llm.fleet_token_budget_daily`, fleet `databases`/`defaults`, `meta_db`,
  `encryption_key`). Header still says "v0.9".
- **Fix:** delete root `config.example.yaml`, point goreleaser at `sidecar/config.example.yaml`
  (or generate the example from the typed registry that already feeds
  `docs/generated/config-lifecycles.md`).
- **Test-to-add:** `TestShippedConfigExamplesLoad` in `internal/config` that runs `loadYAML` on
  every `config.example.yaml` / `docs/**/*.yaml` example and fails on any error; plus
  `TestConfigExampleCoversRegistry` that diffs example keys vs `reflect` over `Config` and fails
  on unknown keys (allow-list for intentionally undocumented ones).

### G10-B03 — Container image is unversioned and tracks master (P2)

- `sidecar/Dockerfile:16` builds with `-ldflags="-s -w"` only, so `main.version` stays `dev`
  (`main.go:51`); `pg_sage_info{version="dev"}` in every container. The `docker` job
  (`ci.yml:84-104`) runs only on master pushes and tags `latest` + SHA; release tags publish no
  image. README Quick Start pulls `:latest`, i.e. whatever merged last, not v1.5.0. Also no
  multi-arch (`linux/arm64`) although goreleaser builds arm64 binaries.
- **Fix:** add `ARG VERSION/COMMIT/DATE` → `-X main.version=…`; move image push to the tag
  pipeline (`docker/metadata-action` semver tags + `latest` only on release), `platforms:
  linux/amd64,linux/arm64`; or use goreleaser `dockers:`.
- **Test-to-add:** CI step `docker run --rm image --version | grep "$GITHUB_REF_NAME"` on tags.

### G10-B04 — SECURITY.md supported versions stale (P2)
Table says `0.8.x Yes`, everything else EOL. Replace with `1.5.x Yes`, `< 1.5 No`, and add a
release-checklist item (see G10-I06).

### G10-B05 — Permanently failing scheduled workflow (P2)
`gh run list --workflow traffic-snapshot.yml` shows failures daily 2026-09-12 → 2026-09-26. Either
add the `TRAFFIC_TOKEN` secret or guard the step with `if: secrets.TRAFFIC_TOKEN != ''` and exit 0.
Also 9 orphaned draft releases (8 × `v1.2`) exist — delete drafts (user action).

### G10-B06 — Inconsistent no-DB behavior; contributor test command is red (P2)
- `docs/reviews/2026-09-26/raw-baseline-unit.txt`: `FAIL cmd/pg_sage_sidecar 211.8s` (14 tests,
  each 15 s of meta-db retry against the disabled sentinel DSN `127.0.0.1:1`), `FAIL
  internal/agentdb` (reproduced: `schema_concurrency_test.go:60,84` call `freshEnsurePool` which
  `t.Fatal`s on connect). Every other DB test uses `t.Skip` on the same condition.
- Conversely the skip path hides real breakage: locally `go test ./...` prints `ok` for packages
  where most DB tests were skipped (e.g. `collector 9.7%`, `querystore 11.9%`, `retention 16.7%`,
  `executor 47.5%` coverage without a DB), violating "no silent skips".
- **Fix:** one policy in `testdb`: `testdb.RequireLive(t)` skips with a banner when no fixture is
  configured, **fails** when `SAGE_REQUIRE_LIVE_DB=1` (set in CI). Route every DB helper through
  it (there are ≥20 hand-rolled `connectTestDB`/`requireDB` variants). Document
  `SAGE_TEST_DATABASE_URL` in CONTRIBUTING.
- **Test-to-add:** `testdb` unit test for both modes; CI skip-budget gate (G10-I03).

### G10-B07 — Clock-skew flake in ramp-start test (P3)
`PersistTrustRampStart` inserts DB `now()`; test brackets with `time.Now()±2s`. Docker Desktop VM
clock ran 2.6 s ahead → fail. **Fix:** bracket with `SELECT clock_timestamp()` from the same pool
before/after. Same pattern should be grepped for elsewhere (`time.Now().Add(-2 * time.Second)` +
DB timestamps).

### G10-B08 — `TestCoverage_ExecuteManual_ConcurrentlyPath` flake: diagnosis (P3)
- **History:** flagged flaky in April-May 2026 ("passes in isolation"); no failures in the Sept
  task logs or in CI runs since July.
- **Root cause (historical):** the test runs `CREATE INDEX CONCURRENTLY` with `LockTimeoutMs: 5000`
  (`coverage_boost_test.go:1944-1949`). CIC waits (a) for lockers of the table and (b) in
  `WaitForOlderSnapshots` for every transaction *in the same database* holding an older snapshot;
  those are virtual-xid lock waits, so `lock_timeout` applies → SQLSTATE 55P03 →
  `execManualSQLWithRetry` retries 3× with 0.5 s/1 s back-off (`manual.go:269-310`) → fails.
  While all packages shared one database under `go test ./...` (parallel packages), any other
  package's open transaction >5 s (and `internal/schema` dropping/recreating `sage`, see the
  `ensureSageSchema` comment at `executor_extra_test.go:145-147`) broke it. The in-test comment
  (`:1963-1964`) admits exactly this.
- **Why it's mostly fixed:** since 2026-07-19 (`492cd7b`) `testdb.Run` gives each package process
  its own fixture DB, and CIC/advisory locks are database-scoped. Residual risk (PLAUSIBLE):
  goroutines leaked by earlier executor tests in the same package (rollback monitors started via
  `startRollbackMonitor` with `context.WithoutCancel`) touching the same fixture DB.
- **Remaining defects in the test:** only asserts `err == nil`; never checks `pg_index.indisvalid`,
  the `action_log` row/outcome, `findings.acted_on_at`, or `actionID > 0`; the comment "actionID
  may be 0 due to pgx type inference" is stale (CI log shows `actionID = 18`).
- **Fix:** assert `actionID > 0`, outcome `success`, index valid, finding acted-on; use a unique
  table/index name per run; set `LockTimeoutMs` explicitly high in this test *and* add a separate
  deterministic test that holds an open snapshot on a second connection and asserts the 55P03 →
  `ErrLockNotAvailable` path (the thing that used to flake, tested on purpose).

### G10-B09 — Unset-env warning scans comments; untested (P3)
Loading `sidecar/config.example.yaml` prints warnings for `${VAR_NAME}` (comment line 4),
`${WEBHOOK_TOKEN}` (commented block) and `${DLE_TOKEN}`. The warning always says
`config "config.yaml"` regardless of the real path. Mutation check: replacing the body of
`warnUnexpandedEnvVars` with a no-op (via `-overlay`, no repo write) → **the whole
`internal/config` package still passes**. Fix: parse with `yaml.Node` and inspect scalar values
only; inject an `io.Writer`/logger; tests assert the exact warning set.

### G10-B10, B11, B12 — see table; fixes are one-liners
B10: `if err := fs.Parse(args); err != nil { return nil, fmt.Errorf("parse flags: %w", err) }`.
B11: emit `pg_sage_mode{mode="fleet"}` or 0/1/2 and fix HELP. B12: `git rm` the four
`test-fixtures/full_surface/*.log`, `local_monitor_config.yaml`, `test_briefing.txt`; rotate
nothing (test instance), but add a pre-commit secret scan (gitleaks) to CI.

---

### A2. Test-suite quality

| ID | Sev | Conf | file:line | summary |
|---|---|---|---|---|
| G10-B13 | P2 | CONFIRMED (AST scan) | 46 tests, list below | Tests with **zero** assertions (no `t.Error*`/`t.Fatal*`, no helper taking `t`) |
| G10-B14 | P2 | CONFIRMED (AST scan) | 52 tests | Tests whose only assertions sit inside `if err != nil {…}` |
| G10-B15 | P2 | CONFIRMED (mutation) | `internal/config/coverage_phase2_test.go:182-223` | Coverage tests survive deletion of the function under test |
| G10-B16 | P3 | CONFIRMED | 30 files, 1,527 `Test*` funcs (29%), ~41k LOC | Coverage-driven files (`coverage_boost`, `coverage_phase2`, `coverage_gaps`, `*_coverage`) — largest `api/coverage_phase2_test.go` 5,097 lines |
| G10-B17 | P3 | CONFIRMED | `internal/alerting/coverage_boost_test.go:872-890` | `t.Skip` used where an assertion belongs |
| G10-B18 | P3 | CONFIRMED | `internal/retention/cleanup_test.go:241-262` | Tautology test |

**G10-B13 — zero-assertion tests (46).** Method: `go/ast` scanner (scratchpad, not committed)
over all `Test*` funcs. Representative, all read and confirmed:

- `internal/executor/executor_new_test.go:78` `TestExecuteManual_AcceptsValidSQL_ButNeedsDB` —
  `recover()`s any panic and discards the return (`_, _ = e.ExecuteManual(...)`, `:100`); passes if
  validation accepts, rejects, or panics.
- `internal/tuner/functional_test.go:1769` `TestFunctional_Coverage_WithLLM_OptionSetsFields` —
  comment "No panic is the assertion"; passes if `WithLLM` is a no-op.
- `internal/alerting/coverage_boost_test.go:247` `TestCoverage_LogAlert_PoolExecError` — captures
  `loggedErr` then `_ = loggedErr` (`:282`).
- `internal/config/coverage_phase2_test.go:182-223` — six `WarnUnexpandedEnvVars` tests (see B15).
- `internal/retention/cleanup_test.go:231,267`, `coverage_boost_test.go:197,465` — "if it doesn't
  panic on nil pool the early return works".
- Concurrency "tests" with no assertions and not run under `-race` in CI (so they assert nothing
  at all there): `internal/collector/collector_coverage_test.go:1028,1068`,
  `internal/executor/runaway_test.go:551`, `internal/executor/wave1_policy_test.go:270`,
  `internal/api/coverage_gaps_test.go:151`.
- Also: `analyzer/analyzer_test.go:102`, `analyzer/coverage_boost_test.go:280`,
  `api/auth_handlers_test.go:213`, `api/config_handlers_test.go:20`,
  `api/coverage_boost_test.go:654,704`, `api/events_test.go:104`, `briefing/briefing_test.go:252,
  263,275`, `briefing/coverage_boost_test.go:169`, `executor/coverage_boost_test.go:256`,
  `executor/dispatcher_test.go:26`, `executor/mode_test.go:140`,
  `forecaster/coverage_boost_test.go:152`, `logwatch/fanout_test.go:67,79`,
  `logwatch/tailer_test.go:320`, `logwatch/watcher_test.go:53`,
  `notify/coverage_phase2_test.go:1352,1356,1360` (interface-satisfaction checks — replace with
  compile-time `var _ Sender = (*EmailSender)(nil)`), `rca/self_action_wiring_test.go:152`,
  `tuner/functional_test.go:3733`, `tuner/revalidate_test.go:37`.
- 55 test comments say some variant of "should not panic" as the success criterion.

**G10-B14 — err-only tests (52).** Concentrated in validators
(`advisor/validate_extra_test.go` 15, `store/notification_validation_unit_test.go` 9,
`advisor/advisor_coverage_test.go` 8, `advisor/validate_test.go` 5, `executor/validate_test.go` 4,
`store/coverage_boost_test.go` 4). For pure validators returning only `error`, `err == nil` on a
valid input *is* the spec — acceptable **if** a paired negative case exists. Action: audit each for
a matching reject-case; the `store/coverage_boost_test.go` ones are not validators and need
state assertions.

**G10-B15 — mutation-proven hollow coverage.** `warnUnexpandedEnvVars` body deleted → 6/6 targeted
tests pass and `go test ./internal/config` passes. Coverage % counted these lines as covered.
Recommend a periodic mutation run (`go-mutesting` or a small overlay script) on
executor/policy/config — the packages where "covered but unasserted" is dangerous.

**G10-B16 — coverage-driven files.** Naming (`TestCoverage_*` ×1,192, `TestPhase2_*`) shows tests
written to hit lines. They are not all bad (many assert), but they are where B13/B14 cluster, and
76 test files exceed the 500-line limit. Recommend folding surviving tests into behavior-named
files per feature and deleting ones that only restate the implementation.

**G10-B17 — skip as assertion.** `TestCoverage_ShouldAlert_QuietHoursBlock` does
`if !th.IsQuietHours(noon) { t.Skip(...) }` — if quiet-hours logic breaks, the test *skips* (CI
reports it green). The remainder of the test has no assertion. Also time-dependent skips:
`executor/executor_extra_test.go:687` (skips 23:59-00:59), `briefing/coverage_boost_test.go:674`.
Use an injected clock.

**G10-B18 — tautology.** `TestRetentionConfig_DefaultValues` builds a `RetentionConfig{30,90,
180,14}` literal and asserts the literal. It never touches `newDefaults()`. The known
"default-value masking" pattern would sail through it. Replace with
`DefaultConfig().Retention` assertions (and ensure 0 is not the default — 0 disables purge,
`cleanup.go:54`).

### A3. CI coverage — what actually runs

What `ci.yml` runs on every push/PR (verified against run 34176764259):

| Suite | Runs in CI? | Notes |
|---|---|---|
| Web lint + vitest + build | Yes | `npm run lint/test/build` |
| `go vet` | Yes (twice: ci.yml + test.yml) | |
| golangci-lint v2.11.4 | Yes (test.yml) | default linter set; see G10-D05 |
| Go unit (untagged) | Yes, **3×** (ci unit, ci integration step re-runs untagged files, test.yml) | against real PG17 fixture DB |
| Go `-tags=integration` | Yes | |
| `sidecar/e2e -tags=e2e` | Yes | LLM tests skip (no key) |
| `internal/advisor` `e2e`-tagged (8 files) | **Never** | CI passes `-tags=e2e` only to `./e2e`; they compile (`go vet -tags=e2e` OK) but never execute |
| `providerlive` (10 files) | Never | by design; needs cloud creds |
| `test/hint_verify` (12 tests) | Compiles, **6+ skip** | CI image lacks `pg_hint_plan` |
| Race detector | **Never** | only local Linux runs in `tasks/*race*.jsonl` |
| PG 14/15/16/18 | **Never** | only `pgvector:0.8.2-pg17`; code branches at `collector.go:180,404,418`, `plancapture.go:70`, `hypopg_session.go:121` |
| Playwright (root `e2e/` 23 specs, `sidecar/web/e2e` 9 specs) | **Never** | |
| C extension regression (`test/`) | Never | |
| `cloudsqltests/*.go` | Never compiled (outside any module) | |
| govulncheck / npm audit / secret scan | Never | |
| goreleaser check / snapshot | Only on tag | a broken release config is found at release time |

Last master run: 37 SKIPs, all attributable — LLM live (15, no API key), live cloud AgentDB (8),
pg_hint_plan (≥6 per step), OS-path (2), Gemini live (2). No FAIL. Skips are not gated, so a new
silent skip would go unnoticed.

| ID | Sev | Conf | where | summary / fix |
|---|---|---|---|---|
| G10-B19 | P2 | CONFIRMED | `ci.yml:70-80` | No `-race` anywhere; the repo has many concurrency tests (B13) that assert nothing without it. Fix: run the unit step as `go test -race ./...` (Linux runner). |
| G10-B20 | P2 | CONFIRMED | `ci.yml:14` services | Single PG major (17). Product claims PG14+ (README badge, `startup/checks.go:103`). Fix: matrix `[14,15,16,17,18]` for unit+integration (PG14/15 need non-pgvector images or pgvector per-version tags). |
| G10-B21 | P2 | CONFIRMED | `ci.yml` "Enable pg_stat_statements" | Per-query tuner is a headline feature but `pg_hint_plan` is never installed, so `test/hint_verify` and hint execution paths never run. Fix: install `postgresql-17-pg-hint-plan` (and `shared_preload_libraries += pg_hint_plan`) — `testdb` already auto-creates it when available (`fixture.go:156-157`). |
| G10-B22 | P3 | CONFIRMED | `test.yml` | Duplicate workflow re-running build/vet/unit on the same triggers; only `lint` is unique. Merge lint into ci.yml and delete test.yml; drop the redundant untagged re-run by making the integration step `-run` only tagged tests or accept one combined `-tags=integration` step. |

---

## B. Dead / unwired / half-built

| ID | file:line | what | verdict | why |
|---|---|---|---|---|
| G10-D01 | `src/` (19 C files), `include/`, `sql/`, `test/`, root `Makefile`, `META.json`, `pg_sage.control`, root `Dockerfile`, `docker-entrypoint-initdb.d/`, `docker-compose.yml` service `pg_sage` | Frozen C extension, ~17k LOC, v0.5.0 | **DELETE** from master (tag `c-extension-final` first) | Not built by goreleaser or CI, has known unfixed SQL injection + ring-buffer race (April review), PGXN-shaped metadata invites installation, and the default `docker compose up` still builds it and points the sidecar at it (`docker-compose.yml:39-79`). If kept, move to `legacy/c-extension/` with a README and remove it from the default compose |
| G10-D02 | `config/defaults.go:7`, `config/fleet.go:109-111`, `config/config.go:595`, `main.go:211-216,2225-2233,2263-2265,2687-2712` (`detectExtension`, `writeExtensionMetrics`, `sage.status()` query) | Sidecar "extension mode" | **DELETE** (after G10-B01) | Only purpose is interop with D01; it is also the cause of B01 |
| G10-D03 | `lockCrossPackage`/`serializeAcrossPackages`/`ensureSageSchema` in 8 packages (analyzer, api, auth, executor, forecaster, schema, schema/lint, store; 14 files), e.g. `executor/executor_extra_test.go:112-185` | Cross-package advisory-lock serialization + "schema dropped by other package" guards | **DELETE** | Advisory locks are keyed per database; with per-package fixture DBs (`testdb.Run`) they serialize nothing and the schema-drop race cannot happen |
| G10-D04 | `cloudsqltests/*.go` (17 `package main` files, one `//go:build ignore`) | Ad-hoc cloud QA scripts | **DELETE** or move to `sidecar/cmd/qa/<name>/` | Not in any Go module → never compiled/vetted; will silently rot against internal APIs |
| G10-D05 | `sidecar/.golangci.yml` | `gosimple` exclusions (merged into staticcheck in golangci v2); blanket staticcheck off for `internal/config/config.go`; errcheck+staticcheck off for all tests | **DELETE** stale rules, narrow the rest | Dead config and a 1,243-line core file exempt from staticcheck |
| G10-D06 | `.github/workflows/test.yml` | Duplicate test job | **DELETE** (keep `lint`) | See B22 |
| G10-D07 | `test_briefing.txt`, `sidecar/seed_snapshots.sql` (unreferenced; differs from `tests/integration/seed_snapshots.sql`), `sidecar/fleet_demo_0_8_5.yaml`, `research/2026-09-04-product/*.jsonl` + `*coverage.out`, `test-fixtures/full_surface/*.log` | Stray artifacts tracked in git | **DELETE** | No references; logs contain a generated password (B12) |
| G10-D08 | root `config.example.yaml` | Stale, non-loading example | **DELETE** (ship sidecar one) | B02 |
| G10-D09 | `sidecar/internal/api/dist/**` committed | Built UI bundle | **TEST-ONLY OK / keep, but guard** | Needed for `go:embed` on `go install`, but 53 historical bundle versions (~0.87 MB each raw) dominate pack growth and nothing checks it matches `web/src`. Add a CI "dist is fresh" diff check or build in goreleaser only |

Hygiene answers to the brief's question (4):

- `files.zip`, `pg_sage-0.5.0.zip`, `pg_sage_claude_code_go.zip`, all `*.exe` (≈30 in main
  checkout incl. `sidecar/*_qa*.exe`), `test-output/`, `logs/`, `tasks/` (381 files, 90 MB),
  `sidecar/tasks/`, `dist/` are **not tracked** — all ignored (`git status --ignored`). Local disk
  only: ≈621 MB of exe/zip, 90 MB tasks, 51 MB test-output, 48 MB dist, 16 MB logs.
- Tracked repo is healthy: 1,408 files, pack 9.23 MiB. Biggest tracked blobs:
  `test-fixtures/full_surface/verify_sidecar.err.log` (3.3 MB) and the UI bundle.
- `.gitignore` ignores `CLAUDE.md`, so the project CLAUDE.md (testing standards, architecture) is
  absent from clones, worktrees (including this review worktree), CI agents, and contributors.

---

## C. Feature improvements (ranked impact × effort)

| ID | Improvement | Impact | Effort |
|---|---|---|---|
| G10-I01 | Fix B01 + B02 together and add a **"documented-path smoke"** e2e: run the binary exactly as README/installation show (`--pg-url` only; `docker run` with only `SAGE_DATABASE_URL`; `--config <shipped example with host substituted>`), assert an instance and a snapshot appear | High | S |
| G10-I02 | **Race + PG matrix + pg_hint_plan** in CI (B19-B21). One matrix job `pg: [14,15,16,17,18]` × `go test -race -tags=integration`; keep e2e on 17 only | High | M |
| G10-I03 | **Skip budget gate**: `go test -json` → script fails the job if any SKIP is not in an allow-list (`live LLM`, `live cloud`, OS-specific). Directly enforces the CLAUDE.md "no silent skips" rule in CI | High | S |
| G10-I04 | **Uniform live-DB policy** in `testdb` (`RequireLive` + `SAGE_REQUIRE_LIVE_DB=1` in CI) replacing ≥20 hand-rolled pool helpers; fixes B06 and deletes D03 | Med | M |
| G10-I05 | **Assertion lint**: extend the AST scanner (≈120 lines) into `sidecar/internal/testsupport/lint` run as a unit test: fail on new zero-assertion tests; allow-list existing ones while they are fixed | Med | S |
| G10-I06 | **Release pipeline**: versioned multi-arch image from tags (B03), `goreleaser check` + `release --snapshot` on PRs, CHANGELOG/SECURITY/README version assertions in a `release-lint` job, run Playwright against the built binary on tags | Med | M |
| G10-I07 | **Nightly workflow** for things too slow/credentialed for PRs: Playwright (both suites), `providerlive` with repo secrets, LLM live tests with a budget-capped key, mutation sampling (B15) on executor/policy/config | Med | M |
| G10-I08 | Consolidate/rename coverage-driven files (B16); target: no test file > 500 lines, no `TestCoverage_*` names; each test named for the behavior it proves | Low-Med | L |
| G10-I09 | Generate `config.example.yaml` and the config docs from the typed registry (`doc:` tags already exist on every field) so the example can't drift | Med | S |
| G10-I10 | Add govulncheck, `npm audit --omit=dev`, gitleaks to CI | Med | S |

### Docs drift (brief task 5) — project `CLAUDE.md` vs reality

| Claim (CLAUDE.md) | Reality | Verdict |
|---|---|---|
| "no C extension, no shared_preload_libraries" | Default mode is `extension`; C ext still in repo and default compose | FALSE in effect (B01, D01) |
| Go 1.24 (also README badge, README "Requires Go 1.24+") | `go 1.25.0` in `go.mod`; Dockerfile `golang:1.25` | Stale; Go 1.24 with `GOTOOLCHAIN=local` cannot build |
| Tests: "771+" (line 11/109) and "1100+" (line 159) | 5,328 `Test*` funcs; ~14.5k PASS lines per CI run | Stale and self-contradictory |
| "REST API (17 endpoints)" + table | 86 distinct `METHOD /api/v1/...` patterns registered in `internal/api` | Stale (5×) |
| Package list: 13 internal packages | 46 (agentdb, autonomy, cases, clone, custodian, ledger, logwatch, mcp, migration, policy, providerobs, querystore, rca, rollout, schemaguard, verify, value, vectorlab, …) | Stale |
| Web: "11 components, 5 pages, hooks/useAPI.js" | 22 components, 41 page modules; `useAPI.js` exists | Stale |
| "Interactive diagnose (ReAct) exists only in the frozen C extension via `sage.diagnose()`" | True — no ReAct/diagnose loop in the sidecar (only static diagnose SQL in `cases/incident_projector.go:454-538`) | TRUE, but README advertises a capability of an artifact that is no longer shipped |
| Deps: `google/uuid`; "keep minimal" | No `google/uuid`; AWS SDK v2 (3 modules) + `golang.org/x/crypto` present | Stale |
| Testing level 3: "Schema tests require local PostgreSQL on port 5432" | Tests use `SAGE_TEST_DATABASE_URL` + per-package fixture DBs (`testdb`) | Stale — and pointing contributors at the local 5432 instance the user wants protected |
| "Do not use global mutable state" | `main.go` is 2,838 lines of package-level globals (`pool`, `cfg`, `fleetMgr`, `extensionAvailable`, …) | Rule not followed in the entrypoint |

README / other docs:

- README Quick Start (`--pg-url`, `docker run … :latest`) → B01, B03.
- README badges Go 1.24 → stale. "Alerting: Slack, PagerDuty, webhook" → true
  (`alerting/{slack,pagerduty,webhook}.go`). "6 LLM advisors" → matches config toggles.
- `CONTRIBUTING.md`: "active v0.9 product"; test command lacks `SAGE_TEST_DATABASE_URL` (B06);
  `npm run test:e2e` documented but never run in CI.
- `SECURITY.md` → B04. `sidecar/config.example.yaml` header "v0.9".
- `docs.yml` runs `mkdocs gh-deploy` on the whole `docs/` dir; mkdocs builds pages not in `nav`,
  so internal handoffs (`codex-session-handoff-*.md`, `codex-security-review-blocked-*.md`,
  `reports/`, `superpowers/`) and **this review directory** get published to GitHub Pages on
  merge (repo is PUBLIC).

### Version consistency (brief task 6)

| Artifact | Version |
|---|---|
| CHANGELOG top / latest tag / latest GH release | v1.5.0 (2026-09-07) — consistent |
| goreleaser binaries | `-X main.version={{.Version}}` → 1.5.0 — correct |
| Docker image | `dev` (B03) |
| `sidecar/web/package.json` | 0.0.0 (harmless, private) |
| `META.json`, `pg_sage.control`, root `Makefile VERSION` | 0.5.0 (C extension, D01) |
| `SECURITY.md` | 0.8.x (B04) |
| CONTRIBUTING / sidecar example | "v0.9" |
| Tags | `v1`, `v1.1`, `v1.2`, `v1.3` are not full semver; 8 draft `v1.2` releases left behind |

---

## D. Questions the user isn't asking

1. **Has anyone ever run the README quick start end-to-end on a clean machine?** B01 says it
   yields an empty dashboard; every automated path sets the mode explicitly.
2. **Should unfixed P0/P1 findings live in a public repo and get auto-published to GitHub Pages?**
   This review directory will be deployed by `docs.yml` on merge. Consider excluding
   `docs/reviews/` from mkdocs (`exclude_docs:`) or keeping security findings private until fixed.
3. **Is 29% of the test suite paying rent?** 1,527 coverage-driven tests make the suite slow and
   the coverage number unreliable (B15). Would you accept a lower coverage % in exchange for a
   mutation-score floor on executor/policy?
4. **What is the supported PG matrix, really?** Docs say 14+, CI says 17, compose says 16, and
   PG18 (GA Sept 2025) is not tested at all.
5. **Why does the project CLAUDE.md live outside git?** Agents in worktrees/CI never see the
   testing standards they are held to.
6. **Is `:latest` a promise?** Every master merge ships to users pulling `:latest`.
7. **Who owns the C extension's known SQL injection?** As long as `src/` + `META.json` exist on
   master, it is a published, installable artifact with a known vuln.

---

## E. Verification notes

Ran (all read-only with respect to the repo; scratch files in the session scratchpad):

- `git ls-files`, `git count-objects -vH`, `git rev-list --objects --all | git cat-file` (largest
  blobs), `git status --porcelain --ignored` in the main checkout, `git log -S`.
- AST scanner over all `*_test.go` (scratchpad `weak/main.go`): 5,494 test funcs parsed,
  46 zero-assertion, 52 err-only.
- `go test -overlay` (virtual files only, nothing written to the repo) in `internal/config`:
  loaded both example configs + `local_monitor_config.yaml` (B02 errors captured); reflect-based
  key diff of `Config` vs examples; mutation of `warnUnexpandedEnvVars` → package still green.
- `go test -count=1 -run 'TestCoverage_ExecuteManual_ConcurrentlyPath|…' ./internal/executor`
  without a DB → SKIP (disabled sentinel DSN, no local PG touched).
- `go test -count=1 -run 'TestEnsureConcurrentColdStores|…' ./internal/agentdb` without a DB →
  FAIL (B06 reproduced).
- `go vet -tags=e2e ./internal/advisor`, `go vet -tags=providerlive ./tests/providerlive
  ./internal/agentdb ./internal/providerobs` → compile clean (never-run suites haven't rotted).
- `gh run list` / `gh run view --log` for run 34176764259 (latest master CI): 37 SKIP, 0 FAIL;
  flaky test PASS in both steps; per-package coverage (cmd/pg_sage_sidecar 43.5% lowest).
- `gh release list`, `gh repo view` (PUBLIC).

Could not verify:

- B01 by executing the binary (brief forbids starting servers); confirmed by reading the full
  mode path (`Load` → `newDefaults` → `normalize` → `main` init switch → `initFleetAndAPI`).
- Residual in-package flake source for B08 (needs a live DB and repeated runs).
- Whether committed `internal/api/dist` matches current `web/src` (needs `npm run build`).
- Race-detector status (needs cgo toolchain on Linux; not run on this Windows host).
