# Fixes — analysis area (2026-09-26)

Branch `fix/2026-09-26-analysis`, worktree `C:/Users/jmass/pgsr-fix-analysis`, base `9a3cac7`.
Owned: `internal/analyzer` (except `optimizer_mapping.go`), `internal/forecaster`,
`internal/schema/lint`, `internal/schemaguard`, `internal/cases`.

Process: scaffold refactor (`6c327a0`, no behavior change) → failing regression tests
(`fbda2d2`, every new test failed on the old code for the asserted behavior) → one commit
per fix. Tests changed in fix commits are listed in the commit messages with the reason
(each encoded the bug being fixed or an API rename).

## Status

| ID | Status | Commit | Test(s) | Notes |
|---|---|---|---|---|
| G2-B01 / C02 | FIXED | 4f034f5 | `TestRegression_SuppressionIsDurable`, `_ExpiredSuppressionReopensOnce`, `_SuppressedCriticalNotNotified` | Upsert skips identities with an active suppression (`suppressed_until` NULL or future); suppressed identities are removed from the in-memory list the executor iterates, so the executor cannot act on them. Expired suppression reopens exactly one row. |
| C04 | FIXED | 4f034f5 | `TestRegression_UpsertRefreshesRollbackSQL` | `rollback_sql` refreshed together with `recommended_sql`. |
| G2-B06 / G7-B07 | FIXED | 30feca9 | `TestRegression_CriticalNotifiedOnlyWhenNew` | Dispatch only opened or severity-escalated critical findings (from `UpsertFindingsWithResult`), 1 h per-identity cooldown; rewrite notifications only on open. |
| G2-B04 / C01 / G1-B08 | FIXED | f3c4494 | `TestRegression_CacheHitRatioUnits`, `TestRegression_CachePressureFractionUnits`, `_SystemAggsNormaliseCacheRatio` | Canonical unit = fraction. `analyzer.NormalizeCacheHitRatio` (>1 → /100) makes either merge order with the collector change correct and handles stored percent rows. Rule treats `<= 0` as no data. Forecaster SQL normalises per row and ignores the -1 sentinel. |
| G2-B02 / C03 | FIXED | a59d4cf | `TestRegression_LastFindingInCategoryResolves`, `TestCycleEval_Resolvable`, `TestRunSnapshotRules_MarksMissingInputFailed`, `TestLastEvaluatedCategories_*` | Per-cycle evaluated-category tracking: `RuleSpec` declares owned categories + required input; DB checks mark failure; forecaster reports evaluated categories. Failed/skipped/missing-input categories are never resolved. LLM producers keep emitted-only semantics (empty = skipped, not clear). |
| G2-B03 | FIXED | 83e5750 | `TestRegression_XIDWraparoundFindingPersists`, `_XIDWraparoundFindingUpserted` | Diagnostic SELECT moved to `detail.diagnostic_sql`; `RecommendedSQL` empty (not an action). |
| G2-B05 | FIXED | 6d0a106 | `TestRegression_PerTableGUCsDoNotConflictAcrossTables`, `_AutovacuumTuningHonorsReloptions` | Per-table GUC conflicts keyed by (guc, object); rule skips tables already at/below target. |
| G2-B13 | FIXED | bf98a8d | `TestRegression_ParseIndexDefIncludeAndWhere`, `_FullIndexNotSubsetOfPartial` | Paren/quote-aware scanner; identifiers unquoted. |
| G2-B22 / G4-B23 / C16 | FIXED | 338e7ac, de12ee8, 86349af | `TestRegression_IndexDDLQuotesIdentifiers`, `TestRegression_LintDDLQuotesIdentifiers`, `TestRegression_TableBloatFallbackQuotes` | Analyzer index DDL, all DDL-building lint rules and the cases VACUUM fallback quote identifiers. G4-B23 deferred to this area by the executor agent — done. |
| G2-B07 | FIXED | c16aea5 | `TestRegression_UnusedIndexFirstSeenClearedOnUse`, `_UnusedIndexRespectsStatsEpoch`, `_LoadStatsEpoch` | FirstSeen cleared on use; stats epoch = max(stats_reset, postmaster start) ported from the lint rule; fails closed on probe error. |
| G2-B08 | FIXED | e53a448 | `TestRegression_MissingFKIndexIncludeAndComposite`, `_FKSchemaNotGuessed` | FK rows regrouped per constraint, set-coverage; ambiguous table names (same name in >1 schema) emit nothing and protect every candidate schema. Collector follow-up needed for full fix (see below). |
| G2-B09 | FIXED | 8e0b3ef | `TestRegression_InvalidIndexBuildInProgress`, `_InvalidIndexNeedsTwoObservations`, `_LoadIndexBuilds` | Skip tables in `pg_stat_progress_create_index` (this DB); two-observation requirement; fail closed on probe error or unattributable build row. |
| G2-B10 | FIXED | ccef190 | `TestRegression_OverlappingIndexSkipsUniqueAndPK` | Excludes unique/PK/constraint-backed short indexes; key columns only; opclass/indoption/collation must match. |
| G1-B06 | FIXED | bafbdb2 | `TestRegression_HistoricalAveragesReadCollectorField` | JSON key `mean_exec_time`. |
| G2-B23 | FIXED | dd83728 | `TestRegression_SlowQueryZeroThreshold` | `<= 0` → default threshold. |
| C18 / G1-B35 | FIXED (consumer) | 8db136a | `TestRegression_DescendingSequenceExhaustion` | Descending sequences use [type min, max_value]. Collector must add `min_value`/`start_value`/`cycle` for custom ranges. |
| G2-B18 | FIXED | 12460a9 | `TestRegression_ConnectionSaturationUsesTotalBackends` | Uses max(total, active) backends. |
| C10 | FIXED | 93bf922 | `TestRegression_QueryAggsUseResetAwareDeltas` | lag()-based per-queryid deltas; reset → new count; first sample → 0. |
| G2-B14 | FIXED | 525519c | `TestRegression_FailingRuleDoesNotResolveFindings` | `Linter.ScanReport` returns failed rule IDs; runner skips them. |
| G2-B27 | FIXED | 38bb3da | `TestRegression_ExcludeSchemasAcceptsQuotedNames`, `TestSchemaExcludeSQL_QuotesUnusualNames` | Quoted literals; backslash/NUL/empty skipped. |
| G2-B24 / SURF-11 | FIXED (findings) | 86349af | `TestRegression_CaseTimesAnchoredToEvidence`, `_VacuumCandidateExpiryAnchored` | `observed_at` = finding `last_seen`; finding-derived candidate expiry = last_seen + TTL. Incident/query-hint candidates still use now() (DEFERRED: their sources carry no evidence time in this API). Ranking part of SURF-11 not in scope. |
| G2-B25 | FIXED | 5548536 | `TestRegression_ValueRowsAreSorted` | Cross-area (`internal/value`). |
| G2-B28 | FIXED | 9b2f8c9 | `TestRegression_WorkMemPromotionCountsDistinctQueries` | dbid filter + `count(DISTINCT queryid)` (no `toplevel` predicate: avoids breaking pg_stat_statements < 1.9). Test added with the fix (needed the live pg_stat_statements fixture); verified failing on the old SQL. |
| G2-B15 | FIXED (cross-area) | e8a0f51 | — (wiring; covered by `go vet`/build) | `main.go`: analyzer `Run` moved below `WithDispatcher/WithDatabaseName/WithPlanNarrator` (standalone + fleet). Required so first-cycle criticals are notified under the new "notify on open" rule. |
| G2-B26 | FIXED (cross-area) | d4e5d59 | — (wiring) | `main.go`: fleet lint runner and migration advisor get `dbPGVersion`. |
| G2-B17 | DEFERRED | — | — | Needs collector to emit PG17 `shared_blk_read_time`/`blk_read_time` per query (collector area). |
| Lock-chain identity (lead request) | DONE | 58ca519 | `TestLockChainDetail_BackendEvidence`, `TestDetectLockChains_QueryRuns` | Detail adds `pid`, `backend_start`, `query_start`, `query_id`, `query` (LEFT 200, no ellipsis), `app_name` — the executor's `backendSignalEvidence` shape. Note for executor: its terminate SQL requires `state='active'`, so idle-in-transaction terminates from lock chains will never match. |
| G2-D01 DedupFindings | DELETED | c0e2a37 | dedup tests call `DeduplicateFindings(in, 0, ...)` | |
| G2-D02 ResolveIfEvidenceMissing | DELETED | c0e2a37 | test removed | Also removed now-unused `cases.StateResolvedEphemeral`. |
| G2-D03/D04/D05 lint unused/duplicate/invalid | DELETED | 2b7b668 | integration tests removed | stats-reset guard ported first (G2-B07); invalid in-progress guard lives in analyzer (G2-B09). `humanSize` moved to `format.go`. |
| G2-D06 lint bloated_table | DEFERRED | — | — | Not small: its `VACUUM FULL` SQL would be projected by `cases.tableBloatCandidate` as a *safe, executable* `vacuum_table` candidate. Needs a non-executable pg_repack/bloat_remediation proposal first. Comment in `linter.go` records this. |

