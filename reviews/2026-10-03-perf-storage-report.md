# perf-storage report: how pg_sage's own data grows and is cleaned up

Branch `claude/perf-storage` (from `claude/perf-base`), head: see the end of this file.
Scope (FIX-BRIEF): query_store, snapshots, the retention engine, update patterns of pg_sage's
own tables. Not touched: collector catalog reads, API/events, SRE probes, the decision table.

## Summary

| Area | Before (perf-base) | After (this branch) |
|---|---|---|
| query_store rows per cycle, idle queries | every sampled query, every cycle (lifeos 290/cycle, 16,091/h) | only queries whose counters moved, plus one keyframe per query per hour: 100 idle queries x 60 cycles = 100 rows (was 6,000) |
| query_store statements per cycle | one INSERT per query, each with a correlated explain_cache subquery | one `INSERT ... SELECT FROM unnest(...) LEFT JOIN LATERAL` |
| query_store retention | snapshots_days (90 d), row DELETEs | `retention.query_store_days` (14 d), daily partitions dropped |
| query_store indexes | pkey (lifeos 108.5 MB, 0 scans ever) + qid_time + time | no pkey (no reader looks a sample up by id) |
| snapshots retention | 90 d row DELETEs (lifeos 58.9k deletes, 9.3 GB) | daily partitions dropped; size cap 5% of the DB (floor 256 MB), oldest day first, never a day a retained delta needs |
| retention run | unbounded DELETE loops, no pause, no time budget | 1,000-row batches (50 for snapshots), 50 ms pause, 30 s run budget with round-robin resume; TID-array deletes |
| explain_results retention | `created_at` (reset by each refresh: never expired) | `expires_at` + 1 day grace |
| action_queue retention | exempt, never deleted | decided proposals age out on actions_days unless an action/decision/SRE proposal points at them |
| agent_db_* (18 tables) | no rule, not in the guard test | rules per table (keep newest ping/attempt, live tokens, authorized estimates); guard test creates them |
| findings refresh | 0% HOT (`last_seen` indexed) | 100% HOT (gate, steady) |
| sre_change_feed_state | 16 updates / 6 cycles (gate), 3,328 on lifeos (8 rows) | 3 / 6 cycles: unchanged state is not written (not even locked) |
| incidents | every tracked incident rewritten each pass (TOAST 1.44 MB) | unchanged incidents skipped; causal_chain TOAST pointer kept when equal |
| sre_service_slos HOT | 62% | 100% (fillfactor 50) |

## Perf gate (small scale, PG17, 6 steady cycles, 90 s steady window)

Same fixture both runs: 500 tables, 1,500 indexes, 20,000 history rows per growing sage table.
Before = `claude/perf-base` with this branch's measurement commits only (no fixes); after =
branch head. Steady phase:

| Metric (steady phase) | Before | After |
|---|---|---|
| Offenders | 15 (17 with gate F: findings 0% HOT, sre_investigations 43% HOT) | 14 |
| Rows written, all sage tables | 842 | 726 |
| UPDATEs, all sage tables | 39 (about 24 HOT) | 17 (17 HOT) |
| Size growth, all sage tables | 15.9 MB/h | 10.3 MB/h |
| Distinct statements | 195 | 175 |
| findings: updates / HOT | 6 / 0% | 6 / 100% |
| sre_change_feed_state: updates | 16 | 3 |
| sre_service_slos: HOT | 62% | 100% |
| incidents: rows written | 3 | 0 |
| query_store: rows written / relations / growth | 348 / 1 / 1.25 MB/h | 308 / 5 (history, 3 days, default) / 0.00 MB/h |
| snapshots: growth | 2.81 MB/h | 2.50 MB/h |

Notes, honestly read:
- The gate fixture runs its workload every cycle, so nearly every sampled query moves and
  change-only sampling saves little there (58 -> 51 rows/cycle). The idle-query saving is proven
  by `TestRecorder_IdleAndMovingQueriesAgainstPostgres` (100 idle + 1 moving query, 60 cycles:
  160 rows (100 idle + 60 moving) instead of 6,060, one statement per cycle) and the collector test
  `TestRecordQueryStore_WritesOnlyMovedQueriesAcrossCycles`. On lifeos (290 sampled queries, 16,091
  rows/h), an idle query goes from 60 rows/h to 1.
- query_store's 0.00 MB/h is page allocation inside 90 s (rows fit pages the new day partition
  already had), not a claim of zero growth; per-row cost is lower (no pkey index).
