# RCA / incident lifecycle fixes (2026-09-26)

Branch `fix/2026-09-26-rca`, worktree `C:/Users/jmass/pgsr-fix-rca`, base `9a3cac7`.
Process: failing regression tests first (`bc5e80b`, `472e8cf`), then the fixes. Before the
fixes, every Phase-1 test failed for the reason it targets. Tests that needed new APIs were
checked with a temporary, uncommitted shim (the checks are listed in `bc5e80b`).

## Status

| ID | Status | Commit(s) | Test(s) | Notes |
|---|---|---|---|---|
| R04: one owner of incident state | FIXED | dfb2a6f, a6f88fa, a83e46c, 70e81f6 | `TestLifecycle_ManualResolveSurvivesNextCycle`, `TestLifecycle_HydrateKeepsIncidentIdentityAcrossRestart`, `TestLifecycle_HydrateAdoptsLegacyRowsWithoutDatabase`, `TestLifecycle_ResolveRacingPersistIsNotOverwritten`, `TestLifecycle_SupersededAfterGapPersistsLink`, `TestRCAAdapterStampsIdentityAndPersists`, `TestRCAAdapterSkipsCycleWhenStateCannotLoad` | `Engine.Hydrate` loads this database's open incidents (and adopts legacy rows with no database). Every cycle first syncs rows that were resolved in the DB. Persistence is insert, or a CAS update (`WHERE resolved_at IS NULL`), so the engine never overwrites a DB resolution. A recurrence after resolve is a new row with `previous_incident_id` (found by `identity_key`), not a reopen. An open match older than the dedup window is resolved `pg_sage:superseded` and linked. `rca.ResolveIncident` is the only operator transition. The adapter skips a cycle if state cannot load. |
| SURF-19: resolution reason dropped | FIXED | a6f88fa, a83e46c | `TestIncidentResolveHandlerPersistsActorAndReason`, `…AlreadyResolvedIsConflict`, `…RejectsOverlongReason`, `TestIncidentDetailHandlerReturnsLifecycleFields`, `TestResolveIncident_PersistsActorReasonAndErrors` | Adds `resolved_by` (`user:<email>`, `api`, `pg_sage:auto_resolve`, `pg_sage:superseded`) and `resolution_reason` (max 2000 chars, else 400). Resolving an already-resolved incident returns 409. Detail and list now return `rollback_sql`, `resolved_by`, `resolution_reason` and `previous_incident_id`. |
| substrate-B5: no rehydrate | FIXED | dfb2a6f | (see R04) | |
| R05: incident identity | FIXED | dfb2a6f, 9365c87, 70e81f6 | `TestAnalyze_StampsDatabaseNameOnEveryIncident`, `TestAnalyze_WithoutDatabaseNameSkipsSelfActionMatch`, `TestSelfAction_EmptyDatabaseNeverMatches`, `TestDatabaseMatches` | `WithDatabaseName` stamps every tree, log, LLM and self-action incident when it is built. The stored name is the runtime name: standalone `resolveDBName()`, fleet alias, meta-db `rec.Name`. An empty name never matches. Actions from the per-database action store get the engine's name. The provenance half of R05 (fixed 0.6 confidence, evidence hashes, model/prompt provenance) is **DEFERRED**. |
| substrate-B2: Tier 2 unreachable | FIXED | dfb2a6f, fd09c56 | `TestTier2_FiresOnRealCoOccurringSignals`, `TestTier2_NoCallWhenEverySignalExplained`, `TestTier2E2E_HappyPath` (now uses real detectors), `TestPlanTier2_LowConfidenceSignalsAreCandidates` | New trigger: at least `llm_correlation_threshold` (default 3) signals co-occur, and at least one is unexplained. Unexplained means no Tier 1 tree used it, or only a tree with confidence below 0.7 did. This matches the existing config doc ("minimum concurrent signals"). The e2e test now drives `idle_in_tx_elevated`, `wal_growth_spike` and `lock_contention` from snapshots. The LLM incident's identity is the unexplained signals only. |
| substrate-B11: self-action family mismatch | FIXED | 9365c87 | `TestSelfAction_MapsExecutorActionFamilies`, `TestSelfAction_RollbackCountUsesMappedFamily` | The executor emits these families (`categorizeAction`, `internal/executor/executor.go:1271`): `create_index`, `drop_index`, `reindex`, `vacuum`, `analyze`, `terminate_backend`, `alter`, `ddl`. `causalFamily` maps `vacuum` + `VACUUM FULL` / `VACUUM (… FULL …)` to `vacuum_full`, and `alter`/`ddl` + `SET`/`RESET work_mem` to `set_work_mem`. This covers causal matching and rollback counting. All 5 causal paths can now match. Attaching `RecentSageActions` to every incident is **DEFERRED** (not assigned). |
| substrate-B3: incidents never notify | FIXED (standalone and static fleet) | 9fbe7f2, dfb2a6f, 70e81f6 | `TestLifecycle_NotifiesDetectedEscalatedResolvedOnce`, `TestLifecycle_ManualResolveEmitsResolvedWithActor`, `TestLifecycle_DispatchFailureIsLoggedNotFatal`, `TestLifecycle_PersistFailureKeepsStateAndRetries`, `notify/TestIncident*` | New events `incident_detected`, `incident_escalated` and `incident_resolved`. Each is sent once, only after the state change is durable. The resolved event keeps the incident's severity, so the rule that delivered the page also delivers the resolution. Meta-db mode still has no dispatcher (B14, out of scope). |
| substrate-B7: unbounded memory and table | FIXED (memory); retention hook provided | dfb2a6f | `TestLifecycle_ResolvedIncidentsLeaveMemory`, `TestDedup_InMemoryIncidentsBounded`, `TestDedup_CapWarnsOnce`, `TestTrimResolvedOverflow_DropsOldestResolved`, `TestPruneResolvedIncidents_DeletesOnlyOldResolved` | Resolved incidents leave memory once their resolution is durable. Open incidents are capped at 500, with one warning when the cap is hit. When nothing can be persisted, unpersisted resolved incidents are trimmed above 1000. **Hook:** `rca.PruneResolvedIncidents(ctx, pool, retention) (int64, error)` deletes in batches, never deletes open rows, and rejects retention ≤ 0. The FK uses `ON DELETE SET NULL`. **Calling it from `internal/retention/cleanup.go` is left to the retention/collection owner** (suggest the findings retention of 180 d). |
| G3-B17: LLM call under `Engine.mu` | FIXED | dfb2a6f | `TestTier2_LLMCallDoesNotHoldEngineMutex`, `TestTier2_CanceledCallerContextSkipsLLM`, `TestTier2_DedupBeforeCallingLLMAgain` | A cycle is now plan (under `mu`) → LLM call and action-store reads (no lock, caller ctx, 30 s cap) → commit (under `mu`). `cycleMu` serializes cycles. Before calling, the engine checks for an open LLM incident with the same unexplained signals and re-observes it instead of calling. The adapter passes the runtime lifecycle context. |
| G3-B27: positional chain; `"; "`-joined SQL | FIXED | dfb2a6f | `TestTier2_ChainStepsNotAttributedByIndex`, `TestTier2_StructuredStepsKeepOnlyRealSignalIDs`, `TestTier2_RecommendedSQLNeverJoined`, `TestParseCausalChainString_*` | The prompt asks for `causal_steps` with `{signal, description}`. A step keeps its signal only if it is a real input signal ID; in the legacy arrow string, only if the step names one. Recommended SQL is the first single statement (trailing `;` trimmed). An entry with an embedded `;` is rejected. Also, because Tier 2 is now reachable, the prompt drops `detail`/`user` metrics and redacts literals in `message`/`query` (`TestTier2_PromptRedactsLogFreeText`). |
| G1-B11: fleet log fanout without DB filter | FIXED (in RCA, all modes) | dfb2a6f | `TestLogSignals_FilteredByLogDatabase` | Log signals whose `database` metric differs from the engine's `current_database()` (learned in `Hydrate`, or set with `WithLogDatabase`) are dropped. Lines with no database (cluster-wide events) are kept. `logwatch/fanout.go` itself is unchanged: it still copies every signal to each subscriber buffer. |
| G1-B25: startup log replay re-fires | FIXED | dfb2a6f | `TestLogSignals_IgnoreReplayOlderThanStart` | Ignores log lines timestamped before engine start minus 5 min. The 5 min tolerates clock skew between the PG server and the sidecar. It is configurable in code with `WithLogReplayCutoff` (zero disables it). A YAML key is **DEFERRED** to the config owner so this branch does not touch `internal/config`. With hydration, a replayed line for a still-open incident deduplicates anyway. |
| Cache-hit unit (coordination) | FIXED | 313a7f3 | `TestDetectCacheHitDrop_AcceptsPercentAndFraction`, `TestNormalizeCacheHitRatio_Invalid` | Values above 1 are divided by 100. Values ≤ 0 mean no data and do not fire. Values above 100% are invalid. On the old code the percent input (99.x ≥ 0.95) meant `cache_hit_ratio_drop` **never fired**, and a COALESCEd 0 fired a false critical signal. |
| substrate-B1: detection latency (RCA only on the 600 s analyzer tick) | DEFERRED | — | — | See the design below. |
| G1-B13: RCA drains logs once per 600 s; tailer queue cap | DEFERRED | — | — | Same root cause as B1. Resolved by the fast path below. |