## Cross-area edits

- `sidecar/cmd/pg_sage_sidecar/main.go` — G2-B15 (move two `Run` starts below setters), G2-B26 (two args).
- `sidecar/internal/api/cases_handlers.go` — one line: `ObservedAt: timeFromMap(row, "last_seen")` (G2-B24).
- `sidecar/internal/value/postgres.go` — sort report rows (G2-B25).

## Coordination notes for other areas

- **RCA** (`rca/signals.go` `detectCacheHitDrop`): call `analyzer.NormalizeCacheHitRatio` on
  `CacheHitRatio` before comparing (fraction thresholds). Not edited here.
- **Collector**: emit cache hit ratio as a fraction and `-1` for no data (consumers already accept
  either unit); add `nspname` + ordered `conkey` to FK rows (G2-B08 ambiguous-schema case); add
  `min_value`, `start_value`, `cycle` to sequence rows (C18); per-query blk time columns (G2-B17).
- **Executor**: lock-chain findings now carry the evidence its approved-signal recheck reads.
  `DROP INDEX` / `CREATE INDEX` recommendations now use quoted identifiers.

## Known limitations (not regressions)

- After a restart, an existing open `invalid_index` finding is resolved once (first observation
  emits nothing) and reopened next cycle.
- LLM producers (optimizer/advisor/tuner) and supplemental detectors still only resolve categories
  they emit; their "empty" result means "skipped this cycle".