- 15 -> 14 offenders is not the measure of this branch: the remaining 14 are outside its scope
  (API/events live-update polls of action_log/findings/health_history; decision withheld purge
  keep-predicate scans of verification/decision; analyzer history reader mean time, B). Two
  disappeared for reasons other than a fix here: sre_investigations had 5,000 live rows instead
  of 5,001 (gate A threshold is "above 5,000") and 0 updates in this steady window (gate F needs
  5). The runway prune is no longer a suspect (EXPLAIN test `TestRunwayPruneDeletesThroughTheKey`);
  one unattributed warmup scan of runway_samples remains, most likely the startup `loadLastSQL`
  read (1 call, 3,000 rows, owned by the runways/SRE branch).
- Retention ran in the gate: query_store went from 20,000 seeded rows (spread over 90 days) to
  5,155 by dropping/truncating expired days (14-day window), with no seq-scan offender.

Reports: `perfgate-before.md`, `perfgate-final.md` in the session scratchpad (not committed).
The fixture database is dropped by the test.

## What changed

### 1. query_store
- `querystore.Recorder` (internal/querystore/recorder.go) remembers what it last wrote per queryid
  and writes a sample only when calls, total time or the stats epoch moved, or when an hour passed
  (`KeyframeInterval`). It forgets queries not sampled for two hours, so memory is bounded by the
  sampled set. A failed write is retried next cycle (state updated only after success).
- One statement per cycle: `INSERT ... SELECT FROM unnest($1..$6) LEFT JOIN LATERAL (latest
  plan_hash) ` — the plan_hash join runs once per statement, not once per row.
- Readers treat a missing sample as "unchanged": preflight evidence (querystore/evidence.go),
  verify's query measurement (verify/postgres.go) and the SLO proxy latency
  (sre/slo/proxy_latency.go) anchor a window on the last sample before it, within
  `AnchorLookback` = 2 h (twice the keyframe interval). An anchor from another stats epoch is a
  reset, as before.
- Own retention: `retention.query_store_days` (default 14, YAML-only; readers look back at most
  7 days).
- `query_store_pkey` dropped. Proof no reader needs it: every reader (evidence, verify, SLO proxy,
  probes, sre-bench) filters by `queryid`/`captured_at`; `id` appears only as an ORDER BY tie-break
  on rows already selected by `(queryid, captured_at)`; lifeos shows 0 scans of the pkey ever
  (measured.md M11). `idx_query_store_qid_time` and `idx_query_store_time` remain (now
  per-partition).
- Partitioned by UTC day (decision below).

### 2. snapshots
- Partitioned by UTC day; primary key becomes `(id, collected_at)` (a unique index on a
  partitioned table must contain the partition key). `base_id` references stay by `id`.
- The writer starts a new keyframe each UTC day (snapstore/writer.go `usable`), so a day's deltas
  only reference that day's keyframe and dropping old days never orphans a retained delta;
  `sage.snapshot_data(data, base_id, collected_at)` reads the base from the same day's partition
  first (partition-pruned), falling back to any partition for rows written before this change.
- Size-aware retention (retention/cap.go): `retention.snapshots_max_pct` (default 5% of
  `pg_database_size`, floor 256 MB, YAML-only). Over the cap, whole days are dropped oldest first;
  today and the default partition are never dropped; a day whose keyframe a retained delta in a
  later day still references is kept (`removable`).

### 3. Retention engine
- `Cleaner.RunOnce`: each rule deletes in batches (1,000 rows; 50 for snapshots, whose rows are
  multi-MB TOAST), pauses 50 ms between statements and stops at the run budget (30 s); the next
  run resumes at the rule it stopped at (round-robin), so no rule starves. RunStats reports
  deleted rows, batches, statements, dropped/truncated partitions, deferred rules, elapsed time.
- Deletes collect the batch's ctids first (`ctid = ANY (ARRAY(SELECT ctid ... LIMIT n))`): with
  `ctid IN (subquery)` the planner hash-joined a full scan of the table (EXPLAIN-asserted test).
  Same fix for the rca incident prune and the runway sample prune (both were gate A offenders or
  plans with a seq scan of a 20k-row table).
- Partitioned tables: expired days are dropped (`DROP TABLE` with a 2 s lock_timeout; a busy day
  is retried next run), the history partition is truncated once all its rows are expired, rows
  are deleted only from history/default, oldest first along the time index.
- explain_results on `expires_at` (+1 day grace), findings on `resolved_at` (new column filled at
  resolution; backfilled from last_seen), action_queue rule with keep predicate, agent_db_* rules
  (agent_rules.go) and exemptions (exemptions.go: cost_samples is the budget ledger).
