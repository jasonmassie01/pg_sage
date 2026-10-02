# Phase 0 tuning correctness (items 7, 8, 10, 11)

Branch `claude/p0-tuning-correctness` from master v1.8.1 (`9c52624`). Evidence:
`reviews/2026-10-02-ai-next/ROADMAP.md` (Phase 0 table), `tuning.md`, `platform.md`. Tests were
written and committed first (`826e7ee`..`3975d15`), then the implementation. Nothing is pushed.

## What was built

### Item 7: optimizer what-if gate, expression indexes, partitions, collation

- **Per-query isolation.** Each workload EXPLAIN in the HypoPG session runs in its own
  savepoint (`optimizer/hypopg_session.go`). A query that cannot be planned is counted as
  `Failed` and skipped; before this, one bad query aborted the whole shared transaction.
  A query planned without the index but not with it also counts as failed. Cancellation
  still aborts, and a savepoint that cannot be rolled back is a session error.
- **Call-weighted improvement** (`optimizer/whatif.go`). Each query is weighted by its
  `total_exec_time`. If no query has total time, the weight is calls, and after that each
  query counts equally. A 90% win on the hot query is no longer averaged down to 9% by cold
  queries.
- **Explicit verdict.** Every recommendation now has a `what_if_verdict`:
  - `verified`: every query was measured, the index has a size, and the gain is at least
    the minimum.
  - `rejected`: every query was measured and the gain was too small. The recommendation is
    dropped.
  - `unverified`: HypoPG is unavailable, an error occurred, nothing could be measured, the
    size is unknown, or any query could not be planned.

  Unverified recommendations are kept and carry a `what_if_reason`. A `validated` flag that
  the LLM writes itself is now ignored. Reloaded open candidates keep the verdict they had
  when stored (`openrecs.go`).
- **Executor gate** (`executor/whatif_gate.go`, one call in `findingRequest`). An optimizer
  `CREATE INDEX` finding whose verdict is not `verified` gets the `approval_required`
  guardrail. The standing gate then queues it for an operator, even at full autonomy. A
  missing verdict (rows written before this change) fails closed.
- **Expression indexes** (`optimizer/indexkeys.go`). Key and INCLUDE lists are parsed with
  the existing balanced-paren DDL scanner. `(lower(email))` and `((payload->>'k'))` are read
  as expressions, and each must reference at least one existing column. Plain columns and
  INCLUDE columns must exist.
- **Duplicate check.** It compares method, keys and expressions, the INCLUDE set and the
  predicate. Casts and grouping parentheses are normalized, so `lower((email)::text)` from
  the catalog matches `lower(email)` from the LLM. Invalid indexes are ignored.
- **Volatility check.** Every function in the keys and the predicate is checked against
  `pg_proc`. A function with no IMMUTABLE overload is rejected.
- **Partitioned parents** (`optimizer/partition_plan.go`). These become advisory: the
  finding has no executable SQL and carries a `partition_plan`. The plan is `CREATE INDEX
  ... ON ONLY parent`, then for each partition `CREATE INDEX CONCURRENTLY` and
  `ALTER INDEX ... ATTACH PARTITION`. Child index names are unique per partition and fit in
  63 bytes. With multi-level partitioning there is no plan, only an explanation.
- **Collation** (`optimizer/collation.go`). It is read from `pg_database.datcollate`; the
  `lc_collate` GUC was removed in PG16. The builtin provider maps to `C` and ICU to
  `ICU <locale>`. A failure is logged, and the collation is no longer silently reported as
  `C`.

### Item 8: analyzer and optimizer self-load

- **`buildHistoricalAverages`** (`analyzer/analyzer_checks.go`) now picks its sample in SQL:
  - It numbers the lookback's non-empty snapshots by reading only ids and timestamps.
    `pg_column_size` skips empty and null snapshots without detoasting them.
  - It keeps every ceil(total/100)-th snapshot, so at most 100, and expands only those.
  - Malformed rows and elements are skipped instead of failing the query.
  - A lookback of 0 or less uses the 7-day default; before, it read nothing.
- **Optimizer cold start.** The check reads at most `min_snapshots` rows (`coldstart.go`)
  instead of `COUNT(*)` over the whole table.

