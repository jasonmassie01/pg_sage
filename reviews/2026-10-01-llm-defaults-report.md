# LLM features on by default — report (2026-10-01)

Branch `claude/llm-defaults-on`, based on master `ca515e5`. Product decision: "LLM
features should be on by default and the whole purpose of pg_sage. We are becoming an AI
DBA."

## Summary

- `llm.enabled`, `llm.optimizer.enabled` (with the deprecated
  `llm.index_optimizer.enabled`), `advisor.enabled`, `tuner.llm_enabled` and
  `rca.narration_enabled` now default to `true`. `explain.enabled` was already `true`.
- `llm.optimizer_llm.enabled` stays `false` on purpose (see "Not flipped").
- An LLM is used only when `llm.enabled` is set and both `llm.endpoint` and `llm.api_key`
  are set (`internal/llm/client_controls.go:63-65`, `configEnabled`). Without them every
  feature runs its deterministic path. Startup logs one INFO line per process
  (`internal/config/llm_notice.go:8`, logged at `cmd/pg_sage_sidecar/main.go:311`, once,
  in `initializeConfigController` after persisted overrides are applied).
- No trust, tier or execution default changed. Every write to the database that an LLM
  suggests still goes through the standing policy gate, the trust level and the execution
  mode (see the table and "Execution gating").
- `llm.token_budget_daily` defaults to 500000, non-zero, when there is no config file and
  when the `llm:` section is partial (tested). `0` still means "no cap".

## Switch table

All file:line references are on the branch head.

| Switch | Old default | New default | What it gates | LLM-only? | How execution stays gated |
|---|---|---|---|---|---|
| `llm.enabled` | false | **true** (`defaults.go:60`, `config.go:790`) | Master switch. `rt.llmOn` (`database_runtime.go:219`) = general client enabled = this switch + endpoint + key. `llmOn` decides whether these are wired: plan narrator (`database_runtime_monitor.go:40`), advisor (`:68-69`), RCA tier-2 correlation (`database_runtime_logs.go:139-140`), schema-lint JSONB enhancement (`database_runtime_logs.go:165-166`), executor action justifier (`database_runtime_exec.go:91-92`). It also caps the optimizer tier (`llm_client_registry.go:180`). The briefing and explain check the client per call. | LLM-only. With no endpoint or key, no client is enabled. | The justifier only writes an audit note after an action has executed (`executor/justify.go:46-70`, called from `apply_finding.go:294`). RCA tier-2 `recommended_sql` is incident evidence only (`cases/incident_projector.go:71`). Case actions come from deterministic playbooks (`incident_projector.go:88-124`). Narrator, lint and briefing write text only. |
| `llm.optimizer.enabled` | false | **true** (`defaults.go:74`, `config.go:804`) | LLM index optimizer. Built only when this switch is on and its client is enabled (`database_runtime_monitor.go:108-114`); otherwise it is nil. | LLM-only. With the optimizer nil, the analyzer's deterministic rules run as before. | Recommendations drop the model's risk (`optimizer/indexspec.go:186`). DDL runs only through `Executor.processFinding`: `evaluateFindingPolicy` (`executor/apply_finding.go:49-53`, `executor.go:315-327`, fails closed without a gate) authorizes the typed contract derived from the SQL (`executor.go:421-427`). Manual mode or a disabled executor stops all background actions (`apply_finding.go:30`). The gate returns observe-only at `trust.level: observation` or in manual mode (`policy/gate.go:48`, `:219-221`). |
| `llm.index_optimizer.enabled` (deprecated) | false | **true** (`defaults.go:66`) | Nothing at runtime. A YAML value is migrated into `llm.optimizer.enabled` (`config/llm_legacy.go:28`, called from `loadYAML`, `config.go:1023`). | LLM-only | Same as `llm.optimizer.enabled`. |
| `advisor.enabled` | false | **true** (`defaults.go:96`, `config.go:827`) | LLM config advisor, built only when `advisor.enabled && llmOn` (`database_runtime_monitor.go:68-69`, `:133`). **Also gated non-LLM behaviour:** the collector's per-cycle configuration snapshot (`pg_settings` plus every table's reloptions). Read-only, but it added extra catalog queries every collector cycle. | No, but not flipped for non-LLM use: the snapshot now runs only when the advisor will run (`collector.go:56`, `:185`; `database_runtime_monitor.go:56-63`). | Findings take `ActionRisk` from the SQL, not from the model (`advisor/prompt.go:100`, `:150`). An `action_risk` in the model's answer is ignored (tested). `ALTER SYSTEM`/`ALTER TABLE` run only through the executor gate path described above. |
| `tuner.llm_enabled` | false (zero value) | **true** (`defaults.go:130`, `config.go:884`) | Lets the tuner ask the LLM for pg_hint_plan hints (`database_runtime_monitor.go:181`). `tuner.enabled`, which is non-LLM and writes hints, was already true and is unchanged. | LLM-only. The tuner skips LLM work unless a primary or fallback client is configured, has its circuit closed and has budget left (`tuner/tuner.go:456`, `tuner/llm_prescriber.go:26-37`). | LLM hints are syntax- and allowlist-validated, and work_mem is clamped (`llm_prescriber.go` `convertPrescriptions`). The `hint_plan.hints` INSERT is the finding's `RecommendedSQL` (`tuner/tuner.go:833`) and runs only through the executor gate path. |
| `rca.narration_enabled` | false | **true** (`defaults.go:168`, `config.go:913`) | LLM rewrite of the incident notification summary. Falls back to the deterministic summary when there is no client or the client is disabled (`rca/narrate.go:98-104`). | LLM-only. Evidence gathering ran before and still runs regardless of the switch. | No SQL, no database access. Text only. |
| `explain.enabled` | true | true (unchanged) | The whole `POST /api/v1/explain` endpoint (`api/handlers_v09.go:266`). The LLM narrative is added only with an enabled client (`explain/explain_llm.go:31`). | No: it gates the deterministic EXPLAIN endpoint too. Left as is. | It is operator-initiated: `operatorUp` guards the endpoint (`api/router.go:345`), and its statement_timeout is configured. Unchanged. |
| `llm.optimizer_llm.enabled` | false | **false** (`defaults.go:90`, `config.go:818`) | A dedicated optimizer/tuner client. | n/a | Not flipped (see below). |