### DEFERRED design: detection latency (B1, G1-B13)

This is not a small, safe change behind existing config. `resolution_cycles`,
`escalation_cycles` and the grace period count *analyzer cycles*. Running the same `Analyze` on
the 60 s collector tick would silently change "escalate after 5 cycles" from 50 min to 5 min.
The lock-chain input (`DetectLockChains`) also lives in the analyzer, which this branch does
not own. Proposed design:

1. Convert the cycle counts to durations once (`cycles × analyzer interval`), and base
   auto-resolve and escalation on `LastDetectedAt` and wall-clock time, not call counts.
2. Add a fast-path ticker at the collector interval that calls
   `Engine.AnalyzeContext(ctx, coll.LatestSnapshot(), coll.PreviousSnapshot(), cfg, nil)`.
   `cycleMu` already makes concurrent callers safe. It drains logs every tick, which also
   keeps the tailer queue below its 10,000-line cap (G1-B13).
3. Have the analyzer export a cheap lock-chain probe so the fast path can pass
   `lockChainFindings`.
4. Gate the fast path with the existing `rca.enabled`. Keep Tier 2 on the slow tick to protect
   the LLM budget.

## Cross-area edits

- `sidecar/cmd/pg_sage_sidecar/main.go`: the standalone and static-fleet adapter construction
  now uses `newRCAAdapter(ctx, eng, pool, dbName, logFn)`, and both call
  `rcaEng.WithDispatcher(...)` next to `exec.WithDispatcher`.