- `TestRetentionRules_CoverEveryTimeSeriesTable` now creates the agent schema first and requires >= 18
  agent tables to be covered; a new test requires every partitioned table to have a rule.

### 4. Update patterns
- findings: indexes that contained `last_seen` replaced (`idx_findings_status` ->
  `(status, severity)`, `idx_findings_schema_lint` -> partial on open lint, retention reads
  `idx_findings_resolved_at`), fillfactor 80. A refresh is now a heap-only update (test reads
  `n_tup_hot_upd`, PG15+). Guard test: no index on a column the refresh writes.
- sre_change_feed_state: the poller skips the upsert when the state is unchanged; the upsert also
  carries `WHERE state IS DISTINCT FROM EXCLUDED.state` (test: xmax unchanged, row not locked).
- incidents: `persistItem.unchanged` compares a fingerprint of the persisted fields with what
  this process last wrote and skips the row; `causal_chain` is written with
  `CASE WHEN causal_chain IS DISTINCT FROM $n THEN $n ELSE causal_chain END`, keeping the TOAST
  pointer (test: TOAST relation size unchanged after an unchanged persist).
- fillfactor: findings 80, recommendation 90, decision 90 (reloption only: `ALTER TABLE SET`
  takes SHARE UPDATE EXCLUSIVE, no rewrite), incidents 80, sre_change_feed_state 50,
  sre_service_slos 50, sre_investigations 70. Small hot tables (incidents, change_feed_state,
  service_slos, investigations) get `autovacuum_vacuum_threshold=10,
  autovacuum_vacuum_scale_factor=0.05`.
- All in one idempotent migration (internal/schema/storage_migration.go) that checks pg_indexes
  before each CREATE INDEX CONCURRENTLY; no index on sage.decision; schema_version not bumped.

### 5. Perf gate additions
- Table statistics roll partitions up to their partitioned table (partitioned parents excluded:
  they have no heap); gate A judges the scanned relation's rows, so seq scans of an empty daily
  partition are not offenders.
- New gate F "HOT share of updates": a table updated 5+ times in the steady phase must have at
  least 50% HOT updates. Report table gains relations, updates, HOT %, MB, MB/hour.

## Decisions

1. **Daily partitioning for query_store and snapshots: yes.** Retention becomes `DROP TABLE` of a
   day instead of millions of row deletes (lifeos: 2.87M query_store and 58.9k snapshot deletes so
   far, with their WAL and vacuum). Layout: one `_history` partition holding everything before the
   upgrade (in-place conversion, no data copy, no heap read under the exclusive lock: see
   "Follow-up"), one
   partition per UTC day, a default partition for rows dated ahead. Partitions for today and
   tomorrow are created by the collector before it writes and by retention. Cost: retention
   granularity is one day (rows live up to 1 day past the window); readers of whole tables (none
   in hot paths) see N partitions; `CREATE INDEX CONCURRENTLY` cannot run on these two tables.
2. **Snapshot keyframe per UTC day.** Makes partition drops safe without a cross-day reference
   walk. Cost: one extra keyframe per category per day (11/day).
3. **query_store change-only with a 1 h keyframe and 2 h anchor lookback.** Rows written scale
   with activity, not with the number of statements tracked. A query idle for more than 2 h and
   then measured has no anchor: its first window is unknown rather than wrong.
4. **query_store pkey dropped; snapshots PK (id, collected_at).**
5. **Snapshots size cap = 5% of the database, floor 256 MB**, YAML-only (`snapshots_max_pct`;
   0 = off). The floor keeps tiny databases from losing history to the percentage.
6. **Retention pacing: 50 ms pause, 30 s budget per run, 1,000/50 row batches.** Fixed
   constants, not settings (no DBA tuning).
7. **explain_results: 1-day grace after `expires_at`**; off when explain retention is off.
8. **action_queue and agent_db_* rules as listed above; `agent_cost_samples` exempt** (budget
   ledger, read for spend totals).
9. **New keys YAML-only** (`retention.query_store_days`, `retention.snapshots_max_pct`), restart
   lifecycle, documented in config.example.yaml and generated docs.
10. **Gate F added** (steady phase only, >= 5 updates, >= 50% HOT): warmup holds one-time
    resolutions (status and resolved_at are indexed, rightly not HOT).

## Follow-up: a conversion that is safe for lifeos-sized tables

Coordinator request after 43d0c1b4: lifeos has `sage.snapshots` at 9.3 GB and
`sage.query_store` at 852 MB. The first conversion held ACCESS EXCLUSIVE while ATTACH validated
the history bound with a heap scan. For snapshots it also built the `(id, collected_at)` key under
that lock. Every reader and writer of the table waited for that whole time.

