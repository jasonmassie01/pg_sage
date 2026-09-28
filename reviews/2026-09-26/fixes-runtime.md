# Fixes — runtime / config / wiring (branch `fix/2026-09-26-runtime`)

Worktree `C:/Users/jmass/pgsr-fix-runtime`, base `733697d`. Process per `00-fix-brief.md`:
regression tests first (`6622dd6`, confirmed failing: new-API tests fail to compile, existing-API
tests were run through a `go test -overlay` and failed for the stated reason), then one fix
commit per logical change.

## Status

| ID | Status | Commit | Test | Notes |
|---|---|---|---|---|
| G5-B01 (P0) | FIXED | ef23e7c | `TestEmergencyStopAll_FailingDatabaseDoesNotAbortLoop` (100 shuffled runs), `..._AgentDatabaseWithoutExecutorIsSkipped`, `TestResume_FailedPersistenceKeepsDatabaseStopped`, `TestResume_RestoresConfiguredExecutorGate`, `TestReplacementInheritsEmergencyStop`, `TestManager_EmergencyStopStrict_PersistenceError` | Latch in memory + `SetExecutorEnabled(false)` first, persist per DB outside the lock (also G5-B27), `*EmergencyStopError` names failed DBs. DBs without an executor (agent DBs) latched, not persisted. Resume only releases DBs whose flag cleared and restores the configured gate. Replacement runtimes inherit the latch. The old PersistenceError test asserted the fail-open behavior and was rewritten (see commit). |
| G5-B02 | FIXED | 821f71f | `TestWatchedConfigApplyInExtensionModeAdvancesGeneration`, `TestEnsureConfigControllerKeepsExistingController` | Controller always built; extension mode keeps config in memory (`configControlPool` returns the monitored pool only in standalone). |
| G5-B03 | DEFERRED | — | — | Needs `internal/api`: router clones `ConfigBase` at construction (`router.go:493`), so main cannot refresh it. Fix for the api owner: take a `func() (*config.Config, error)` base provider and pass `loadConfigCandidate`-without-overrides from main. |
| G5-B04 / G3-B02 | FIXED | f6522ce | `TestLLMDisableReachesFleetAndOptimizerClients`, `TestLLMRotationReachesExistingAndLaterClients` | `llmClientRegistry` is the single `llm` owner; every shared/per-DB/optimizer client is created through or added to it; later clients start from the active config; clients are untracked when their instance context ends. Behavior change: the optimizer tier now also obeys global `llm.enabled=false`. Scenario C (enabling LLM at runtime reports `applied` for hooks never built) DEFERRED: owners cannot report `pending_restart` without a controller API change. |
| G5-B05 | FIXED | a7985bc | `TestGetConnectionString_MigratesLegacyCiphertext` (v1 + v2-legacy fixtures), `TestGetUpdateConnectionString_MigratesLegacyCiphertext`, `TestGetConnectionString_WrongPassphraseStillFails` (integration tag) | `DatabaseStore.WithKeyMigration`; CAS re-encrypt. Key-check row in `crypto_meta` not added (DEFERRED, G5-I13). |
| G5-B06 / G3-B03 | FIXED | 0495cd6, f6522ce | `TestBudgetRegister_*`, `TestBudgetUnregister_*`, `TestBudgetRename_MovesAllocation`, `TestFleetBudgetAdmitsDatabaseAddedAfterStartup`, `TestDurationUntilNextUTCMidnight` | Register on client construction, Unregister on delete/rename, UTC-midnight reset. |
| G3-B14 | PARTIAL | 0495cd6, f6522ce | `TestBudgetSnapshot_ReportsEveryDatabase`, `TestFleetBudgetMetricsExposePerDatabaseSpend` | Prometheus `pg_sage_llm_fleet_budget_{used,allocation}_tokens{database}` and `InstanceStatus.LLMTokensUsed`. `/api/v1/llm/status` + reset still see only the shared manager (api owner). |
| G5-B07 | FIXED | f6522ce | `TestFleetPerDatabaseLLMDisabledIsEnforced` | YAML fleet only; meta mode has no `llm_enabled` column (DEFERRED, needs schema + store). |
| G5-B08 / C08 / G1-B04 | FIXED | 17c7341 | `TestFleetCycleRunsRetentionForInstance` | Per-instance `retention.Cleaner` in YAML-fleet and meta orchestrators. Retention for the other append-only tables is the retention owner's (not in this package). |
| G5-B09 | FIXED | 17c7341, a125edc | `TestRestartRequiresDeclaredSupervisor`, `TestForcedShutdownExitCodeHonoursRestart` | Restart wired only with `SAGE_SUPERVISED=1` (else 501); forced exit keeps 42; compose gets `restart: unless-stopped` + `SAGE_SUPERVISED=1`. |
| G5-B10 / G7-B05 | FIXED | 17c7341 | `TestNotificationControlPoolPrefersMetaDatabase` | One dispatcher per control pool (meta DB, else fleet primary = auth pool); meta instances now have a dispatcher and database name. |
| G5-B11 | DEFERRED | — | — | Needs `internal/executor`: `EnableStandingPolicy` builds `policy.NewStore(e.pool)` internally. Fix: add `EnableStandingPolicyWithStore(ctx, controlPool, profile, databaseID)`; main then passes the meta/primary pool (YAML fleet: after `registerFleetDatabases` so the DB id is known). |
| G5-B12 | FIXED | 17c7341 | `TestUpdateInstanceFindingsMarksUnreachableDatabaseDown` | Query failure + failed ping → `Connected=false`, `Error` set (health 0, degraded); success clears. Staleness decay not added. |
| G5-B13 / G1-B07 | FIXED | 17c7341 | `TestInstanceRuntimeConfigCarriesCapabilityFlags` (+ meta integration tests exercise `RunChecks` on the fixture) | `startup.RunChecks` per YAML-fleet and meta DB; failing DB registers as failed; collector, lint and migration get per-DB version/flags. |
| G5-B14 | DEFERRED | — | — | P2; needs owner warnings in the controller result. |
| G5-B15 | FIXED | b68bb51 | `TestAgentDeploymentToFleetConfig_Defaults` | Default `require`. Cross-area one-line edit in `agentdb_fleet.go`. Existing test asserted `disable`. |
| Legacy crypto (D06) | FIXED | a7985bc | see G5-B05 | `DecryptWithMigration` wired. |
| G2-B15 | FIXED | 17c7341 | — (no practical unit test without the runtime-constructor refactor; verified by reading all three builders) | `With*` before `Run` in standalone, fleet and meta. |
| G7-B09 | FIXED | 17c7341 | `TestInstanceAlertManagerFollowsAlertingFlag` | Alert manager per fleet/meta instance; standalone uses the same builder. |
| G5-B22 | FIXED | ac55fbe, 5288d45 | `TestPostgresDSN_QuotesKeyValueFields`, `TestPostgresDSN_DatabaseURLWins` | `TestDSN_BuildsLibpq` asserted the injectable unquoted string; updated. |
| G5-B27 | FIXED | ef23e7c | covered by B01 tests | DB writes outside `m.mu`. |
| G5-B28 | PARTIAL | ef23e7c, 5ba2198 | — | `shutdownFlag` removed; reconnect loop reads `Stopped` under the lock. `inst.DatabaseID` write after publish (main.go `registerFleetDatabases`) remains. |
| G5-B29 / G10-B06 | FIXED | 6622dd6 | — | `metaLifecycleFixture` uses `testdb.SkipUnlessLive`: without a DSN the 15 cmd DB tests skip in 2.3 s (was 14 FAIL, 212 s); with a DSN they run and pass. `internal/agentdb` failures belong to the agentdb owner. |
| G10-B01 | FIXED | 6aa62a9, a125edc | `TestLoad_PgURLWithoutModeRunsStandalone`, `TestLoad_EnvDatabaseURLWithoutModeRunsStandalone`, `TestLoad_ExplicitExtensionModeIsKept`, `TestLoad_MetaDBWithURLDoesNotInferStandalone`, `TestLoad_NoURLNoModeKeepsDefault` | DSN + no mode → standalone (not with `--meta-db`, G5-B25). Standalone instance identity now comes from the URL. Without a DSN the default is still `extension`, so `TestConfigDefaults`/`TestDefaultConfig_NonZeroFields` keep asserting it; `TestConfigDefaults` now clears `SAGE_DATABASE_URL`. Compose pins `SAGE_MODE=extension` (it targets the C-extension DB). |
| G10-B02 | FIXED | 3f7ea06 | `TestShippedConfigExamplesLoad` | Root example fixed in place (goreleaser still ships it). |
| G10-B03 | FIXED | e1c7099 | — (CI) | Dockerfile `VERSION/COMMIT/DATE` args; metadata-action semver tags; `:latest` only on `v*`. Multi-arch not added. |
| G10-B04 | FIXED | cb9148e | — | |
| G10-B05 | FIXED | 373706f | — | Job-level `HAS_TRAFFIC_TOKEN` guard. |
| G10-B09 | FIXED | 6aa62a9 | `TestUnexpandedEnvWarnings_IgnoresCommentsAndNamesFile`, `TestUnexpandedEnvWarnings_WrittenDuringLoad` | Six zero-assertion tests deleted (G10-B13/B15). |
| G10-B10 | FIXED | 6aa62a9 | `TestLoad_UnknownFlagFails` | |
| G10-B11 | FIXED | 17c7341 | `TestModeGaugeValue` | 0/1/2. |
| G10-B12 | FIXED | 7de6629, 832a8b6 | — | **Before merging into the main checkout, back up `local_monitor_config.yaml`: git deletes the tracked copy.** |
| G10-B19 | FIXED | e1c7099 | — | `-race` on the ubuntu unit step. Not runnable locally (no cgo); pre-existing races such as the `DatabaseID` write (G5-B28) may surface in CI. |
| G10-B22 | FIXED | e1c7099, d23f520 | `TestWave5GolangCILintUsesPinnedV2Contract`, `TestWave5CIUsesDesignatedParallelDatabaseFixtures` | lint job moved into ci.yml; test.yml deleted. |
| G5-D01/D02 | FIXED (deleted) | 8537f32 | — | `applyHotReload`, `NewWatcher`, `NewWatcherWithLoader` + 16 tests of applyHotReload. `Watcher.Current` kept (test probe). |
| G5-D04 | NOT A BUG | — | — | `NewConfigController` is used by `internal/api` tests; kept. |
| G5-D07 | DEFERRED | — | — | `EnsureDatabasesTable` is still called by `internal/api/coverage_phase2_test.go`; delete after the api branch drops that call. |
| G5-D11 | FIXED (deleted) | 5419430 | capability tests call the internal builder | |
| G5-B16, B17, B18, B19, B20, B21, B23, B24, B25, B26, B30 | DEFERRED | — | — | P2/P3 or PLAUSIBLE; not cheap without the runtime refactor or live repro. |

