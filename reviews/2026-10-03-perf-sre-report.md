# Performance fix: SRE, runways, earned autonomy, probes, history reads

Branch `claude/perf-sre`, from `claude/perf-base`. Not pushed. Scope from the fix brief
(`reviews/2026-10-03-perf/FIX-BRIEF.md`): gate offenders 3-9 and the `measured.md` items for
the wraparound probe, the schema guard's structural catalog scan and the SLO aggregate.
Collector catalog reads, API/events and storage/retention/partitioning were left to their
branches.

## Result on the performance gate (small scale, PG17)

Same fixture and command before and after (`go test -tags=perfgate -run '^TestPerfGate$'
./cmd/pg_sage_sidecar`, `PG_SAGE_PERF_SCALE=small`: 500 tables, 1,500 indexes, 500
sequences, 20,000 rows per growing sage table).

| | Before (`claude/perf-base`) | After |
|---|---|---|
| Offenders | 15 | 8, all owned by other branches (action_log, findings, health_history, verification: collector/API/retention reads) |
| sage.incidents seq scans at startup | 3 (60,000 rows) | 0 |
| sage.decision seq scans | 2 steady + 1 warmup (60,000 rows) | 0 |
| sage.runway_samples seq scan at startup | 1 (20,000 rows) | 0 |
| sage.sre_autonomy_events seq scan at startup | 1 (20,000 rows) | 0 |
| sage.sre_investigations seq scan | 1 per window (5,001 rows) | 0 |
| Analyzer history statement (gate B, 100 ms) | 224 ms mean, 5 calls, 1,118 ms steady | snapshot decode 25 ms mean on a cold start (2 calls), then only new snapshots (below the steady top 15) |
| Forecaster daily-aggregate statements, steady | 23.6 ms + 7.9 ms mean, 189 ms per 6 cycles | below the steady top 15 (< 22 ms total) |
| runway_trends, steady | v1 14.4 ms mean | v2 5.7 ms mean |
| wraparound_tables, steady | v1 3.8 ms mean | v2 below the steady top 15 |
| Plan-regression rule | failed every pass ("cannot scan NULL into *float64") | 7.7 ms mean, 2 plans per query |
| Unexplainable statements | 0 | 0 (the first after-run found 5, all mine; fixed, see Bugs found) |

The gate still reports FAIL because of the other branches' offenders. The final run's
generic-plan suspects list the earned-autonomy reconcile on `sage.decision`. That is a
normalization artifact: the gate explains pg_stat_statements' text, where the literals
`'execute'` and `'incident_family'` are parameters, so the partial index predicate cannot
match. The statement pg_sage runs has them as literals, so its custom and generic plans
can both use the partial index. `TestReconcile_ReadsBoundedByAutonomousActions` explains
the statement as executed.

Two statements are slower on the synthetic gate fixture, by design:

- **Runway restart load:** 44.7 ms (DISTINCT ON over the 48 h window, a seq scan of
  `runway_samples`) became 58.8 ms (a recursive skip scan, no seq scan). The gate seeds
  3,000 series of about 7 samples each, so a skip scan does ~3,000 index probes. With
  real shapes (100 series x 2,880 samples) the same change is 28 ms -> 1.3 ms. It runs once
  per start.
- **sequence_runway** at 500 sequences: v2 11-13 ms, v3 11-17 ms in isolation (the
  coverage CTE and the slice hash). The gate's warmup shows v3 at 61.9 ms mean because
  the first call took 108.9 ms (cold catalog caches on a new connection). v3's advantage
  is at scale (below).

## What was built

