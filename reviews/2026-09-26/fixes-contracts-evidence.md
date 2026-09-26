# Evidence and identity contracts: auditor tests (2026-09-26)

Branch `fix/2026-09-26-contracts-evidence`, worktree `C:/Users/jmass/pgsr-fix-contracts-evidence`,
base `d74cc69` (merged review fixes). Scope: the Codex auditor's RCA persistence/process, analyzer
evidence, collector/querystore epoch and cohort tests, plus the HA and `audit_regression` guards.

Process: the auditor tests were copied in and committed first (`c5f8a12`), with setup-only
adaptations. Three failed on product code for the reason they target. Those were fixed in separate
commits. The `TestPreflightContract*` tests were written after the fixes, as extra unit and DB
coverage for the new behavior. They are not Phase-1 tests.

## Results per auditor test

| Test | Classification | Change | Commit |
|---|---|---|---|
| rca `TestPreflightRCAInProcessDedupAndClearControl` | PASS (control) | Shared helper now hydrates, as the production adapter does | c5f8a12 |
| rca `TestPreflightRCARestartResumesExistingIdentity` | FIXTURE-INCOMPATIBLE | The test drove `Engine.Analyze` without `Hydrate`. Production never does: `rcaAdapter.Analyze` (standalone, fleet and meta-db, `cmd/pg_sage_sidecar/rca_adapter.go`) calls `Engine.Hydrate` before every cycle. The helper now calls it too. Assertion 1 active / 1 total is kept, and passes. | c5f8a12 |
| rca `TestPreflightRCARestartClearsPersistedIncident` | FIXTURE-INCOMPATIBLE | Same hydration adaptation. Assertion 0 active is kept, and passes. | c5f8a12 |
| rca `TestPreflightRCAManualResolutionSurvivesStaleConcurrentFlush` | FIXTURE-INCOMPATIBLE | The lock-wait probe searched only for `INSERT INTO sage.incidents`. A persisted incident is now written with the CAS `UPDATE … WHERE resolved_at IS NULL` (merged R04 fix), so the probe also matches that. The held-open resolve sets `resolved_by`, as `rca.ResolveIncident` does. The engine write really blocks on the row lock. After commit it matches 0 rows and adopts the resolution. The "resolution survives" assertion is kept, and passes. | c5f8a12 |
| rca `TestPreflightRCAProcessRestartContinuesIdentity` | FIXTURE-INCOMPATIBLE | Two real OS processes, each hydrating as production does, give 1 active / 1 total. | c5f8a12 |
| rca `TestPreflightRCAProcessRestartClearsIncident` | FIXTURE-INCOMPATIBLE | A fresh process with 8 healthy cycles leaves 0 active. | c5f8a12 |
| rca `TestRCAChildProcessFixture` (helper) | FIXTURE | A whole-package run selected the helper, and it failed. It now skips without its parent. The parents require the child's `--- PASS` line. The child's `coverage:` echo made `go test -cover -v` report rca at 30.4% instead of 96.4%, so the echo now renames it. | 97969a6, 26bd3d9 (f28f30e reverted in 21c7291: wrong diagnosis) |
| analyzer `TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding` | REAL DEFECT | After a failed collection, the analyzer re-ran on the same snapshot and bumped `occurrence_count` 1→2 and `last_seen`. It now records the snapshot it consumed (pointer + `collected_at`). It skips the whole cycle (findings, RCA, LLM producers) with a WARN until a newer snapshot exists. | 70d95f5 |
| analyzer `TestPreflightEvidenceZeroExecPlanningPersistence` | REAL DEFECT | `calls=100, mean_exec=0` gave `ratio=+Inf`. `json.Marshal` failed, and the upsert batch aborted. The rule now skips a zero exec mean. The marshal error also names the finding. | 4163987 |
| analyzer `TestPreflightEvidencePercentCollectorToRatioRule` | PASS (guard) | none | — |
| analyzer `TestPreflightEvidenceFailedExecutionsDoNotCreatePlanningFinding` | PASS (control) | none | — |
| analyzer `TestAudit*` (3) | PASS (guard) | none | — |
| collector `TestPreflightEvidenceResetAfterRegrowth` | REAL DEFECT | A real `pg_stat_statements_reset()` followed by regrowth (10→30 calls) got `StatsReset=false`, and the window was certified. The collector now records the statistics epoch: `GREATEST(pg_stat_statements_info.stats_reset, pg_postmaster_start_time())`, PG14+, probed. Samples store `stats_epoch`, and windows that span epochs are `counters_reset`. The test now reports `StatsReset=true, ok=false`. | 986b2a7 |
| collector `TestPreflightEvidenceRoleIdentityWindow` | FIXTURE-INCOMPATIBLE | The merged G1-B05 fix already aggregates per queryid. The residual mismatch (0.067594 vs 0.067934) came from the previous test's samples of the same statement falling inside the window, plus a role-name collision from earlier runs. Setup now empties `sage.query_store`, uses per-process role names (dropped afterwards) and a fixture-DB-only reset. Measured and expected are now exact: 0.056489 = 0.056489. | c5f8a12 |
| collector `TestPreflightEvidenceTopLevelIdentity` | FIXTURE (GUC) | `levels=1`: nested statements are tracked only with `pg_stat_statements.track='all'`. The fixture pool sets it per session (superuser GUC), so no ENV-LIMITED case remains. Now `levels=2`, and the aggregate matches exactly. | c5f8a12 |
| collector `TestAudit*` (3) | PASS (guard) | none | — |
| querystore `TestPreflightEvidenceInteriorResetWindow` | FIXTURE (clock) | "stable" was flaky. `captured_at` uses the server clock, which runs about 42 ms ahead of this host (measured), and the window ended at client `time.Now()`, which excluded the last sample. The window bounds now come from the server clock. The interior-reset and no-calls cases already passed. | c5f8a12 |
| ha `TestPreflightEvidenceUnknownHAWithholdsPolicyAdmission` | PASS (guard) | none | — |