- `TestPostgresLockTimeoutIsBounded` (`internal/vectorlab`, not touched) failed once in the full
  `./internal/...` run under parallel load and passed in isolation; unrelated to this branch.

## Test Results

**Command:** `go test -count=1 -cover -v ./internal/analyzer/ ./internal/forecaster/ ./internal/schema/lint/ ./internal/schemaguard/ ./internal/cases/ ./internal/value/ ./internal/api/ ./cmd/pg_sage_sidecar/`
with `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`;
plus `go test -tags=integration -count=1 -cover -v` on the six owned/edited internal packages;
plus `go test -count=1 ./internal/...`; `go build ./...` and `go vet ./...` clean.

**Total:** unit run 1893 passed, 0 failed, 0 skipped; integration-tag run 806 passed, 0 failed,
0 skipped; `./internal/...` 52 packages ok, 1 flaky unrelated failure (vectorlab, passes alone).

**Coverage:**
| Package | Unit | -tags=integration |
|---|---:|---:|
| internal/analyzer | 84.9% | 84.5% |
| internal/forecaster | 87.4% | 92.9% |
| internal/schema/lint | 75.9% | 75.9% |
| internal/schemaguard | 87.9% | 87.9% |
| internal/cases | 88.7% | 88.7% |
| internal/value | 91.5% | 91.5% |
| internal/api | 71.8% | — |
| cmd/pg_sage_sidecar | 43.5% | — |