### Item 10: live schema families vs leftover copies

`analyzer/clone_schemas.go` and `analyzer/clone_activity.go`. A clone family (at least 5
schemas with the same tables and a generated suffix) is called a **leftover** only when it
is idle. Idle means all of the following:

- No scan or tuple activity since the stats reset. Counters that stayed unchanged for the
  whole idle window (`unused_index_window_days`, tracked across cycles) also count as idle.
- No `pg_stat_statements` text names a member schema.
- The set of sessions is known, and no session holds locks in or runs statements on a
  member schema. If the session lookup fails, the family is never treated as idle.

A family that is not idle is a **schema family** (for example multi-tenant):

- Each distinct issue is reported once, on its most severe instance. That finding keeps its
  own SQL and gains `affected_schemas` (up to 100 names), `affected_schema_count` and
  `schema_family`.
- One info finding describes the family and never suggests a drop.
- Idle leftovers still collapse into one finding, as before (lifeos-1).

### Item 11: tuner heuristics and hint state

- **Real plans and catalog facts.** Heuristics now run on real plans with catalog facts
  (`tuner/catalog_facts.go`, `plan_heuristics.go`, `plan_filter.go`). The facts are each
  table's `reltuples` and its valid, non-partial btree indexes with a plain leading column,
  loaded once per cycle. Plain EXPLAIN has no schema, so an unqualified relation resolves
  only when exactly one schema has it.
- **`parallel_disabled`.** Only a serial Seq Scan is flagged: not parallel-aware, not under
  Gather or Gather Merge, on a table with at least `parallel_min_table_rows` rows (the
  setting was never used before).
- **Seq scan to IndexScan.** A hint is made only when all of these hold: the table has at
  least 10k rows, the scan returns at most 10% of it, and a top-level conjunct of the Filter
  compares the leading column of a usable index with a btree operator. The hint names that
  index.
- **Join hints.** `HashJoin(...)` lists the real aliases of the join's children, excluding
  SubPlans and InitPlans, and needs at least two of them. Identifiers that pg_hint_plan
  would need quoted are skipped. An empty hint is never installed or recorded.
- **`sage.query_hints` status** (`tuner/hint_status.go`). A proposal is `proposed`. Each
  cycle reconciles it:
  - to `active` once that exact hint text is installed in `hint_plan.hints`;
  - to `rolled_back` (with `rolled_back_at`) once the installed hint is gone.

  Proposed hints still count for the restart cooldown.
- **Real plan fixtures.** `TestGeneratePlanFixtures` captures 13 real EXPLAIN (FORMAT JSON)
  plans plus `catalog.json` from the test server into `internal/tuner/testdata/plans`. They
  were generated on PG17 and are committed. Regenerate them with
  `PG_SAGE_REGEN_PLAN_FIXTURES=1`.

## Product decisions (made under the "earn trust, then autonomy" principle)

1. **Unverified means approval, including when HypoPG is not installed.** An index that
   pg_sage could not measure end to end is evidence-free. It is still recommended, with
   the reason, but never auto-applied. This changes behavior for installs without HypoPG:
   those indexes now wait for an operator instead of running at the moderate tier. Advisory
   confidence without HypoPG is unchanged.
2. **A partial measurement can't reject.** If some queries could not be planned, the
   unplanned ones may be the beneficiaries. The result is `unverified`, not `rejected`.
3. **Partitioned parents are advisory** rather than executed as a multi-statement plan.
   The executor runs one statement per action. A partially attached index is a state that
   verify-and-revert does not model yet.
4. **History sampling is even across the lookback, at most 100 snapshots.** I first drafted
   "first and last snapshot per day". I chose even sampling instead because it keeps the
   estimator the analyzer has always used, and the bound is a constant. The drafted test
   was revised, and the commit message says why.
5. **The schema-family window reuses `unused_index_window_days`** (default 7) and does not
   add a new setting. The quiet tracker lives in memory, so after a restart a family with
   past activity shows as a schema family until the window passes again. That errs toward
   never hiding findings.
6. **The tuner's index hint requires catalog evidence** (size, selectivity, usable index).
   With no facts, no catalog-dependent symptom is emitted (fail quiet).

## Spec CHECKs