| Item | Change | Files |
|---|---|---|
| 1. Earned-autonomy reconcile (offender 3) | Partial indexes on `decision(id) WHERE verdict = 'execute' AND evidence ? 'incident_family'` and `action_queue(action_log_id) WHERE identity_key LIKE 'autonomy:%'`: the reconcile's two reads now probe the autonomous executions and handoffs only. | `schema/sre_perf_migration.go` |
| 2a. Analyzer history (offender 4) | `buildHistoricalAverages` used to number every snapshot in the lookback (row_number + count over all) and decode an evenly spaced sample. It now takes the first non-empty snapshot of each of at most 100 fixed time buckets (width = lookback / 100, aligned to the epoch, so the picks do not move between cycles), decodes each once (batches of 16) and keeps the decoded means across cycles; a steady cycle decodes only new buckets. | `analyzer/query_history.go`, `analyzer.go` |
| 2b. Plan regression (static A_snapshots) | Two LATERAL reads per query (newest plan; the one before it) from the distinct query ids of the last 7 days, instead of numbering and detoasting every plan of the week. NULL `query_text`, `execution_time` and costs no longer end the rule. | `analyzer/plan_pairs.go`, `rules_plan_diff.go` |
| 2c. Forecaster (offender 4) | Daily query and sequence aggregates use the first and last non-empty snapshot of each day (found by index, one LATERAL pair per day), decoded once per Forecaster; only snapshots it has not seen are decoded. The SQL `lag` semantics of the old aggregate are reproduced in Go. | `forecaster/day_history.go`, `day_aggs.go`, `datasource.go`, `forecaster.go` |
| 3a. sequence_runway (offender 5) | v3: at most 2,000 sequences per statement (was 20,000, capped by a quarter of the lock table) and an optional slice `hashint8(seqrelid) % K = k`. When a pass is capped, the runway monitor re-reads the catalog in slices sized to three quarters of what one statement read, merges and keeps the 50 nearest their limit. A coverage-only row reports the catalog when no sequence is used. | `sre/probes/catalog_sequences.go`, `types.go`, `registry.go`, `decode_*.go`; `runway/sequence_cadence.go` |
| 3b. runway_trends (offender 6) | v2: recursive skip scan over (kind, subject) and per-series LATERAL reads of the window (latest sample, count/min/regression over the current epoch) by index, instead of a window over every sample. | `sre/probes/catalog_runway.go` |
| 3c. Runway restart load (offender 7) | The newest sample per series via a recursive skip scan bounded to the 48 h window. | `runway/store.go` |
| 4. Incidents and investigations (offenders 8, 9) | Startup: the incident CHECK widening drops and re-adds a constraint only if it exists and lacks a value (it rebuilt and validated three constraints on every start: the 3 seq scans); the `last_detected_at` backfill runs only when the column is added; the per-database autonomy backfill is driven by the legacy rows (LATERAL first carried-over event) instead of joining every event. Investigations list: one statement per variant (scope; scope + case; with or without cursor) on new `(deployment, database, created_at DESC, id DESC)` and `(..., source_case_id, ...)` indexes. Autovacuum-cancellation probe: partial index on `incidents(detected_at) WHERE 'log_autovacuum_cancel' = ANY(signal_ids)`. Verification watch lookup: expression index on `(baseline->>'watch_id')`. | `schema/incident_migration.go`, `bootstrap.go`, `sre_m7_database_scope_migration.go`, `sre_perf_migration.go`; `sre/postgres_list.go` |
| 5a. Wraparound probe (measured.md) | v2 ranks `pg_class` by age and keeps the top N before calling the statistics functions for those rows only. | `sre/probes/catalog_runway.go` |
| 5b. Schema guard structural scan (measured.md, ~17x/h) | The scan reruns only when the change counters of `pg_class`, `pg_attribute`, `pg_namespace` and `pg_type` (`pg_stat_sys_tables`) moved, at most every 5 minutes, and at least hourly. Callers get a copy of the cached answer. | `autonomy/schema_structural_cache.go`, `schema_detect_postgres.go`, `schema_postgres.go` |
| 5c. SLO aggregate (measured.md M12, static F10) | Each sample stores its series' running reset-compensated totals (`cum_bad`, `cum_eligible`, `cum_samples`, `cum_resets`, `chain_start`). Writes to a series take a transaction advisory lock; a late sample re-chains the samples after it. A window is newest minus baseline: three primary-key probes per series, with bounds written as row comparisons so the generic plan cannot fall back to the time index of every SLO. Samples written before the upgrade (NULL counters) use the old raw aggregation per series until they age out. | `sre/slo/store_cumulative.go`, `store_window.go`, `store.go`; columns in `schema/sre_perf_migration.go` |