### Skipped Tests (must be zero or justified)
- None (grep of `-v` output for `--- SKIP`: 0 in both runs).

### Failures
- None in touched packages. `internal/vectorlab` `TestPostgresLockTimeoutIsBounded` — flaky under
  parallel package load (SQLSTATE 25P03), passes in isolation; package not touched.

### Coverage Gaps (packages below threshold)
- `cmd/pg_sage_sidecar`: 43.5% (wiring/entrypoint; unchanged from the review baseline of 43.5% in
  `raw-baseline-unit-db.txt`). Only lines were moved/changed here; no new logic added.
- All business-logic packages touched meet the 70% threshold.

### Bugs Found This Session
1. [BUG] analyzer/finding.go — suppressed identity reopened each cycle and executor-eligible (G2-B01/C02).
2. [BUG] analyzer/finding.go — refresh left stale rollback_sql (C04).
3. [BUG] analyzer/analyzer.go — last finding in a category never resolved (G2-B02/C03).
4. [BUG] analyzer/rules_vacuum.go — xid_wraparound dropped by self-monitor filter (G2-B03).
5. [BUG] analyzer/rules_system.go, forecaster — cache-hit unit mismatch (G2-B04/C01).
6. [BUG] analyzer/dedup.go, rules_autovacuum_tuning.go — only one table ever tuned (G2-B05).
7. [BUG] analyzer/analyzer.go — critical findings paged every cycle (G2-B06/G7-B07).
8. [BUG] analyzer/rules_index.go — unused-index drop right after stats reset (G2-B07).
9. [BUG] analyzer/rules_index.go — FK rule wrong schema / composite / INCLUDE (G2-B08).
10. [BUG] analyzer/rules_index.go — in-progress CIC recommended for drop (G2-B09).
11. [BUG] schema/lint/rule_overlapping_index.go — PK/unique drop recommended (G2-B10).
12. [BUG] analyzer/index_parser.go — greedy parser (G2-B13).
13. [BUG] schema/lint/linter.go — failing rule resolved its findings (G2-B14).
14. [BUG] cmd main.go — analyzer configured after Run (G2-B15); fleet PG version (G2-B26).
15. [BUG] forecaster/rules.go — active vs total backends (G2-B18); datasource.go lifetime counters (C10).
16. [BUG] identifier quoting in analyzer/lint/cases DDL (G2-B22/G4-B23/C16).
17. [BUG] analyzer/rules_query.go — zero threshold +Inf (G2-B23).
18. [BUG] cases — observed_at/expiry recomputed per request (G2-B24).
19. [BUG] value/postgres.go — random row order (G2-B25).
20. [BUG] schema/lint/rule.go — exclude_schemas dropped (G2-B27).
21. [BUG] analyzer/rules_workmem_promotion.go — inflated hint counts (G2-B28).
22. [BUG] analyzer — historical averages read wrong JSON key (G1-B06).
23. [BUG] analyzer/rules_sequence.go — descending sequences never reported (C18/G1-B35).
24. [BUG] (found during fix) analyzer/cycle.go — seq-scan watchdog FK skip compared
    `schema.table(col)` to `schema.table`, so it never skipped; fixed with G2-B08.

### Manual Checks Remaining
- CHECK-01: MANUAL — end-to-end suppress → next cycle → executor in a running sidecar (unit/DB
  tests cover upsert, in-memory list and dispatch; e2e not run per brief).