### What changed

1. **No heap read under the exclusive lock** (partition/convert.go, convert_prepare.go).
   - Before the exclusive transaction:
     - `ADD CONSTRAINT <t>_cutover_check CHECK (<key> IS NOT NULL AND <col> IS NOT NULL AND
       <col> < cut) NOT VALID`. This is a catalog change. It takes ACCESS EXCLUSIVE only
       briefly, with the 2 s lock timeout. Note: ADD CONSTRAINT takes ACCESS EXCLUSIVE even with
       NOT VALID, not SHARE UPDATE EXCLUSIVE.
     - `VALIDATE CONSTRAINT`, under SHARE UPDATE EXCLUSIVE, so writers keep running.
     - For snapshots, `CREATE UNIQUE INDEX <t>_cutover_key (id, collected_at)`. It is built
       CONCURRENTLY only when the heap is over 32 MB. A smaller key builds in well under a
       second under a SHARE lock, within the lock timeout. CONCURRENTLY waits for every older
       snapshot in the database, and under bootstrap's advisory lock that deadlocked against a
       second instance waiting for the lock (found by the full suite, bug 11). Bootstrap only
       converts heaps up to 32 MB.
   - The exclusive transaction only changes the catalog:
     - The pre-built index becomes the history table's primary key (`ADD CONSTRAINT ... PRIMARY
       KEY USING INDEX`). The CHECK proves the key columns NOT NULL, so this does not scan.
     - The parent's copy of the CHECK (copied by `LIKE`) is dropped.
     - `ATTACH ... FROM (MINVALUE) TO (cut)` finds the bound implied by the validated CHECK and
       adopts the existing indexes.
     - Then the CHECK is dropped and the empty default partition is created.
   - **The cut** is the second UTC midnight after `now()`, or after the newest row when rows are
     dated ahead. A row written between VALIDATE and the lock is therefore at least a day below
     the cut and cannot violate the CHECK.
   - A row more than 7 days ahead is a broken clock. The conversion refuses with
     `ErrFutureRows` rather than stretch the history partition over months.
   - Cost: the history partition now also covers tomorrow, one day more than before.
   - A session advisory lock serializes conversions and cleanups. A second converter gets
     `ErrBusy`.
2. **Failure behaviour.**
   - Any failure undoes the preparation on its own bounded context, so a cancelled bootstrap
     still cleans up. It drops the CHECK and the pre-built key. The exclusive transaction rolls
     back, so no renamed objects are left. The session's timeouts and advisory lock are released
     and restored.
   - The undo may itself fail, for example when the lock is still unavailable. In that case
     `partition.Cleanup` removes the leftovers on the next retention run, every run, independent
     of the backoff. A leftover CHECK would refuse every insert once the clock passes the cut,
     which is at least a day away.
   - **Bootstrap** (startup has a 10 s deadline) converts a table only when its heap is at most
     32 MB.
     - A larger table is left to retention.
     - A failed conversion logs one `slog.Warn` with a hint and does not fail startup.
     - The `stats_epoch` and `base_id` ADD COLUMN migrations now check the catalog first. Before,
       they took ACCESS EXCLUSIVE on every start, so a long reader of either table stalled
       startup and everything queued behind it.
   - **Retention** (`Cleaner.Run`) does two catalog reads per run.
     - A plain table that is due is converted on a background goroutine, so a long VALIDATE never
       holds the orchestrator cycle.
     - Retries back off at 1 h, 2 h, 4 h and so on, up to 24 h. A success resets the backoff.
     - Each failed attempt logs one WARN: what failed, that pg_sage keeps working on the plain
       table and deletes expired rows in paced batches, when the next attempt is, and what to do
       (`partition.Hint`). The hint distinguishes a lock timeout (end long transactions or
       restart at a quiet time), a statement timeout, a full disk, a broken clock and a cancel.
     - Until the conversion succeeds, the plain table is purged by the paced row deletes (tested:
       2,500 expired rows deleted in batches of 1,000).
3. **Disk headroom.**
   - No row is copied: the cut is past every row.
   - The old table is not rewritten. A test asserts the history partition keeps the plain
     table's relfilenode and TOAST relfilenode, and the default partition holds 0 rows, also
     with a row dated 3 days ahead.
   - The only extra space is the pre-built `(id, collected_at)` key of snapshots. On lifeos that
     is 50.9k rows, a few MB.

### Measured (PG17 `pgsage-ag3`, query_store-shaped plain table, writer inserting every 2 ms)

`go test -tags=convertscale -run TestConvertAtScale ./internal/partition` with
`PG_SAGE_CONVERT_MB=250` and `=1000`. The "before" numbers come from the 43d0c1b4 conversion code
with the same harness. Fixtures were 367 MB and 1.09 GB in the package's throwaway database,
dropped after each run (C: had 9.4 GB free before and after).

| Fixture | Code | Exclusive lock held | VALIDATE (writers running) | Longest writer stall | Writes failed |
|---|---|---|---|---|---|
| 2.0M rows, heap 240 MB, 367 MB total | before | whole conversion, 184 ms | (inside the lock) | 154 ms | 0 |
| same | after | 43 ms | 83 ms | 39 ms | 0 |
| 6.0M rows, heap 721 MB, 1.09 GB total | before | whole conversion, 313 ms | (inside the lock) | 292 ms | 0 |
| same | after | 104 ms | 293 ms | 98 ms | 0 |

- **Heap blocks read in the exclusive transaction:** 0
  (`TestConvert_ExclusiveWindowReadsNoHeap`, `pg_stat_get_blocks_fetched` delta around the
  transaction). That holds both with and without a key, on PG17 and PG18. PG14 skips this test
  because it has no forced stats flush. Before, the transaction read the whole heap: a
  validation scan, plus an index build for snapshots.
- **Warm cache:** the fixtures were just loaded, so the heap was cached. The old conversion's
  stall is a heap read, which from disk is much slower than these numbers. The new exclusive
  window reads no heap block, so the cache does not matter to it.
- **What still grows with size in the new lock window** (43 to 104 ms) is not heap reading. The
  transaction drops the old single-column primary key, and the likely cost is unlinking that
  index's files at commit. I have not separately verified this.

**Extrapolation to 9.3 GB**, taking the worst case that all of it is heap. Linear in heap size
from the two points: stall about 0.29 ms/MB warm.

- **Before:**
  - Warm cache: about 2.8 s with every reader and writer of the table blocked.
  - Cold, at a 200 MB/s disk read: about 47 s, plus the snapshots key build under the same lock.
  - Either way above the 10 s startup deadline once cold: the old code ran the conversion inside
    bootstrap, so the upgrade would have failed startup.
  - lifeos's real heaps are smaller: query_store heap 402 MB, so about 0.2 s warm and 2 s cold;
    the snapshots heap holds 50.9k rows and its 9.3 GB is TOAST, which validation does not read.
- **After:**
  - The exclusive window does not depend on heap size. It grew 43 to 104 ms across the two
    fixtures. Scaled by the dropped primary key, lifeos's would be about 0.1 s for query_store
    (108.5 MB pkey) and less for snapshots (2.4 MB pkey). A straight-line worst case at 9.3 GB of
    heap is about 1.2 s.
  - VALIDATE scales with the heap (0.4 ms/MB warm: about 4 s warm, about 47 s cold at 9.3 GB),
    but writers keep running. It is bounded by a 10 min statement timeout. A timeout counts as a
    failed attempt: retried with backoff, plain table kept.

### Tests (written first, d61a6036)
- partition:
  - `TestConvert_ExclusiveWindowReadsNoHeap`, `TestConvert_CopiesNoRows`,
    `TestConvert_CutLeavesADayOfMargin`, `TestConvert_RowsFarAheadAreRefused`.
  - `TestConvert_LockTimeoutAtCutoverLeavesPlainTable`,
    `TestConvert_LockTimeoutAddingCheckLeavesPlainTable`,
    `TestConvert_StatementTimeoutValidatingLeavesPlainTable`,
    `TestConvert_DiskFullInsideTransactionLeavesPlainTable` (injected SQLSTATE 53100) and
    `TestConvert_CancelledContextStillCleansUp`. Each asserts relkind `r`, no cutover CHECK or
    key, no `_history`/`_default` table, no advisory lock held, and a later successful Convert.
  - `TestCleanup_RemovesLeftoversOfACrashedConversion` (also ErrBusy while another session holds
    the conversion lock), `TestConvert_ConcurrentWriterNeverFails`,
    `TestConvert_RestoresSessionSettings`.
  - `TestConvert_SmallTableKeyBuildDoesNotWaitForOtherSessions` (added after the full suite
    found the deadlock; it fails with CONCURRENTLY).
- schema: `TestStorageMigration_LargePlainTableIsLeftToRetention` and
  `TestStorageMigration_FailedConversionDoesNotFailBootstrap` (a long reader on query_store; 10 s
  deadline).
- retention: `TestConversionBackoffDoublesToADay`,
  `TestConvertHistory_FailsSoftRetriesWithBackoffThenSucceeds` (one WARN naming the lock and
  `1h0m0s`, no retry inside the backoff, paced purge of the plain table, success after the
  backoff), `TestRun_DoesNotWaitForTheConversion` and
  `TestConvertHistory_RemovesLeftoverCheckEveryRun`.
- **Existing tests changed because behaviour changed:** the partition tests now expect the
  history partition to end two midnights out (Ensure creates 2 of 4 days, 3 of 5 concurrently).
  `TestConvert_RowsDatedAheadWaitInDefault` became `..._StayInHistory`, because rows ahead no
  longer move to the default partition. Keeper coverage is read from the history bound.
- **Test bugs fixed while making them pass:** relfilenode is `oid` and must be cast to `bigint`
  to scan. The async-run test now turns off the query_store purge, because that DELETE queues
  behind the attempt's queued lock request, which is bounded by the 2 s lock timeout but is not
  what that test measures.
- **Mutation testing:** 17 mutants, all killed:
  - drop the CHECK before ATTACH
  - skip VALIDATE
  - no undo on failure
  - one-day cut
  - no future-row guard
  - pre-built key read into the plan
  - no session restore
  - no PRIMARY KEY USING INDEX
  - no leftover cleanup at start
  - no backoff doubling
  - synchronous conversion in Run
  - no per-run cleanup
  - no backoff check
  - bootstrap converts any size
  - bootstrap fails on a failed conversion
  - unguarded ADD COLUMN
  - always build the key CONCURRENTLY (killed by
    `TestConvert_SmallTableKeyBuildDoesNotWaitForOtherSessions`: deadlock detected)

## For the coordinator / other branches

- perf-api proposes an index on findings `(status, last_seen DESC, id DESC)`: it would make every
  refresh non-HOT again. A guard test here (`TestStorageMigration_FindingsIndexesLeaveLastSeenOut`) fails if
  any findings index contains a column the refresh writes. Suggest a different key or keeping it
  off this branch's findings table.
- `CREATE INDEX CONCURRENTLY` is rejected on the partitioned `sage.query_store` and
  `sage.snapshots`. Any branch adding an index there (e.g. a proposed `idx_query_store_planned`)
  must create it per partition or with `CREATE INDEX ON ONLY` + attach.
- analyzer `query_history.go` now joins picked snapshots on `(id, collected_at)`; overlaps with the
  snapshot-readers work (B offender on perf-base: the history reader's mean time).
- Left as offenders (out of this scope): live-update polls of action_log/findings/health_history
  (API/events), decision withheld purge keep-predicate scans of verification/decision (decision
  table), sre_investigations HOT 43% (`updated_at` is indexed in the SRE store; re-keying it is an
  SRE store change), sre_autonomy_events startup update, incidents warmup scans.
- `reviews/2026-10-03-perf/FIX-BRIEF.md` is left untracked (coordinator's file).

## Test Results

**Command:** `go test -p 2 -count=1 -cover -v -timeout 3600s ./...` (sidecar, golang:1.25 in
Docker, `--cpus=2`, PG17 `pgsage-ag3` :55473)
**Total (final run, after the follow-up, head a957cabc):** 11,426 passed (tests and subtests),
0 failed, 21 skipped. 85 packages ok. internal/rollout, which this branch does not touch, reported
FAIL only because its fixture-database cleanup timed out connecting to the shared server; all
its tests passed. A rerun gave ok, and I dropped the database left behind. The first full run
of the follow-up found the CIC deadlock (bug 11). In that run an executor timing test
(`TestApplyLockCeilingCapsCycleAnalyze`, package not touched) also failed under load; it passed
in the final run. Before the follow-up: 11,405 passed, 0 failed, 21 skipped, 86 ok.

Also run:
- Touched packages (16: partition, querystore, retention, snapstore, schema, verify, sre/slo,
  sre/changefeed, rca, analyzer, collector, config, store, runway, testsupport/perfgate, smoke)
  on PG14 (:55414) and PG18 (:55418): all ok. On PG14 two new tests failed first (bugs 7 and 8);
  fixed in e3ceb685, then all ok. Rerun after the follow-up: all ok on both. On PG14 the
  heap-read test skips (no `pg_stat_force_next_flush`); its other conversion tests run.
- Same packages with `-race` on PG17: all ok, no data race reported (before and after the
  follow-up).
- e2e: `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok (163.8 s; 171.6 s after the
  follow-up).