## Cross-area edits

- `sidecar/cmd/pg_sage_sidecar/agentdb_fleet.go` (agentdb owner): sslmode default `disable` →
  `require` (one hunk, G5-B15).
- `sidecar/cmd/pg_sage_sidecar/metadb.go` reconnect loop reads `fleetMgr.InstanceStopped(inst)`.
- No library package outside my ownership was changed. `startFleetRolloutScheduler()` in
  `initFleetAndAPI` was not touched.
- For the api owner: the emergency-stop handler still maps any error to 500 "failed to
  persist"; `errors.As(err, *fleet.EmergencyStopError)` gives per-DB results (every target is
  already stopped in memory). G5-B03 and G3-B14 status/reset also need api changes.

## Merge notes

- `main.go` wiring for B04/B06/B07 and for B08/B10/B12/B13/G7-B09/G2-B15 landed as two commits
  (`f6522ce`, `17c7341`) because the fixes interleave inside `initFleetMultiDB`. New helpers
  live in new files (`llm_client_registry.go`, `fleet_orchestrator.go`, `notify_runtime.go`,
  `runtime_metrics.go`) to keep `main.go` hunks small; `fleetDBOrchestrator` moved out of
  `main.go`.
- Files I edited were rewritten with LF endings (repo uses `autocrlf=true`); diffs are content
  only.

