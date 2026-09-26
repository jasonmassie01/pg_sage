# Fix report: cross-area integration (2026-09-26)

Branch `fix/2026-09-26-integration` (worktree `C:/Users/jmass/pgsr-fix-integration`), on
top of the 8 merged fix branches. Sources: the "Cross-area edits" and follow-up sections of
every `fixes-*.md` in this directory. Process: failing tests first (`4c47000`), then one
commit per logical fix.

Excluded packages (other agents are active there) were not edited: `internal/executor`,
`autonomy`, `verify`, `policy`, `rca`, `analyzer`, `collector`, `querystore`, `agentdb`,
`api/agent_db_*.go`.

## Status

| ID | Status | Commit | Test name(s) | Notes |
|---|---|---|---|---|
| G5-B03 | FIXED | 36f57d8, ae5fd3a | `api/TestGlobalDeleteRebasesOnCurrentFileConfig`, `api/TestGlobalDeleteFailsClosedWhenFileConfigUnreadable`, `cmd/TestFileConfigBaseLoaderReadsCurrentYAML`, `cmd/TestAPIServerWiresRuntimeRegistries` | New `RuntimeDeps.ConfigBaseLoader`. The sidecar passes `loadFileConfigBase` (YAML + env + flags, no overrides, plus the runtime facts detected at startup). Deletes rebuild from the current file plus the remaining overrides. If the base cannot be loaded (for example invalid YAML), the delete returns 500 and publishes nothing. Checked by mutation: with the startup base, the test fails with "re-escalated live trust to autonomous". |
| G5-B14 | FIXED | d9595dd | `cmd/TestMetaGlobalTrustDowngradeReachesEveryDatabase`, `…RaiseNeverEscalatesAndWarns`, `…PersistFailureRollsBack`, `cmd/TestYAMLFleetTrustOwnerHasNoMetaPersistence`, `config/TestApplySurfacesOwnerWarningsAfterCommit`, `store/TestSetDatabaseTrustPolicy*` (integration) | **Product decision (safer option).** In meta-db mode the global `trust.level` is now a ceiling. A downgrade lowers every database above it, in memory and durably (`sage.databases.trust_level`, audited, legacy overrides removed), and rolls back fully on failure. A raise never escalates a database: the API response carries a warning ("N databases were not raised"). The controller gains an optional `config.WarningReporter`. YAML-fleet semantics are unchanged: explicit per-database trust still wins. |
| G3-B14 | FIXED | 2704b29, 9a1425d | `api/TestLLMStatusCoversEveryRegisteredClient`, `api/TestLLMBudgetResetReachesEveryRegisteredClient`, `api/TestLLMStatusFallsBackToManagerAndEmptyState`, `cmd/TestLLMBudgetStatusAndResetCoverEveryClient`, `cmd/TestLLMBudgetStatusIncludesAndResetsFleetBudget`, `cmd/TestLLMBudgetStatusDropsRemovedClients`, `llm/TestStatusOfMatchesManagerStatus`, web `renders an exhausted per-database client` | New `api.LLMBudgetRegistry` (`*llm.Manager` still satisfies it as the fallback), passed as `RuntimeDeps.LLMBudgets`. The sidecar's client registry reports and resets `general`, `optimizer`, `<db>/general`, `<db>/optimizer` and `<db>/fleet_budget`. The web banner now lists every exhausted client. |
| RCA retention (substrate-B7) | FIXED | aef37a1 | `retention/TestRun_PrunesIncidentsByResolutionAge`, `…IncidentPruneDisabledByZeroWindow`, `…IncidentPruneFailureIsLogged` | `Cleaner.Run` calls `rca.PruneResolvedIncidents(ctx, pool, findings_days)`. This reuses `retention.findings_days`, so no new key. The generic rule, which keyed on `last_detected_at`, is removed: it deleted incidents resolved minutes earlier. `incidents` is now in the exemption list with that reason. It runs per instance in all modes, because the cleaner is already wired in standalone, fleet and meta. |
| Meta-db notification dispatcher (rca B14) | FIXED | c126a71 | `cmd/TestMetaRuntimeWiresRCANotifications` | Meta already gave the shared control-pool dispatcher to the analyzer and executor. The RCA engine was the missing consumer. Meta file logwatch (the other half of rca B14) was not assigned. |
| G3-B15 | FIXED | dd69cbe | `cmd/TestStandaloneLLMManagerUsesDedicatedOptimizerClient`, `…TunerFallbackNeverRetriesSameClient`, `…TunerLLMClientsNilManager`, `cmd/TestStandaloneUsesSharedNotifyDispatcher` (AST) | Standalone builds its manager with the registry-tracked optimizer client. The index optimizer and the tuner share it. The tuner fallback is nil when it would equal the primary. Fleet-global and meta-global managers: **NOT NEEDED**. They serve only `General` (explain, blueprint), and per-database managers already carry the optimizer. |
| G7-B20 | FIXED | d2e3c3d | `cmd/TestMetaNotificationSecretEncryptedAtRestAndDelivered`, `cmd/TestNotificationSecretKeyDerivation` | The router's channel store and test dispatcher, and every runtime dispatcher (one shared per control pool: standalone, YAML fleet, meta), use the same key. The key is the meta encryption key, or in standalone / YAML fleet `--encryption-key` derived with the control database's persisted KDF salt. Extension mode stays unsealed, so it never writes sidecar rows into the C extension's schema. End-to-end test: a channel created through the API is sealed at rest, and the runtime dispatcher still delivers to the webhook. Checked by mutation: with a nil router key, the test fails with "stored in plaintext". |
| G7-B21 (policy key) | FIXED | 98c914f, d2e3c3d | `config/TestNotificationPolicyAllowPrivateTargets*`, `store/TestNotificationPolicyIsNotAnAPIOverride`, `cmd/TestNotificationPrivateTargetRefusedByDefault` | Key is **`notification_policy.allow_private_targets`**, default false, YAML-only, restart. The top-level `notifications:` key is rejected by the loader as retired, so it could not be reused. It deliberately does NOT follow the API-override part of the 7-place lesson: an admin session must not be able to reopen SSRF to private networks. It has a consistency-test exclusion and a guard test. It is wired into the struct, defaults, lifecycle (restart), `config_meta.json`, the lifecycle reference, `config.example.yaml`, the router store/dispatcher and every runtime sender. |
| G3-B07 (lint, explain) | FIXED | c543d12 | `lint/TestBuildUserPrompt_DelimitsAndRedactsQueries`, `explain/TestEnhanceWithLLM_PromptDelimitsAndRedactsUntrustedText`, `explain/TestExplainUserPrompt_EmptyPlan` | Object names, query text and plan JSON are passed through `llm.UntrustedData` / `llm.SanitizePromptSQL`, and `llm.UntrustedDataRule` is added to both system prompts. |
| G3-B07 (justifier) | DEFERRED | — | — | The justifier lives in `internal/executor/justify.go` (excluded package). `analyzer/plan_narrative.go` and `rca/tier2.go` are also excluded. |
| G3-B28 | FIXED | c543d12, c1a92fa | `lint/TestParseLLMJsonbResponse_BlankIsEmptyResponseError`, `…RepairsTruncatedArray` | The JSONB enhancer parses with `llm.ParseJSON(raw, llm.JSONArray, …)`. The local fence stripper and its table test are removed. |
| G2-B24 leftover | FIXED | 94d136e | `cases/TestIncidentCandidateExpiryAnchoredToLastDetection`, `…StableAcrossReads`, `…FallsBackToDetectedAt`, `…QueryHintCandidateExpiryAnchoredToHintEvidence`, `…WithoutEvidenceTimeIsBounded` | Incident candidates expire from `last_detected_at` (else `detected_at`). Query-hint candidates expire from the latest of rollback, verification or creation time. Only a completely unknown time falls back to now. The API already selects and maps these columns, so no `cases_handlers.go` change was needed. |