- Lint: `golangci-lint run ./...` (v2.11.4), with `--build-tags=perfgate` on the gate packages
  and `--build-tags=convertscale` on partition: 0 issues.
- Perf gate: `go test -tags=perfgate -run '^TestPerfGate$' ./cmd/pg_sage_sidecar` with
  `PG_SAGE_PERF_SCALE=small`: FAIL with 14 offenders, all outside this branch's scope (above).

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/partition (new) | 83.4% |
| internal/querystore | 95.0% |
| internal/retention | 86.2% |
| internal/snapstore | 98.6% |
| internal/schema | 80.9% |
| internal/verify | 86.8% |
| internal/sre/slo | 89.0% |
| internal/sre/changefeed | 89.9% |
| internal/rca | 95.6% |
| internal/analyzer | 87.9% |
| internal/collector | 86.5% |
| internal/config | 90.9% |
| internal/store | 74.5% |
| internal/runway | 90.1% |
| internal/testsupport/perfgate (utility) | 92.2% |

### Skipped Tests (must be zero or justified)
None of the 21 skips is in logic this branch added. All are existing environment gates:
- Live cloud/provider tests (AWS RDS, Cloud SQL, Lakebase, AgentDB gauntlet x3, Azure x2):
  these need `PG_SAGE_LIVE_*` and cloud credentials.