**Migration.** One new idempotent migration (`ddlSREPerf`, registered after `ddlPerfIndexes`)
reuses the decision-ledger loop: each index is checked in `pg_indexes` first, built
`CONCURRENTLY` and rebuilt if INVALID; no bare `CREATE INDEX IF NOT EXISTS` on
`sage.decision`. I grepped `internal/schema` for each definition; none duplicates an
existing index. The SLI columns are added in a DO block guarded by the catalog.

## Measurements outside the gate (PG17, synthetic)

5,000 sequences, 150,000 runway samples in 3,000 series (`perfsre_scratch`, dropped):

| Statement | Before | After |
|---|---|---|
| runway_trends | 98-169 ms | 7.6-24 ms |
| runway_trends, 100 series x 2,880 samples | 86-106 ms | 13-16 ms |
| Runway restart load, 100 x 2,880 | 28 ms | 1.3 ms |
| Runway restart load, 3,000 synthetic series | 48 ms (seq scan) | 66 ms (no seq scan) |
| wraparound_tables | 21-37 ms | 7-15 ms |
| sequence_runway, whole catalog | v2 30-57 ms, 3,376 locks | v3 32-52 ms, 2,016 locks |
| sequence_runway, one of 4 slices | n/a | 11-28 ms, ~1,230-1,320 locks |

Snapshot fixture (`perfsre_fixture`, dropped): the old analyzer history statement took
232-310 ms; `sage.snapshot_data` costs about 1.35 ms per 60-element delta, so cost is in
the number of snapshots decoded, which is now at most 100 on a cold start and only new
buckets after that.

## Decisions

1. **Analyzer sampling changed from "evenly spaced by row number" to "first snapshot per
   time bucket".** Row-number sampling moves every pick when one snapshot is added, so
   nothing can be cached. Time buckets are stable; the averages are over the same number
   of points (100) and the small-history case still uses every snapshot.