- `sidecar/cmd/pg_sage_sidecar/metadb.go`: one-line switch to `newRCAAdapter`.
- `sidecar/internal/notify/notify_extended_test.go`: the exact-set event test now includes the
  3 new types.
- `sidecar/internal/api/coverage_gaps_test.go`: it called the removed `resolveIncident` helper
  and now uses `rca.ResolveIncident`. `incident_resolve_test.go`: its incident-only fixture
  schema now mirrors the new columns.

## Follow-ups for other owners

- **retention**: call `rca.PruneResolvedIncidents` (see B7).
- **notify/pagerduty.go**: the dedup key is `type:database`, so all incidents in one database
  collapse into one PagerDuty incident, and `incident_resolved` is sent as a `trigger`. Use
  `Data["incident_id"]` in the key, and send `event_action: resolve` for `incident_resolved`.
- **web**: `web/src/pages/notifications/shared.jsx` does not list the `incident_*` event types.
  The incident UI should show `resolved_by`, `resolution_reason` and `previous_incident_id`.
- **meta-db mode**: there is no notify dispatcher and no file logwatch (B14).
- **config**: add an optional YAML key for the log replay cutoff.
- Pre-existing lint items not touched: `rca/self_action.go` causalPaths lines are over 100
  chars, `rca/tier2_live_test.go` is not gofmt-clean, and `migrateIncidentConstraints` was
  already over 50 lines (it gained 3).

## Test Results