### Execution gating (verified in code)

1. The background executor acts only on durable recommendations. Each one runs through
   `evaluateFindingPolicy` → `policy.Gate.Authorize` (`executor/apply_finding.go:49`,
   `executor.go:315-330`). Without a gate it fails closed (`executor.go:327`).
2. The contract and risk tier come from the SQL (`contractForFinding`,
   `executor.go:421-427`), never from LLM output.
3. The gate runs hard stops, validation, the provider check, observe-only (observation
   trust or manual mode, `policy/gate.go:48`, `:219-221`), tier and ramp, refusal set,
   SQL-validation degradation and the windows (`policy/gate.go:31-58`).
4. None of the flipped switches is read by the policy package, the executor's
   authorization or the trust config. Tests assert that trust, tier and runaway defaults
   are unchanged (`TestLLMDefaultsLeaveExecutionDefaultsUnchanged`).

### Not flipped, and why

- **`llm.optimizer_llm.enabled` stays `false`.** It is a routing switch, not a feature.
  Without its own endpoint or model it just builds a second client for the same provider
  with its own 500000-token `token_budget_daily`. That would silently double the default
  daily ceiling to 1M tokens, which conflicts with honouring `llm.token_budget_daily`. The
  LLM optimizer and tuner are fully on through the general client. The docs and CHANGELOG
  say how to enable it.
- **`tuner.enabled` / `tuner.verify_after_apply`:** non-LLM (they write hints and run
  revalidation). Already `true`, untouched.
- **The advisor's configuration snapshot** (the non-LLM part of `advisor.enabled`): now
  collected only when the advisor will actually run.