2. **Sequence cap 2,000 per statement, slices instead of one big statement.** A 20,000-
   sequence statement held 20,000 locks and could break other sessions ("out of shared
   memory" on PG14/PG18 with defaults). Slicing keeps the whole-catalog ranking; the
   monitor only slices when a pass was capped, so small catalogs still use one
   statement.
3. **Structural scan: 5-minute floor, 1-hour ceiling.** The counters are statistics; a
   reset or late flush could hide DDL, so the hourly rescan is the backstop. Limitation:
   temporary tables also move these counters, so a workload that creates temp tables
   all the time gets a scan every 5 minutes (12/h instead of ~17/h), not one per DDL.
4. **SLO running counters instead of incremental aggregates in a side table.** They live
   on the samples, so retention, late samples and resets need no second structure; exact
   equality with the raw aggregation is tested on seeded random series with resets, a
   gap and out-of-order (late) inserts, and under concurrent writers.
5. **Runway restart load uses the skip scan even though the synthetic gate shape is
   slower** (58.8 vs 44.7 ms, once per start): it removes the seq scan and is 20x faster
   on real series lengths.
6. **No keyframe index for snapshots.** Bucket picks use the existing
   `(category, collected_at)` index; adding one would duplicate storage-branch work.
7. **Probe versions bumped** (sequence_runway v3, wraparound_tables v2, runway_trends v2)
   because their SQL changed; the catalog test's version table was updated.

## Bugs found

1. **Plan-regression rule failed every pass** when a captured plan had no
   `execution_time` or `query_text` (`cannot scan NULL into *float64`), in every gate run.
   Fixed in `plan_pairs.go` (COALESCE), pinned by
   `TestPlanRegression_NullTimingAndTextDoNotFailThePass`.
2. **Incident CHECK migration rebuilt three constraints on every start** (drop, re-add,
   validate = 3 full scans of `sage.incidents`). Fixed; re-run test asserts no OID change
   and 0 seq scans.
3. **My own SLO statements were unexplainable to the gate**: a constant-only `CASE THEN 1
   ELSE 0` becomes `text` once pg_stat_statements normalizes constants (`bigint + text`,
   `sum(text)`). Found in the after-gate run; fixed with `(...)::int`, pinned by
   `TestSLOStatements_PlanWithNormalizedConstants`.

## Tests (phase 1 before the fix)

Phase 1 (`da7787c4`) added the failing specifications: EXPLAIN-based (index ranges, no
seq scan, rows read per node, generic plans), scan-counter deltas
(`pg_stat_xact_user_tables`), decode counts via a query recorder, lock counts for the
sequence slices, and cadence tests for the structural scan. Helpers in
`internal/testdb/explain.go`. `f43ab8a7` and `c759502a` fix errors in the tests themselves
(listed in those commit messages); no assertion was relaxed.

## Test Results

**Command:** `go test -p 2 -cover -count=1 ./...` (repo root mounted in `golang:1.25`,
`--cpus=2`, PG17 at :55474), then `go test -tags=e2e -count=1 -timeout 900s ./e2e/`; the
touched packages again with `-race` (PG17) and on PG14 (:55414) and PG18 (:55418).
**Total (full suite, PG17):** 11,343 passed, 4 failed, 21 skipped. All 4 failures were
re-run and pass (below). **e2e:** 78 passed, 0 failed, 13 skipped.
**PG14 / PG18 (touched packages):** all packages ok. **-race (PG17):** all ok after one
timing re-run (below), no data races.

**Coverage (touched packages):**

| Package | PG17 | PG14 | PG18 |
|---|---|---|---|
| internal/analyzer | 88.2% | 88.2% | 88.2% |
| internal/autonomy | 83.8% | 83.8% | 83.8% |
| internal/earned | 88.6% | 88.6% | 88.6% |
| internal/forecaster | 88.9% | 88.9% | 88.9% |
| internal/runway | 91.1% | 91.1% | 91.1% |
| internal/schema | 83.7% | 83.7% | 83.7% |
| internal/sre | 87.5% | 87.5% | 87.6% |
| internal/sre/probes | 93.2% (re-run) | 93.1% | 93.2% |
| internal/sre/slo | 89.2% | 88.7% | 89.1% |
| internal/verify | 86.8% | 87.4% | 86.8% |
| internal/testdb (utility) | 72.3% (was 43.2%; tests added in `6ac62093`) | 72.3% | 72.3% |

All packages meet coverage thresholds (70% business logic, 50% utilities).

### Skipped Tests (must be zero or justified)
- Full suite (21): live cloud/provider tests without credentials (AWS RDS, Cloud SQL,
  Lakebase, Azure x2, agentdb gauntlet x3), live LLM (x3), a Windows-only path test,
  container failover/restart fixtures (x3), PgBouncer without a pooler (x3), the RCA
  child-process fixture, the plan-fixture generator and the incident bench. All are
  environment-gated; none is in a touched package.
- e2e (13): `SAGE_LLM_API_KEY` not set (live LLM tests).

### Failures (if any)
- internal/sre/probes: `TestCatalog_HasEveryR1FamilyWithinCeilings`. The test's version
  table still expected the old probe versions. The SQL changed, so the versions were
  bumped on purpose; table updated (`c759502a`), passes.
- internal/config: `TestConfig_StandaloneNormalization*` (2). My run also exported
  `SAGE_DATABASE_URL`, which those tests read as configuration. Re-run without it: ok.
  Not a code failure.
- internal/executor: `TestApplyLockCeilingCapsCycleAnalyze`. Timing: the control finished
  in 1.4 s, want about 4 s, with other agents loading the host. Re-run: ok. Untouched
  package.
- -race: internal/sre/probes `TestRunner_SidecarWideLimit`. A 150 ms sleep probe hit its
  statement timeout under the race detector. Re-run with `-race`: ok. Runner code is
  untouched.

### Coverage Gaps (packages below threshold)
- None.

### Bugs Found This Session
1. [BUG] analyzer/rules_plan_diff.go: the plan-regression rule failed on every pass when a
   plan had NULL `execution_time` or `query_text`. Fixed in `plan_pairs.go`.
2. [BUG] schema/incident_migration.go: the CHECK widening rebuilt and validated three
   constraints on every start (3 full scans of `sage.incidents`). Fixed.
3. [BUG] sre/slo (this branch): a constant-only CASE made the running-counter writes
   unexplainable to the gate. Fixed and pinned.

### Mutation testing
20 mutations, each run against the tests meant to catch it. All 20 failed their tests
(killed); files were restored and checked afterwards.

| # | Mutation | Killed by |
|---|---|---|
| m01-m03, m07, m10 | Remove each new index from the migration | the reconcile (x2), watch lookup, investigations list and autovacuum-cancel tests |
| m04 | Incident CHECK rebuilt on every run | `TestIncidentConstraintMigration_*` (2) |
| m05 | `last_detected_at` backfill unconditional | `TestIncidentsLastDetectedMigration_NoScanOnceTheColumnExists` |
| m06 | Autonomy backfill joins every event again | `TestAutonomyScopeMigration_LegacyLevelReadsItsPairOnly` |
| m08, m09 | wraparound / runway_trends back to v1 SQL | `TestWraparoundTables_RanksBeforeReadingStatistics`, `TestRunwayTrends_SameTrendsBoundedRead` |
| m11 | Sequence cap back to 20,000 | `TestCatalog_SequenceRunwayV3IsBounded`, `TestSequenceRunway_SlicesCoverTheCatalogOnce` |
| m12 | Monitor never slices | `TestReadSequences_EveryPassReadsTheWholeCatalogInSlices`, `TestMonitorRead_ReportsCoverageOncePerChange` |
| m13 | Restart load back to DISTINCT ON | `TestLoadLast_OneRowPerSeries` |
| m14 | Analyzer forgets decoded picks | `TestHistoricalAverages_*` (2) |
| m15 | Forecaster forgets decoded samples | `TestForecaster_DecodesEachDailySampleOnce` |
| m16 | Plan pairs without COALESCE | `TestPlanRegression_NullTimingAndTextDoNotFailThePass` |
| m17 | Structural cache never fresh | `TestStructuralScan_*` (2) |
| m18 | SLO always on the raw path | `TestAggregate_ReadsAFewRowsPerSeries` |
| m19 | No re-chain after a late sample | `TestAggregate_RunningCountersMatchTheRawReference`, `TestRecordSample_ConcurrentWritersKeepCountersExact` |
| m20 | Plan pairs back to numbering every plan | `TestPlanRegression_ReadsTwoPlansPerQuery` |

### Post-test audit
- **Inputs not tested:** sequence catalogs above `MaxSequenceSlices` x 2,000 (20M
  sequences; the slice count is clamped and coverage reports the gap); the structural
  cache after a real `pg_stat_reset` (the hourly floor covers it, but no test resets the
  statistics).
- **Assertions that could pass on broken code:** none found. Every plan test also asserts
  the result: finding found, trends equal to the v1 reference, aggregates equal to the raw
  reference, list pages ordered and complete.
- **Test doubles:** the runway cadence tests and the `testdb` helper tests use fakes. The
  SQL each fake stands for is also tested live (`sequence_slices_db_test.go`,
  `store_skipscan_db_test.go`, and every package that calls `testdb.Explain`).

**Lint:** `golangci-lint run` on every touched package: 0 issues. gofmt is clean on every
changed file. Limits: no changed function is over 50 lines, no file over 500 and no line
over 100, except `schema/bootstrap.go` (already 1,007 lines with 3 long lines on
`claude/perf-base`; this branch changes one block there).

### Manual Checks Remaining
- None.

## Left for later

- The sequence probe's whole-catalog ranking still reads every sequence's catalog row
  each pass (cheap, but O(sequences)); a cached ranking would remove it.
- The investigation-memory candidate read (`sre/postgres_memory.go`, `trigger_kind = $5 OR
  summary->>'family' = $6`, ordered by `concluded_at`) ranges over the scope and sorts.
  It was not among the offenders and was not changed.
- On workloads that create temporary tables all the time, the structural scan reruns
  every 5 minutes (see Decisions 3).