Also fixed (same defect, no auditor test): `verify.queryMeasurement` differenced only the window
endpoints. It now returns zero samples for any window with a counter decrease, an epoch change, or
an unknown epoch next to a known one. The evaluator already treats zero samples as insufficient
evidence (extend). Commit a5d36e9, test `TestPreflightContractVerifyRefusesCrossEpochWindow`.

Added tests (after the fixes): `TestPreflightContractClaimFreshSnapshot`,
`…StaleCycleLogsAndSkips`, `…ZeroExecPlanRatioSkipped` (analyzer); `…EpochChanged`,
`…MarkStatsResetOnEpochChange`, `…CollectStatementsEpoch`, `…StatementsEpochUnknownOnError`
(collector); `…EpochWindows`, `…RecordStoresEpoch`, `…LegacyWindowUsesEpochs` (querystore);
`…VerifyRefusesCrossEpochWindow` (verify).

## Design notes

- **Epoch**: the epoch is read once per cycle, before the counters. A reset between the two reads is
  still caught by the counter-decrease check, and by the next cycle's epoch change. An unreadable
  epoch is logged and stored as NULL. It is never guessed. A clean restart also changes the epoch
  (`pg_postmaster_start_time`), so windows across a restart are refused on purpose (fail closed).
- **Legacy rows**: rows written before this change have NULL `stats_epoch`. A window of only NULL
  rows keeps the old monotonic check. A window that mixes NULL and known epochs (it spans the
  upgrade) is refused.
- **Cohorts**: per-queryid aggregation (sum of `total_exec_time` over sum of `calls` across roles
  and top-level/nested entries) was already in the collector SQL and in `querystore.Record`.
  The auditor's expected arithmetic matches it exactly.

## Cross-area edits

- `internal/schema/query_store_epoch.go` (new) plus one line in `bootstrap.go`
  `migrationStatements`: `ALTER TABLE sage.query_store ADD COLUMN IF NOT EXISTS stats_epoch
  timestamptz`.
- `internal/verify/postgres.go`: epoch-aware measurement SQL.
- `internal/verify/testmain_test.go` (new, `testdb.Run`) and `postgres_integration_test.go`
  (the pool bootstraps the schema). The verify tests used to run against the shared server's
  `postgres` database, whose schema lacked the new column.

## DEFERRED / residual

- **Concurrent sidecars (RCA)**: 2 processes running *at the same time* can both insert the same
  open identity. There is no uniqueness on the open `identity_key`. A partial unique index first
  needs a reviewed migration of historical duplicates (DIAG-11).