- Live LLM (TestChatWithToolsLive_RealProvider, TestChatLive_RealProvider,
  TestTier2Live_RealGemini): these need `PG_SAGE_LIVE_LLM=1` and an API key.
- HA/restart containers (TestContainer_FailoverWhileDownOpensTheCooldown,
  TestContainer_RestartBetweenSamplesInvalidatesComparisons,
  TestContainer_PromotionBetweenSamplesInvalidatesComparisons): these need a disposable standby
  or server.
- PgBouncer x3: these need `SAGE_TEST_PGBOUNCER_*`.
- TestResolveLogDir_AbsoluteWindows: Windows-only, and the suite runs on Linux.
- TestRCAChildProcessFixture: a helper process that runs only under its parent test (which
  passed).
- TestGeneratePlanFixtures: fixture regeneration only. TestPGIncidentBench: CI runs it in its
  own step.
- On PG14 only, three new tests skip by design: TestFindingRefreshIsAHeapOnlyUpdate,
  TestPersist_UnchangedCausalChainWritesNoToast and TestConvert_ExclusiveWindowReadsNoHeap need
  `pg_stat_force_next_flush()` (PG15+). All three run on PG17 and PG18.

### Failures (if any)
None in the final runs. Earlier in the session, TestMetaReconcileAppliesPolicyColumnsInPlace
failed once in a full run with a connection timeout (infrastructure). It passed on rerun and in
the final run.