No `AI-SRE-SPEC` CHECK targets these tuning items directly; they serve the product rule
"never grant autonomy that evidence has not earned". The work's own checks:

```
CHECK-01: [PASS] An optimizer index with a failed or partial what-if queues for approval at full autonomy
CHECK-02: [PASS] One unplannable workload query does not abort the HypoPG run (real HypoPG)
CHECK-03: [PASS] Hot-query win dominates call-weighted improvement (real HypoPG)
CHECK-04: [PASS] (lower(email)), ((payload->>'k')) admitted; catalog-form duplicates detected
CHECK-05: [PASS] Partitioned parent never gets CONCURRENTLY on the parent
CHECK-06: [PASS] Regression baseline expands <= 100 snapshots (EXPLAIN ANALYZE loop count)
CHECK-07: [PASS] Cold-start check scans <= min_snapshots rows (EXPLAIN ANALYZE)
CHECK-08: [PASS] Live tenant family: findings fanned out, none hidden, no drop suggested
CHECK-09: [PASS] Real plans: no parallel symptom under Gather/Gather Merge; HashJoin(c o) from children
CHECK-10: [PASS] query_hints proposed -> active -> rolled_back against real hint_plan.hints (PG17/PG18)
```

## Test Results

**Command:** `go test -p 2 -count=1 -cover -v ./...` (repo root mounted, `--cpus=2`, PG17
`pgsage-ag4` :55474). The touched packages (`optimizer`, `analyzer`, `tuner`, `executor`)
were also run on PG17 with `-race`, PG14 (:55414) and PG18 (:55418). The e2e tag was run
with `-run TestTunerAllHints`.

**Total (PG17 full suite):** 8158 top-level tests passed, 1 failed, 21 skipped across 80
packages.

**Coverage (touched packages):**

| Package | PG17 | PG17 -race | PG14 | PG18 |
|---|---|---|---|---|
| internal/optimizer | 83.8% | 83.8% | 82.4% | 83.8% |
| internal/analyzer | 86.5% | 86.5% | 86.5% | 86.5% |
| internal/tuner | 85.0% | 85.0% | 83.3% | 85.0% |
| internal/executor | (failed run, see below) | 83.8% | 83.8% | 83.8% |

### Skipped Tests (must be zero or justified)
- In the touched packages on PG17 and PG18 only `TestGeneratePlanFixtures` is skipped. It
  runs only when `PG_SAGE_REGEN_PLAN_FIXTURES=1`, and it was run to create the fixtures.
- On PG14 the image ships pg_hint_plan < 1.7, whose `hint_plan.hints` has no `query_id`.
  pg_sage reports the hint table as not ready there. The 6 `hint_status_test` tests, 1
  db_coverage test and 1 review_hint_removal test skip for that reason, and
  `plancapture_generic_test` skips because GENERIC_PLAN needs PG16+.
- Other packages in the full suite skip 20 live-cloud, LLM, standby, PgBouncer and bench
  tests behind their env flags. They are unrelated to this branch.

### Failures (if any)
- `internal/executor`: `TestApplyLockCeilingCapsOperatorAnalyze` failed once in the full
  run. Its uncapped control waits for a lock and took 2.06 s against an expected ~4 s, a
  timing assertion on a shared Docker VM with load average around 60. It passed three
  times in a row in isolation, and the whole executor package passed on PG17 -race, PG14
  and PG18. This branch does not touch that path.

### Coverage Gaps (packages below threshold)
All touched packages meet the 70% threshold (lowest: optimizer 82.4% on PG14).

### Bugs Found This Session
1. [BUG] `optimizer/indexspec.go` `parseTail`: `WHERE` text was read with `rest()` without
   consuming it, so every partial index failed with "unexpected trailing text" and was
   rejected. Fixed with `consumeRest`.
2. [BUG] `optimizer/optimizer.go`: a `"validated": true` written by the LLM in its own JSON
   passed through when HypoPG was unavailable. It is now always reset.
3. [BUG] Tuner: plain EXPLAIN plans carry no `Schema`, so every catalog lookup keyed
   `public.<rel>` missed non-public tables. The new facts resolve unambiguous unqualified
   names. The existing stale-stats routing (`annotateForStaleStats`) still assumes
   `public`; see "What's left".