## Test fixes (phase-1 tests corrected, with reasons)

- `TestGlobalDeleteRebasesOnCurrentFileConfig` asserted a restart-bound override on the
  *active* snapshot. Such an override lands in *desired*, so the assertion now reads desired.
  The trust assertion stays on active.
- The API reset tests posted without `Content-Type: application/json` and got 415 from the
  middleware. They now send the header, as the dashboard does.
- The config-key test used `notifications:`, a key the loader rejects as retired. It now uses
  `notification_policy:` (file renamed).
- Existing call sites were updated mechanically for changed signatures:
  `configGlobalDeleteHandler` and `registerConfigRoutesRuntime` now take a base source, and
  `registerNotificationRoutes` takes route deps.

## Cross-area edits

All edits are outside the excluded packages. Files touched: `cmd/pg_sage_sidecar/*`
(`main.go`, `wire.go`, `metadb.go`, `notify_runtime.go`, `config_trust_owner.go`, new
`config_base_runtime.go`, `llm_budget_status.go`, `llm_standalone.go`), `internal/api`
(`router.go`, `config_handlers.go`, `llm_handlers.go`, new `config_base.go`),
`internal/config` (`config.go`, `defaults.go`, `controller.go`), `internal/store` (new
`config_trust_policy.go`), `internal/llm/manager.go` (`StatusOf`), `internal/retention`,
`internal/cases`, `internal/schema/lint`, `internal/explain`,
`web/src/components/TokenBudgetBanner.jsx`, `web/src/generated/config_meta.json`,
`docs/generated/config-lifecycles.md` and `config.example.yaml`. `internal/api/dist` was
**not** rebuilt. The web build was verified into a scratch directory.