### Coverage Gaps (packages below threshold)
No touched package is below threshold (the lowest is internal/store at 74.5%). Packages below
70% that this branch does not touch: cmd/create_admin 51.5% and cmd/reset_admin_for_test 50.0%
(utilities, at the 50% floor), internal/testdb 61.0% (test helper), sre-bench 62.2%. All four
were already below 70% before this branch.

### Bugs Found This Session
1. [BUG] retention: `explain_results` was purged on `created_at`. Every cache refresh resets
   that column, so refreshed entries never expired. Now `expires_at` + 1 day.
2. [BUG] retention: `action_queue` was exempt ("executor owns lifecycle"), but nothing ever
   deleted from it. Now it has a rule with a keep predicate.
3. [BUG] retention: the 18 lazily created `agent_db_*` tables had no rule and no exemption. The
   guard test missed them because they did not exist when it ran.
4. [BUG] retention: `ctid IN (SELECT ... LIMIT n)` was planned as a hash semi-join over a full
   scan of the table being purged (EXPLAIN test). rca's incident prune and runway's sample prune
   (`id IN`) had the same shape. All three now use `= ANY (ARRAY(...))`.
5. [BUG] findings: 0% HOT. Every analyzer refresh rewrote every index because `last_seen` was in
   two indexes and in the retention index.
6. [BUG] sre_change_feed_state and incidents: unchanged state was rewritten every cycle, and
   incidents rewrote the TOASTed causal_chain each time.
7. [TEST BUG, fixed] retention pacing test (new): it counted one pause per batch of every
   relation, but on PG14 an empty default partition adds a zero-row batch with no pause. It now
   counts full batches only.
8. [TEST BUG, fixed] perfgate partition rollup test (new): it compared reltuples estimates
   exactly, and PG14's estimate differed by 2.5%. It now allows 10% (a double-counted parent
   would be off by 100%).