- **`sre.llm.enabled`:** belongs to `claude/sage-sre-m3-llm`. `internal/config/sre.go` and
  `internal/llm/` were not touched.

## Changes (commits, oldest first)

1. `83e525a test(config)`: phase-one tests (written and committed before any code change
   or run).
2. `076e922 fix(tuner)`: skip LLM work when no client can serve. A budget refusal no
   longer suppresses the query.
3. `8e68097 perf(collector)`: skip the advisor configuration snapshot when no advisor
   runs.
4. `a3a424c feat(config)`: log the LLM setup notice once per process.
5. `0974d37 fix(config)`: migrate `llm.index_optimizer` only for keys the file sets.
6. `2f5c4da test(config)`: test bug fix (yaml.v3 error text).
7. `f22abba feat(config)`: flip the defaults and doc tags, and regenerate
   `web/src/generated/config_meta.json` and the embedded `internal/api/dist` bundle.
8. `7f04523 test(advisor)`: two test bug fixes (advisor risk expectation; a non-compiling
   func literal).
9. `08cf404 docs`: example configs, configuration, security and costing docs, README,
   reverse spec, CHANGELOG.
10. `6e51d38 test(metrics)` + `f8bce49 fix(metrics)`: `pg_sage_optimizer_enabled` reports
    whether the optimizer can actually run.
11. `fbf5d15`, `7188e50`, `e27f8d5 test(...)`: post-audit tests and their fixture fixes.

Generated artifacts:
- `go run ./cmd/gen_config_meta` regenerated `config_meta.json`.
- `go run ./cmd/gen_config_meta -lifecycle-only -lifecycle-out ../docs/generated/config-lifecycles.md`
  produced no change.
- `npm run build` regenerated the dist. A build of the master source reproduced the
  committed dist byte for byte, so the diff contains only the new metadata.

## Test Results

**Command:** `go test -cover -count=1 -json ./...` (Docker `golang:1.25`, repo root
mounted, PG15 `:55415`, `GEMINI_API_KEY` unset)
**Total:** 8745 passed, 1 failed, 20 skipped

Other runs:
- **Integration** (touched packages, `-tags=integration`, PG15): 3837 passed, 0 failed,
  7 skipped.
- **e2e** (`go test ./e2e -cover -tags=e2e -count=1 -timeout 900s`, PG15): 77 passed,
  0 failed, 13 skipped.
- **Race** (touched packages, `-race`, final head): PG14 (`:55414`) 3760 passed, 0 failed,
  7 skipped (PG14-version and live-LLM skips, all allowlisted), no data races; PG18
  (`:55418`) 3765 passed, 0 failed, 2 skipped (`TestRCAChildProcessFixture`,
  `TestTier2Live_RealGemini`), no data races.
- **Web:** `npm run lint` clean; `npm test` 39 files and 196 tests passed.
- **Lint:** `golangci-lint run ./...` reported 0 issues.

**Coverage (touched packages, PG15):**

| Package | Coverage | Threshold |
|---|---|---|
| internal/config | 88.6% | 70% |
| internal/api | 74.9% | 70% |
| internal/tuner | 82.4% | 70% |
| internal/rca | 95.8% | 70% |
| internal/advisor | 80.0% | 70% |
| internal/optimizer | 79.7% | 70% |
| internal/explain | 87.6% | 70% |
| internal/collector | 85.0% | 70% |
| cmd/pg_sage_sidecar | 71.9% | 70% |
| cmd/gen_config_meta | 86.4% | 50% (tool) |

### Skipped Tests (must be zero or justified)

All are pre-existing and listed in `sidecar/.skip-allowlist`. None is a new test.
- **Live cloud/LLM (opt-in env vars):**
  - agentdb: `TestAWSRDSLiveProvisioning`, `TestCloudSQLLiveProvisioning`,
    `TestLakebaseLiveProvisioning`, three `TestAgentDBLiveGauntlet*`.
  - azure: `TestAzureLive*` ×2.
  - llm: `TestChatWithToolsLive_RealProvider`.
  - rca: `TestTier2Live_RealGemini`.
  - e2e: `TestLLM*` ×6, `TestTunerLLM_*` ×6, `TestOptimizerMultiQueryConsolidation`
    (`SAGE_LLM_API_KEY` not set).