4. The Phase 0 items themselves, all confirmed by failing tests first:
   - HypoPG failed open.
   - Improvement was an unweighted mean.
   - Expression indexes were always rejected.
   - The duplicate check ignored method, INCLUDE and WHERE.
   - CONCURRENTLY was used on partitioned parents.
   - `SHOW lc_collate` failed on PG16+.
   - The analyzer read 7 days of history every cycle; the cold start ran `COUNT(*)`.
   - Live tenants were hidden.
   - Every scan was flagged parallel; every seq scan got an index hint.
   - `HashJoin()` and `HashJoin(<one>)` were emitted, and empty hints were recorded.
   - `query_hints` rows were marked `active` at proposal.

### Mutation testing
I ran 20 mutants (`p0tuning_mutate.py` in the scratchpad), each against its targeted tests
on PG17. All 20 were killed. Two needed follow-up:
- **M10:** removing the `LIMIT` survived because the sampling step already bounds the
  sample. The LIMIT was dead and was removed; the mutant that removes the step is killed.
- **M16:** the OR guard survived because EXPLAIN always parenthesizes OR operands. I added
  an unparenthesized case, which kills it.

The mutants were:
- what-if: partial failure verified, unweighted mean, executor ignores the verdict,
  failing EXPLAIN aborts the run;
- index shape: invalid index counted as duplicate, functions counted as column refs,
  expression refs unchecked;
- clone families: sessions ignored, activity window ignored, live family treated as a
  leftover;
- tuner: Gather ignored, one-alias HashJoin, selectivity gate removed, OR filters usable,
  ambiguous relation resolves, empty hint inserted, any installed hint activates;
- bounds: unbounded history, unbounded cold start;
- partitions: CONCURRENTLY on the parent.

### Manual Checks Remaining
- CHECK-11: MANUAL. The web query-hints page shows the new `proposed` and `rolled_back`
  statuses as plain text. Needs a browser check; no web code changed.

## Post-test audit

- **Untested inputs found and added:**
  - Function volatility against real `pg_proc` (`TestCheckExpressionVolatility_DB`).
  - Unparenthesized OR filters.
  - Malformed history snapshots: null, object, non-numeric fields.
  - Ambiguous unqualified relations.
- **Assertions that would pass if broken.** The clone tests also assert that input findings
  are not mutated. The hint-status tests read the database row, not only the returned
  finding. The fixture tests assert exact aliases and index names, not "some hint".
- **Fakes that hide real failures.** `fakeWhatIf` drives the verdict tests. The same
  behavior is checked on a real HypoPG session (`TestHypoPGCallWeightedAndIsolated`,
  `TestHypoPGFailureAndEmptyQueriesCleanSession`), and plan heuristics run on real EXPLAIN
  output.
- **Logically wrong old tests, changed with reasons in the commits:**
  - The hand-written tuner plans used shapes PostgreSQL never emits.
  - Some tests expected every Seq Scan to be flagged twice, or `HashJoin(<one alias>)` and
    `IndexScan(<alias>)` with no index.
  - The `IndexInfo` duplicate fixtures had no `IsValid`.
  - The HypoPG tests expected the run to abort on a bad query and inconclusive results to be
    "neutral".

## What's left

- `annotateForStaleStats` still canonicalizes plan relations to `public.<rel>`. Stale-stats
  routing for non-public schemas needs the same unambiguous resolution as `CatalogFacts`.
- The quiet-window tracker for schema families is in memory. Persisting it would let an
  idle family be called a leftover again soon after a restart.
- Executing a partition plan as one supervised multi-step action (and verifying it) is
  future work; today it is advisory.
- There is still no write-cost model in the index gate (tuning.md I3). It was not in this
  scope.

## Coordinator decisions

1. Do you agree that unverified optimizer indexes, including every index on installs
   without HypoPG, now need approval? (I chose yes.)
2. `CHANGELOG.md` gained a `## Unreleased` section with one bullet. Other branches add
   their own, so merge the sections.
3. Fixtures in `internal/tuner/testdata/plans` were generated on PG17. If another branch
   changes the plan-capture options (for example adds VERBOSE), regenerate them.
