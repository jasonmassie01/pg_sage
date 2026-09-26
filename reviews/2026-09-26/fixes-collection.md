# Fixes — collection area (branch `fix/2026-09-26-collection`)

Base: `9a3cac7`. Owned packages: collector, querystore, logwatch, providerobs,
autoexplain, ha, selfmonitor, retention, and additive schema DDL (FK ON DELETE).
Phase 1 test commit: `07662cf` (every regression test verified to fail on the
unmodified code, using throwaway stubs for new symbols; see commit message).

## Status

| ID | status | commit | test name | notes |
|---|---|---|---|---|
| G1-B02 | FIXED | 2b793f0 | TestParseJSONLogLine_RealPostgresKeys | Real jsonlog keys (`user`, `dbname`, `statement`) and csvlog-style timestamp per `jsonlog.c`. Existing fixtures used a fictional format and were corrected. |
| G1-B03 | FIXED | 807a11c | TestSplitLines_CSVQuotedNewlinesAcrossReads, TestSplitLines_CSVResyncAfterMidRecordStart, TestFileWatcher_CsvlogMultiLineDeadlock | csvlog newline ends a record only outside quotes. Partial records carry across reads, and each record is parsed by `encoding/csv`. Resyncs on a timestamp-prefixed line after a mid-record start. |
| G1-B05 / R10 | FIXED (R10 partial) | 3fb3838, 2a36e55 | TestCollectQueries_AggregatesAcrossUsers, TestRecord_AggregatesDuplicateQueryIDs | SQL aggregates per queryid (sums; recombined mean and pooled stddev), and `querystore.Record` also dedups. R10: resets are now detected between consecutive samples. No stats-reset epoch column yet (DEFERRED: needs a schema column plus reader changes in executor/verify). |
| G1-B08 / C01 | FIXED | 3fb3838 | TestCacheHitRatioExpression_FractionAndUnknown, TestCollectSystem_CacheHitRatioIsFraction, TestSystemStats_CacheHitRatioJSONContract | Contract: `SystemStats.CacheHitRatio` is a fraction in [0,1]. `collector.CacheHitRatioUnknown` (-1) means no data (same as the analyzer's existing `< 0` guard), and it persists as JSON `null`. `CacheHitRatioKnown()` helper. The rca signal and forecaster consumers belong to the analysis agent. |
| G1-B09 | FIXED | b0e1673 | TestCaptureOnDemand_ParameterizedUsesGenericPlan, TestCaptureOnDemand_ParameterizedPrePG16NotStored | PG16+: `EXPLAIN (GENERIC_PLAN)` stored as `source='generic_plan'`. Before PG16, parameterized statements are skipped, not stored with a low-fidelity label, because `optimizer.fromExplainCache` does not filter by source. The PREPARE/NULL path is removed, which also removes G1-B29. |
| G1-B10 | FIXED | 04b328f | TestPollLogs_OutageKeepsWindowBoundedAndRecovers, TestPollLogs_SaturationDoesNotWedgeCursor | 10-minute request cap. Lag over 6 hours skips ahead and reports the gap. On saturation the window halves down to 2 minutes, then the cursor advances and reports the drop. |
| G1-B12 + G7-B11 | FIXED | c0e96d1, 3c9d3eb | TestBootstrap_RetentionForeignKeysSetNull, TestRun_PurgesFindingReferencedByAlertLog, TestRun_PurgesActionLogWithDependents, TestRun_KeepsActionReferencedByValueLedger | Idempotent migration sets 10 nullable FKs to ON DELETE SET NULL. SET NULL, not CASCADE, because the children are audit or current-state rows. NOT NULL children (incident_avoided, verification, change_lease) are handled with keep-predicates, and children are purged first. |
| G7-B12 + G4-B26 | FIXED | 3c9d3eb | TestRun_PurgesPreviouslyUnboundedTables, TestRetentionRules_CoverEveryTimeSeriesTable | Adds notification_log, alert_log, decision, change_lease, verification, retention_run, health_history, size_history, incidents (resolved), briefings, explain_results. Batches stay at 1000 rows. An exemptions map with a guard test forces a retention decision for every future sage time-series table. |
| G1-B14 | FIXED | c629ea6 | TestClassifier_DefaultExcludesSidecarApplication | Empty exclude list now means `[pg_sage]`. An explicit list replaces the default. |
| G1-B15 | FIXED | b0e1673 | TestCaptureOnDemand_DoesNotLeakAutoExplainSettings | `ConfigureSession` (session SET) is replaced by `ConfigureTransaction` (SET LOCAL, with a savepoint per SET). The connection is released before the insert, which also fixes a 1-connection pool deadlock. |
| G1-B16 | FIXED | 3fb3838 | TestCollectQueries_BlockReadTimeFromStatements | Probes the installed pg_stat_statements columns: `shared_+local_blk_*_time` (pgss 1.11 / PG17) or `blk_*_time`. The result is cached. |
| G1-B17 | FIXED | 3fb3838 | TestCollectLocksAndActivity_ScopedToCurrentDatabase | Locks are limited to the current DB, and pg_class is joined only for same-DB relation locks. idle_in_transaction counts local sessions only. Active/total backends stay cluster-wide because they are compared with the cluster-wide max_connections. |
| G1-B18 | FIXED | 3fb3838 | TestCollectTables_ErrorMidPaginationDoesNotSkipTables | The keyset cursor is now local to the call. The struct fields are removed. |
| G1-B20 | FIXED | 3fb3838 | TestScanReplicaRows_NullLSNs | LSNs are `*string`. No consumer outside collector reads them. |
| G1-B21 | FIXED | 129ea61 | TestMonitor_UnknownRoleFailsClosed, TestMonitor_SafeModeReachableAtRealSamplingRate, TestMonitor_SafeModeExitRequiresStableCooldown, TestMonitor_IsolatedFailoversDoNotEnterSafeMode | `Check` returns true (not a writable primary) on probe error. New `Role()` and `MutationsAllowed()`. Safe mode triggers at 2 flips per hour and exits after 3 stable checks plus a 30-minute cooldown. Executor code is not changed (see cross-area). |
| G1-B24 | FIXED | 2b793f0, 8926817 | TestParsePostgresTimestamp_ZoneAbbreviations, TestParsePostgresTimestamp_UnknownAbbreviationRejected, TestFileWatcher_ReportsUnparseableLines | Explicit table of unambiguous abbreviations plus numeric offsets. Unknown or ambiguous abbreviations (CST, IST) are rejected, and Drain logs parse failures at WARN once per cycle. |
| G1-B26 | FIXED (querystore side) | 2a36e55 | TestWindowedLatencyEvidence_Statuses, TestWindowedLatencyEvidence_NoNewCalls | `querystore.WindowedLatencyEvidence` returns measured, not_sampled, insufficient_samples, no_new_calls or counters_reset. `WindowedLatencyMsBetween` is kept as a wrapper for the executor. |
| G1-B27 | DEFERRED | — | — | This is a product decision, not a code defect. `DeriveTelemetry` deliberately leaves IO% unknown ("byte/time counters cannot establish capacity utilization"), and an existing test asserts that. The result fails closed: provider load is unavailable and autonomous index admission is withheld. Deriving IO% (for example from `node_disk_io_time_seconds_total`) needs a decision on whether busy-time counts as capacity evidence. |
| G1-B30 | FIXED | b0e1673 | TestRun_NonPositiveIntervalDoesNotPanic | Logs WARN and uses `config.DefaultAutoExplainCollectInterval`. |
| G1-B32 | FIXED | 129ea61, 3c9d3eb | TestPurgeTable_FailureLoggedAtErrorLevel, TestMonitor_UnknownRoleFailsClosed | retention and ha log at ERROR/WARN/INFO instead of passing a component name as the level. |
| C18 / G1-B35 | FIXED | 3fb3838 | TestCollectSequences_DirectionAndRange | Collects `min_value` and `cycle`. `pct_used` is measured over [min,max] in the direction of travel, using numeric math. `sage.*` sequences are excluded. |
| Dead code | DONE | 792fc65 | — | Removed querystore.Prune (retention covers query_store), ha.Monitor.IsReplica, autoexplain.ConfigureSessionBatch and retention.cleanStaleFirstSeen. `explain.New` was skipped (api-web area). `querystore.WindowedLatencyMs` is left in place for the analysis agent. |

## Cross-area notes (no edits made outside owned packages)

- **Retention wiring (C08/G1-B04, runtime agent):** per database, call
  `go retention.New(dbPool, cfg, logFn).RunEvery(ctx, time.Hour)` in the fleet,
  meta-db and dynamic worker lifecycles. `RunEvery` runs at once, then on every
  tick, and returns when ctx is cancelled. A non-positive interval means 1 hour.
- **Executor (G1-B21):** gate mutations on `haMon.MutationsAllowed()`. Today's
  `RunCycle(ctx, haMon.Check(ctx))` already fails closed, because Check returns
  true for an unknown role.
- **Executor/verify (G1-B26):** switch `perQueryRegression` to
  `querystore.WindowedLatencyEvidence`. Treat every status except `measured` as
  unknown (fail closed or extend the watch), never as "no regression".
- **Analysis (G1-B08/C01):** use the cache-ratio contract described in the table
  above. Historical `sage.snapshots` rows still hold percent values, so the
  analysis agent's defensive normalization is still needed.
- **Analyzer (C18):** `SequenceStats.Cycle` is now available. A CYCLE sequence
  near its bound wraps instead of failing, so it may deserve lower severity.
- **Config (G1-B14):** config.go could also seed `ExcludeApplications: [pg_sage]`.
  logwatch applies the default by itself, so this is optional.
- **Schema DDL literals** still say `REFERENCES ...` with no action. The Bootstrap
  migration converts them on fresh and existing installs alike, so the DDL text
  was left unchanged to keep agent-native migration tests stable.
- `internal/schema/bootstrap.go` (969 lines) and the pre-existing long functions
  `collector.collect` (71 lines) and `Tailer.splitLines` (62 lines) exceed the
  hard limits from before this work. Each got only a minimal edit.

## Test Results

**Command:** `go test -count=1 -cover -v ./internal/{collector,querystore,logwatch,providerobs,autoexplain,ha,selfmonitor,retention,schema}/`
(and again with `-tags=integration`), `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable` (PG 17.10).
Also ran `go build ./...` and `go vet ./...` (clean), plus a smoke run of the dependent
packages analyzer, rca, executor, forecaster, optimizer, verify, api, advisor and cmd/...
(all ok).

**Total:** 575 passed, 0 failed, 1 skipped (top-level tests; subtests not counted)

**Coverage:**
- collector 85.0%
- querystore 88.1%
- logwatch 84.7%
- providerobs 87.8%
- autoexplain 84.6%
- ha 100.0%
- selfmonitor 75.0%
- retention 96.7% (100.0% in one run)
- schema 82.2%

### Skipped Tests (must be zero or justified)
- logwatch: TestResolveLogDir_AbsoluteUnix — SKIPPED: this test only applies on Unix, and the run was on Windows (pre-existing).

### Failures
- None.

### Coverage Gaps
- All packages meet coverage thresholds (lowest: selfmonitor 75.0%).

### Bugs Found This Session (beyond the assigned list)
1. [BUG] autoexplain/collector.go: `captureOnDemand` held its pooled connection while `storePlan` used the pool, so a 1-connection pool deadlocked. Fixed in b0e1673.
2. [BUG] logwatch/parser.go: the csvlog numeric-offset layouts had no space before the zone, but PostgreSQL always writes `"%S.mmm %Z"`. Both forms are now accepted (2b793f0).
3. [NOTE] The race detector is unavailable here (`-race` requires cgo). HA concurrency was exercised without `-race`.

### Manual Checks Remaining
- None. Supabase endpoint behaviour (G1-B28) was not in scope and was not run live.