9. [BUG, follow-up] schema: the first conversion ran inside bootstrap, which startup bounds
   with a 10 s deadline (cmd metadb.go). On lifeos-sized tables, the heap scan under the lock
   would have run into that deadline and failed startup. Bootstrap now converts only heaps of
   32 MB or less and never fails on a conversion.
10. [BUG, follow-up] schema: the `stats_epoch` and `base_id` migrations ran `ALTER TABLE ...
   ADD COLUMN IF NOT EXISTS` on every start. That takes ACCESS EXCLUSIVE before it checks the
   column, so a long reader of query_store or snapshots stalled startup, and every writer
   queued behind the request. They now check the catalog first.
11. [BUG, follow-up, found by the full suite] partition: the first version of the safe
   conversion built the snapshots key with CREATE INDEX CONCURRENTLY inside bootstrap. CIC
   waits for every older snapshot, including a second instance waiting for the bootstrap
   advisory lock, so `TestMetaReconcileConcurrentPassesPublishOneRuntime` hit "deadlock
   detected". Fixed by building small keys without CONCURRENTLY and dropping with a plain
   DROP INDEX.

Existing tests changed because behaviour changed (not to make them pass):
- smoke CHECK-08 backdated only `last_seen`. Resolved findings now age from `resolved_at`, so it
  backdates that too.
- The snapstore keyframe chain test now pins its chain to one UTC day. The writer starts a new
  keyframe at each UTC midnight, so a chain crossing midnight gives a different result that is
  also correct.
- The rca preflight concurrency test relied on an unchanged incident being rewritten. It now
  changes the incident (re-Analyze) before the concurrent flush.
- Schema snapshot accessor test: 2 -> 3 `snapshot_data` overloads.

### Manual Checks Remaining
- CHECK-M1: MANUAL — upgrade of lifeos itself (query_store 852 MB, snapshots 9.3 GB). It is
  off limits, so I did not run it. Measured instead on 367 MB and 1.09 GB fixtures and
  extrapolated (see "Follow-up"). The exclusive window reads no heap block (asserted), so the
  remaining unknown is VALIDATE time on a cold cache, and writers do not wait for it.

## Post-test audit

1. Inputs not tested: sidecar and database clocks more than a day apart. Rows dated ahead
   when the table is converted stay in the history partition (the cut moves past them, up to a
   week; tested); rows dated ahead later land in the default partition and the next Ensure
   moves them (tested). A sidecar clock more than a day ahead while a conversion is preparing
   would have its inserts refused by the cutover CHECK until the conversion ends or is undone;
   that is not tested. A clock that is behind is not tested. Also untested: a deployment with
   extra user indexes on query_store/snapshots. Conversion recreates every index from its
   definition, and a non-PK unique index is refused (tested).
2. Behaviour not asserted: the exact pause length between batches (only a lower bound is
   asserted), and log message wording.
3. Assertions that would pass with a broken feature: none found. Every DB test asserts row
   counts, relation names, plan shapes (EXPLAIN), `n_tup_hot_upd`, `xmax` or TOAST size.
4. Test doubles hiding failure modes: the Recorder unit tests use a fake Execer, but the same
   behaviour is asserted against PostgreSQL (`TestRecorder_IdleAndMovingQueriesAgainstPostgres`
   and the collector's `TestRecordQueryStore_WritesOnlyMovedQueriesAcrossCycles`). Retention,
   partition, schema, rca and changefeed tests all run on real PostgreSQL.

Mutation testing: 24 mutants (M1-M25; M17 not applicable) on the new logic, each run against
its targeted tests. They covered: epoch change, keyframe boundary, evidence anchor, SLO lateral
anchor, snapshot day rule, no pacing, removable-always, cap dropping today, sameState, rca
unchanged skip, rca TOAST CASE, partition ORDER BY, `ctid IN` subquery, rca prune `IN`,
`last_seen` back in a findings index, no fillfactor, truncating an unexpired history, no run
budget, no default-row move, never forgetting queries, pings keep-newest, queue/decision keep,
explain grace, and dropping a partial day. All 24 are killed. Two survived the first pass (no
pacing, no in-rule deadline); the pacing and budget tests were tightened (0aa212d5), and both are
now killed.

## Branch

- Head: the commit adding this report (latest on `claude/perf-storage`). Pushed to
  `origin/claude/perf-storage` at the coordinator's request (follow-up); not merged, no PR.
- Tests were committed before each implementation.
- The struct-tag lines for the two new config keys exceed 100 columns, like every other field in
  `config.go`; Go struct tags cannot wrap.