## Known limitations and follow-ups

- If the KDF salt cannot be read in standalone / YAML fleet mode, the notification key is
  nil. The error is logged at ERROR. New channels are then stored unsealed, and sealed
  channels fail with an explicit "encrypted but no key" error. Meta mode fails at startup
  instead, as before.
- Meta trust ceiling: a per-database PUT can still raise one database above the global level.
  That is an explicit per-database decision. The ceiling applies when the global level changes.
- The following functions were already over 50 lines and grew by 1–2 lines:
  `NewRouterFullRuntime` (119→121), `registerConfigRoutesRuntime` (69→70),
  `registerNotificationRoutes` (62→63) and `ConfigController.applyLocked` (65→66).
  `applyControlledGlobalDelete` is kept at 50.
- Still open, and outside my packages: G3-B07 in `executor/justify.go`,
  `analyzer/plan_narrative.go` and `rca/tier2.go`, plus meta-db file logwatch.

## Test Results

**Commands** (env: `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`, `GEMINI_API_KEY` unset):
- `go build ./...`, `go vet ./...`, `go vet -tags=integration ./...`: clean.
- `go test -count=1 -cover -v <pkgs>`, then the same with `-tags=integration`, where
  `<pkgs>` = `./cmd/pg_sage_sidecar ./cmd/gen_config_meta ./internal/api ./internal/cases
  ./internal/retention ./internal/schema/lint ./internal/explain ./internal/config
  ./internal/store ./internal/llm ./internal/notify`. `api`, `cases` and `cmd` were re-run
  on the final HEAD after the size refactor.
- Web (`sidecar/web`): `npm ci`, `npm run lint`, `npm run test -- --run`, and
  `vite build --outDir <scratch>`. `internal/api/dist` was not rebuilt.

**Total:** unit 2284 passed, 0 failed, 0 skipped. Integration 2381 passed, 0 failed,
0 skipped. Web: 26 files, 131 tests passed. Lint clean. Build OK.

**Coverage** (integration run):

| Package | Coverage |
|---|---|
| cmd/pg_sage_sidecar | 50.5% (baseline 48.6%) |
| cmd/gen_config_meta | 86.4% |
| internal/api | 73.7% |
| internal/cases | 89.5% |
| internal/retention | 97.6% (unit 100%) |
| internal/schema/lint | 77.1% |
| internal/explain | 94.4% |
| internal/config | 86.3% |
| internal/store | 77.6% |
| internal/llm | 89.1% |
| internal/notify | 90.5% |

### Skipped Tests
- None. `grep -- '--- SKIP'` returns 0 for both runs.

### Failures
- None on the final runs. One earlier run had a `config/TestWatcherStopCannotLoseCancellationDuringBlockedCallback`
  timeout, which passed on every re-run. It is in watcher code I did not touch, and it looks
  like a timing flake under load.

### Coverage Gaps
- `cmd/pg_sage_sidecar` is at 50.5%, below the 70% business-logic floor. This is the
  process-wiring `main` package (a 2.8k-line `main.go`); coverage is up from 48.6%. Every
  helper added here is covered by unit or DB tests.
- Every other touched package meets its threshold.

### Bugs Found This Session
1. [BUG] api/config_handlers.go: DELETE rebuilt from the startup config and reverted YAML trust downgrades (G5-B03).
2. [BUG] cmd/config_trust_owner.go: a global trust change in meta-db mode was a no-op reported as applied (G5-B14).
3. [BUG] api/llm_handlers.go: LLM status and reset only saw the shared client (G3-B14).
4. [BUG] retention/cleanup.go: incidents resolved minutes earlier were purged because the rule keyed on `last_detected_at`.
5. [BUG] cmd/metadb.go: the meta-db RCA engine had no notification dispatcher.
6. [BUG] cmd/main.go: the standalone tuner used the general client, and its fallback was that same client (G3-B15).
7. [BUG] api/router.go and cmd dispatchers: channel secrets were never sealed, and the target policy was never configurable (G7-B20, G7-B21).
8. [BUG] schema/lint and explain: raw SQL, literals and plans were sent to the LLM (G3-B07). The lint parser did not use `llm.ParseJSON` (G3-B28).
9. [BUG] cases: incident and query-hint candidate expiry was recomputed from now() on every read (G2-B24).

### Manual Checks Remaining
- CHECK-M1: MANUAL. In a live meta-db deployment, PUT a global `trust.level` downgrade and then
  a raise. Confirm the dashboard shows the lowered per-database levels and the raise warning.
- CHECK-M2: MANUAL. Deliver through a real internal SMTP relay with
  `notification_policy.allow_private_targets: true`.