- **PG16+ only on PG15:**
  - autoexplain: `TestCaptureOnDemand_*` ×2.
  - collector: `TestCollectIO`, `TestPhase2_CollectIO_FieldsPopulated`.
  - optimizer: `TestGenericPlanCapturesUnboundParameters`.
- **pg_hint_plan < 1.7 on PG15:** tuner `TestFunctional_Coverage_DB_HintPlanSQLUsesQueryIDSchema`,
  `TestHintRemovalFindings_RetiredAndBrokenOnly`; test/hint_verify `TestHint_HintTableIntegration`.
- **Helper or platform:** rca `TestRCAChildProcessFixture` (child-process helper),
  logwatch `TestResolveLogDir_AbsoluteWindows` (Windows only).

### Failures

- `internal/analyzer`: `TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding` fails:
  "missing slow workload", after a pg_stat_io error.
  - **Pre-existing:** a full-suite run of master `ca515e5` on the same PG15 fails
    identically. It passes in isolation on both master and this branch.
  - **Likely cause:** the test hard-codes `pgVersionNum 170000` on a PG15 server, and its
    `pg_stat_statements` workload is cluster-wide state shared by concurrently running
    packages. Unrelated to this change. Its config sets `Advisor.Enabled=false`
    explicitly.
- **Flaked once, then passed in later full runs and alone:**
  - `internal/analyzer` `TestRegression_WorkMemPromotionCountsDistinctQueries`.
  - `internal/sre` `TestStore_StaleWorkerCannotCommit` and
    `TestStore_PendingFindsQueuedAndOrphanedWork` (DB lease state). Other agents' test
    containers were running at the same time. Neither package's code is touched.

### Coverage Gaps (packages below threshold)

All touched packages meet their thresholds: business logic ≥ 70%, the generator tool
≥ 50%. `cmd/pg_sage_sidecar` (71.9%) is closest to the line.

### Bugs Found This Session

1. **[BUG] Tuner per-candidate failures with no usable client**
   (`tuner/tuner.go` `tryLLMPrescribe`, fixed in `076e922`). With `tuner.llm_enabled`
   set and no LLM configured, every candidate fetched plan and table context, failed
   with "LLM not enabled", logged a line and wrote an `llm_suppression` finding, every
   cycle. With the new defaults this would have hit every deployment without an LLM.
2. **[BUG] Budget refusal muted LLM tuning** (same function, fixed in `076e922`). A
   budget refusal was recorded as an `llm_error` suppression.
3. **[BUG] Deprecated migration overrode an explicit value** (`config/config.go` `Load`,
   fixed in `0974d37`). The `llm.index_optimizer` migration turned
   `llm.optimizer.enabled` back on despite an explicit `false`. Once the default flipped
   it would also have ignored an explicit legacy `false`.
4. **[DEFECT] Catalog queries for an advisor that cannot run**
   (`collector/collector.go`, fixed in `8e68097`). Turning `advisor.enabled` on would
   have run `pg_settings` and full-catalog reloptions queries every collector cycle with
   no consumer.
5. **[DEFECT] Misleading optimizer gauge** (`pg_sage_optimizer_enabled`, fixed in
   `f8bce49`). It echoed the config value, so after the flip it read 1 with no optimizer
   running.
6. **Test bugs.** Each fix is explained in its commit message:
   - yaml.v3 names the line and value, not the key (`2f5c4da`).
   - The advisor risk is SQL-derived, not empty (`7f04523`).
   - A func literal mixed named and unnamed params (`7f04523`).
   - The fixture used a nonexistent user id (`7188e50`).

### Manual Checks