## DEFERRED design: single per-database runtime constructor (G5-I07)

The three builders (`initStandalone`, `initFleetMultiDB` loop body, `buildStoreDatabaseRuntime`)
still drift; this pass patched each. Proposed shape:

```go
type runtimeSpec struct {
    Name       string
    DatabaseID *int
    Pool       *pgxpool.Pool
    Config     config.DatabaseConfig     // per-DB overrides (llm_enabled, trust, mode)
    Checks     *startup.CheckResult      // RunChecks result -> instanceRuntimeConfig
    Control    *pgxpool.Pool             // notifications, policy, auth (meta or primary)
    Parent     context.Context
}
func buildDatabaseRuntime(spec runtimeSpec) (*fleet.DatabaseInstance, error)
```

built from <= 50-line feature builders (`buildCollector`, `buildLLM` -> registry,
`buildAnalyzer` (configured before Run), `buildExecutor` (policy store on `Control`),
`buildRetention`, `buildAlerting`, `buildLogwatch`, `buildLintAndMigration`), each registering
its goroutines with `startInstanceWorker`. Standalone becomes "one spec, Control = Pool"; YAML
fleet adds a reconnect source (G5-B19); agent DBs get either the full runtime or a
visibility-only card (G5-B16). This also moves `main.go` under 500 lines.