- **Direct engine use (RCA)**: `rca.NewEngine` + `Analyze` without `Hydrate` still starts
  without state. All 3 production constructions go through `newRCAAdapter`.
- **Partial resets**: `pg_stat_statements_reset(userid|dbid|queryid)` or entry eviction followed
  by regrowth *between two samples* does not change `stats_reset`. It is caught only if a
  decrease is sampled. PG17's per-entry `stats_since` could close this; not done.
- **Mixed clocks**: `captured_at` uses the server clock, but executor rollback windows use the
  sidecar clock (about 42 ms skew here). This is negligible for 30-minute windows. Stamping samples
  with the snapshot time is a cross-package change.
- **Upsert batch**: one unpersistable finding still aborts the rest of the upsert batch.

## Operational notes

- Run these packages with `-p 1`. `TestPreflightEvidenceResetAfterRegrowth` needs a *full*
  `pg_stat_statements_reset()`, because only a full reset changes `stats_reset`. That reset is
  cluster-wide, so it can disturb tests in other packages or agents on the same server that
  depend on pg_stat_statements.
- **A live model request was made once by mistake.** `GEMINI_API_KEY` is set in this shell, so the
  first full rca run executed `TestTier2Live_RealGemini`: one real Gemini call, which failed its
  assertion. All later runs used `env -u GEMINI_API_KEY`, and the test skips.

## Test Results

**Command:** `go build ./... && go vet ./...`, then
`env -u GEMINI_API_KEY go test -p 1 -count=1 -cover -v <pkgs>` and the same with
`-tags=integration`, where `<pkgs>` = `./internal/rca ./internal/analyzer ./internal/collector
./internal/querystore ./internal/verify ./internal/executor ./internal/schema ./internal/ha`,
with `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`.
Also run: `./internal/api ./cmd/pg_sage_sidecar ./internal/retention` (1156 passed, 0 failed,
0 skipped).

**Total:** unit 1949 passed, 0 failed, 2 skipped. Integration tag: 1956 passed, 0 failed, 2 skipped.
(`--- PASS` lines, subtests included.) Auditor scope `-run '^(TestPreflight|TestAudit)'`: 33
top-level passes, 0 failures.

**Coverage (unit = integration):**
- rca 96.4%
- analyzer 85.0%
- collector 84.5%
- querystore 89.1%
- verify 85.8%
- executor 78.2%
- schema 81.5%
- ha 100.0%
- api 71.9%
- retention 96.7%
- cmd/pg_sage_sidecar 48.3%

### Skipped Tests (must be zero or justified)
- rca `TestRCAChildProcessFixture`: SKIPPED. It is a helper process that runs only when invoked by
  its 2 parents, which ran and passed (each requires the child's PASS line).
- rca `TestTier2Live_RealGemini`: SKIPPED. It is a live cloud model test, and the brief forbids
  live cloud tests (`GEMINI_API_KEY` unset).
- Output was grepped for SKIP, TODO and PENDING. Nothing else was found.

### Failures
- None.

### Coverage Gaps (packages below threshold)
- `cmd/pg_sage_sidecar`: 48.3%. This package was already below threshold, it was not touched here,
  and it is the runtime wiring package. Every package touched here meets the 70% threshold.

### Bugs Found This Session
1. [BUG] `analyzer/cycle.go`: a stale snapshot was re-analyzed after collection failures, which
   refreshed finding counts and `last_seen` (70d95f5).
2. [BUG] `analyzer/rules_query.go`: a zero exec mean produced a +Inf ratio, and the finding batch
   could not be persisted (4163987).
3. [BUG] `collector` / `querystore`: a reset followed by regrowth was certified as one window (R10)
   (986b2a7).
4. [BUG] `verify/postgres.go`: endpoint-only differencing ignored interior resets and epochs
   (a5d36e9).
5. [TEST INFRA] The verify tests ran on the shared default database without bootstrapping the
   schema. The RCA child-process output masked package coverage under `-v`.

### Manual Checks Remaining
- CHECK-M1: MANUAL. On PG13, or with pg_stat_statements below 1.9, confirm the epoch falls back to
  the postmaster start time. Only PG17 was available here.
- CHECK-M2: MANUAL. Before a release, inspect the historical `query_store` rows that have NULL
  epochs (DIAG-13). Windows that span the upgrade are refused until they age out.