The real binary was built in Docker and run against PG15 (`llmdef_smoke` database) with
a config that has no `llm:` section and no LLM environment. Collector ran at 5 s,
analyzer at 10 s, for 75 s (7 analyzer cycles).
- CHECK-01: PASS. Exactly one `[INFO] [llm] LLM features are on by default but no LLM is
  configured…` line.
- CHECK-02: PASS. No other LLM-related log line, error or warning across 7 cycles.
- CHECK-03: PASS. `GET /health` returned 200 `{"status":"ok"}`.
- CHECK-04: PASS. `/metrics` showed `pg_sage_llm_enabled 0` and
  `pg_sage_llm_tokens_budget_daily 500000`. This run predates `f8bce49` and showed
  `pg_sage_optimizer_enabled 1`; that misleading reading is fixed and covered by
  `TestOptimizerGaugeReportsEffectiveState`.
- CHECK-05: MANUAL. Settings UI rendering of the new defaults needs a browser check. The
  values come from the server and the tooltips from the regenerated `config_meta.json`.

## Post-test audit

1. **What input would break this that I haven't tested?**
   - A whitespace-only `llm.endpoint` counts as "configured". This matches
     `configEnabled`, so the notice stays silent while calls fail and trip the breaker.
     It is pre-existing behaviour and left as is.
   - Configuring an LLM at runtime through the Settings UI reconfigures clients, but
     components built at startup only when `llmOn` (advisor, optimizer, plan narrator,
     RCA LLM, justifier, lint) need a restart. This is pre-existing.
2. **What behaviour is not covered by any assertion?**
   - Fleet and meta-db startup log the notice through the same
     `initializeConfigController`, but only the standalone path is exercised.
   - Per-database `databases[].llm_enabled: false` is covered by existing G5-B07 tests.
3. **Assertions that would pass if the feature were broken?**
   - Each "without LLM → nil/deterministic" test has a paired "with fake provider →
     called/non-nil" test, so an always-off implementation fails.
   - Some existing api tests set a switch to `true` and assert `true`. Their configs are
     literals starting from `false`, so they still detect a broken setter.
4. **Test doubles hiding failure modes?**
   - LLM providers are real HTTP servers (`httptest`) speaking the OpenAI-compatible
     protocol through the real `llm.Client`.
   - Database work runs against real PostgreSQL: suppression rows, collector catalog
     queries, and `sage.config` persisted overrides loaded by
     `initializeConfigController`.

Gaps fixed after the audit:
- `TestPersistedFalseOverridesBeatLLMDefaultsAtStartup`: the real `sage.config` path at
  startup.
- `TestTunerLLMSwitchIsNotAPersistedOverride`: guards the CHANGELOG's "YAML-only" claim.
- `TestTunerSkipsLLMWhileCircuitOpen`.

## Notes for the reviewer

- **Upgrade behaviour:** deployments that already have an endpoint and key but never set
  these switches start making LLM calls after the upgrade. The CHANGELOG states this
  under "Changed (read before upgrading)".
  - Explicit `false` in YAML is honoured (tested per switch).
  - Persisted Settings/API overrides are honoured (tested through `sage.config`).
  - If an older UI ever persisted `llm.enabled=false` for a user, that user stays off.
- **No env var exists** for these switches. The opt-out paths are YAML, the Settings UI
  (`llm.enabled`, advisor, optimizer) and `PUT /api/v1/config/global`, which accepts
  every switch except `tuner.llm_enabled` (YAML-only).
- **`/api/v1/config` `llm_enabled`** still reports the config value (now `true`), not
  whether a provider is configured. The web UI does not read this field;
  `pg_sage_llm_enabled` and `/api/v1/llm` budget status report the effective state.
- **Pre-existing limit violations:** `config.go`, `main.go` and `tuner/tuner.go` already
  exceed 500 lines. New code went into new files where possible (`llm_notice.go`,
  `llm_legacy.go`, `llm_setup_notice.go`, `runtime_metrics.go`). `tryLLMPrescribe` was
  brought under 50 lines.