**Command:** `go build ./... && go vet ./... && go vet -tags=integration <pkgs>`, then
`go test -count=1 -cover -v <pkgs>` and `go test -count=1 -cover -tags=integration -v <pkgs>`,
where `<pkgs>` = `./internal/rca/ ./internal/notify/ ./internal/schema/ ./internal/api/
./internal/store/ ./cmd/pg_sage_sidecar/`, run with
`SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`.

**Total:** unit run: 2108 passed, 0 failed, 0 skipped. Integration-tag run: 2018 passed,
0 failed, 0 skipped. These counts are `--- PASS` lines, subtests included. Build and vet are
clean.

**Coverage (unit / integration; baseline in parentheses):**
- `internal/rca`: 96.1% / 96.3% (96.2% / 97.9%)
- `internal/notify`: 88.7% / 88.7% (88.1%)
- `internal/schema`: 81.8% / 81.8% (82.4%)
- `internal/api`: 71.9% / 72.4% (71.9% / 72.4%)
- `internal/store`: 75.3% / 75.4% (unchanged)
- `cmd/pg_sage_sidecar`: 43.7% / 43.7% (43.5%)

### Skipped Tests (must be zero or justified)
- None. Output was grepped for `SKIP`.

### Failures
- None.

### Coverage Gaps (packages below threshold)
- `cmd/pg_sage_sidecar`: 43.7%. This was below threshold before this branch (43.5% baseline).
  It is the `main` wiring package, and most of it needs a live multi-mode runtime. The new
  adapter is covered by 2 tests. All other touched packages meet the 70% threshold.

### Race detection
- `-race` could not run: the host has no gcc (cgo). Concurrency is covered functionally by
  `TestTier2_LLMCallDoesNotHoldEngineMutex` and `TestTier2E2E_ConcurrentAnalyze`.

### Bugs Found This Session
1. [BUG] `rca/rca.go` `PersistIncidents`: the upsert wrote `resolved_at = EXCLUDED.resolved_at`
   and reopened manual resolutions every cycle (R04).
2. [BUG] `api/handlers_v09.go` `resolveIncident`: the reason was discarded, the actor was not
   recorded, and "already resolved" was reported as 404 (SURF-19).
3. [BUG] `rca/tier2.go`: the trigger counted only uncovered signals, which can never reach 3
   (B2). The LLM call ran under `Engine.mu` with `context.Background()` and no dedup
   (G3-B17). Chain steps were attributed by array index, and SQL was joined with `"; "`
   (G3-B27).
4. [BUG] `rca/self_action.go`: an empty database acted as a wildcard (R05), and 3 of 5 causal
   paths were dead because the family names did not match (B11).
5. [BUG] `rca/signals.go`: comparing a percent value with a fractional threshold meant
   `cache_hit_ratio_drop` never fired, and "no data" (0) fired a false critical signal.
6. [BUG] The old upsert sent `''` for an empty `action_risk`, which the table CHECK rejects.
   It is now sent as NULL.
7. [BUG, found while fixing] The Tier 2 prompt would have sent raw log `detail`/`user`/query
   literals to the LLM once Tier 2 became reachable. The prompt now redacts them.

### Post-test audit
- Added in `943ed07`: supersede after a detection gap (unit and DB), within-window bump, the
  cap warning firing once, overflow trimming, low-confidence Tier 2 candidates, invalid cache
  ratios, `Hydrate(nil)`, empty resolver, externally deleted rows, dispatch failure, persist
  failure then retry (no event before the durable write), and a resolve racing a persist.
- Mutation check: removing the `resolved_at IS NULL` CAS guard makes
  `TestLifecycle_ResolveRacingPersistIsNotOverwritten` fail.
- Test doubles: `recordingDispatcher` and `mockActionStore` stand in for delivery and
  `action_log`. All incident persistence tests use the real fixture PostgreSQL. Nothing was
  exercised against real Slack or PagerDuty (out of scope).

### Manual Checks Remaining
- CHECK-M1: MANUAL. Run the sidecar in fleet mode against 2 databases on one cluster, and
  confirm that one database's log errors do not raise incidents on the other database.
- CHECK-M2: MANUAL. Check that a PagerDuty channel receives detected and resolved events
  (after the pagerduty follow-up above).