## Test Results

**Command:** `go test -count=1 -cover -v <pkgs>` and the same with `-tags=integration`,
`SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`,
pkgs = `./cmd/pg_sage_sidecar ./internal/config ./internal/fleet ./internal/store
./internal/crypto ./internal/startup ./internal/schema`. Also `go build ./...`, `go vet ./...`,
`go vet -tags=integration ./...`: clean.

**Total (unit):** 871 passed, 0 failed, 0 skipped
**Total (integration tag):** 921 passed, 0 failed, 0 skipped
**Without a DB DSN:** `./cmd/pg_sage_sidecar` 15 skipped in 2.3 s, 0 failed (was 14 FAIL, 212 s).

**Coverage (unit / integration):**

| Package | Unit | Integration | Threshold |
|---|---|---|---|
| cmd/pg_sage_sidecar | 47.7% | 47.7% | 70% — BELOW |
| internal/config | 86.3% | 86.3% | ok |
| internal/fleet | 79.7% | 79.7% | ok |
| internal/store | 74.0% | 76.4% | ok |
| internal/crypto | 86.4% | 86.4% | ok |
| internal/startup | 92.2% | 92.2% | ok |
| internal/schema | 82.4% | 82.4% | ok |

### Skipped Tests
- None with the fixture DSN set.

### Failures
- None.

### Coverage Gaps
- `cmd/pg_sage_sidecar` 47.7% (baseline 24.5% without DB / 43.5% in CI): `initStandalone`,
  `initFleetMultiDB`, `main`, `handleMetrics` need live processes; the runtime-constructor
  refactor above is the way to make them unit-testable.

### Bugs Found This Session (beyond the assigned list)
1. [BUG] config/fleet.go normalize — standalone with a DSN URL registered the instance as
   `localhost`/`postgres` instead of the URL's host and database (fixed in 6aa62a9).
2. [BUG] fleet/manager_test.go — `TestManager_EmergencyStopStrict_PersistenceError` enshrined a
   fail-open kill switch (rewritten in 6622dd6).
3. [BUG] llm optimizer clients ignored global `llm.enabled=false` (fixed in f6522ce).

### Manual Checks Remaining
- CHECK-01: MANUAL — `docker compose up`, click Restart now, confirm the sidecar returns.
- CHECK-02: MANUAL — CI run of the new `-race` unit step and the docker metadata tags on a `v*` tag.
