# Snapshot size cap: the history partition (2026-10-03)

Branch `claude/fix-snapshot-cap-history` (from `origin/master` at v1.8.3).

## The gap, and a correction to the brief

On lifeos, converting `sage.snapshots` to daily partitions put every existing row (9.3 GB on
disk, 6.85 GB of stored documents, 59,154 rows, mostly TOAST) into
`sage.snapshots_history`, bounded `FROM (MINVALUE) TO ('2026-10-05 00:00+00')`. Every
retention run since has logged
`sage.snapshots is 9332 MB, over its 938 MB cap, with nothing older than today left to remove`.

What v1.8.3 actually did (read from `retention/cap.go` before the change):

- Its cap loop stopped at the first partition whose upper bound is after today. The history
  partition sorts first, so **until the bound passed (about 2 days after the conversion) the
  cap could do nothing**, and it warned on every run (every ~10 minutes).
- **The brief's "about 90 days" is not right for lifeos.** At the first run after
  2026-10-05 00:00 UTC, v1.8.3 would have truncated the whole history partition (no row of a
  v1.8.3 day partition is built on a history row, since keyframes are day-aligned), and
  returned the 9.3 GB then. It stays ~90 days only when the cap is off or the table is under
  its cap by its files, or when a kept row is built on a history row.
- Other real problems found: the truncation removed all of the history partition even when
  the cap needed a little; it left an empty partition behind for good; the expired-history
  path (also `sage.query_store`) truncated instead of dropping; and **`removable` only
  checked direct references**, so dropping or truncating a partition could orphan a kept
  delta built on a checkpoint whose keyframe is in that partition (pre-v1.8.3 chains cross
  midnight). A test reproduces it on the old code: "2 kept rows lost their base".
- `sage.query_store` has no size cap path (`enforceSnapshotCap` runs for snapshots only). Its
  history partition drains by age (14 days) and is now dropped once empty or wholly expired.

## Design

Files: `sidecar/internal/retention/cap.go`, `cap_history.go` (new), `cap_notes.go` (new),
`partitions.go`, `purge.go`, `rules.go`, `cleanup.go`; `sidecar/internal/partition/ensure.go`,
`partition.go`.

1. **Open history partition (its bound is ahead; writers still put today's rows in it):
   trimmed oldest-first.** `trimHistory` picks a target boundary with one window query
   (rows oldest first until their stored size reaches the excess), caps it at today's UTC
   midnight, walks it back to a keyframe-safe boundary, then deletes rows before it with the
   existing paced machinery (`paced`, factored out of `purgeRows`): 50-row TID-array
   statements along the time index, 50 ms pause, the run's 30 s budget, resumed by the next
   run (the snapshots rule is first). A per-run byte budget of 1 GiB (`defaultTrimBudget`)
   ends the trim for the run without spending it, so other tables are still purged. Reason:
   deleting TOAST rows writes about as much WAL as it deletes (measured 247 MB of WAL for
   234 MB trimmed, after a checkpoint), so one 30 s run could otherwise write several GB.
2. **Keyframe integrity.** `safeBoundary` moves the boundary back until no row collected at or
   after it is built on a row before it (fixed point; at most 3 steps for the writer's
   two-deep chains, capped at 16). Rows before the oldest base a kept row needs go; that base
   and everything after it stay. The delete statement also carries the existing two-deep
   keep predicate (now `keepSnapshotBases(from)`, shared with the age purge) as a guard for
   rows written after the boundary was chosen (an older writer still running). `removable`
   now follows chains through a checkpoint (`on_p` = rows built on the partition, then rows
   at or after the bound built on those), using only base-id range scans.
3. **Drops.** `partition.DropHistory` drops the history partition under the existing 2 s lock
   timeout, re-checking under its locks (parent then partition, the order DROP takes them)
   that no row at or after a bound arrived. It replaces `partition.Truncate` (removed: no
   other user). The history partition is dropped:
   - for the cap, once it is closed (bound passed) and no kept row is built on it
     (`dropClosedHistory`): its rows are the oldest, and deleting them frees nothing;
   - by age, once closed and empty, or holding only rows past the window that nothing kept is
     built on (`dropDoneHistory`, replacing `truncateExpired`), for both tables.
   A lock timeout is logged and retried next run. Rows older than every day partition land in
   the default partition afterwards (tested: they age out like the others).
4. **Warnings.** `capNotes` logs a cap line when a table's state changes (`under`, `trimming`,
   `stuck`) and at most once per UTC day while it lasts; "under" only when coming back under.
   The trimming warning says what is happening and how much is left
   (`trimming the oldest rows of sage.snapshots_history in paced batches, N MB to go. Rows
   written until <bound> reuse the space; the rest returns to the operating system when the
   partition is dropped after that`); each run that trims logs an INFO with rows, MB and MB to
   go. The stuck warning names the knobs (`retention.snapshots_max_pct`,
   `retention.snapshots_days`).
5. **Space, and what the cap measures.** A DELETE frees no disk space; no VACUUM FULL is run.
   Autovacuum makes trimmed space reusable, but only by rows of the same partition. An open
   history partition still receives rows, so its freed space is reused; a closed one never
   receives another row, so its freed space is dead weight until the partition is dropped
   (vacuum can only truncate empty pages at the end of the file, and the newest TOAST chunks
   live there). Measured on the fixture: 245 MB on disk before and after trimming all 2,000
   rows. So `pg_total_relation_size` alone would make the cap delete forever. The cap counts:
   - daily partitions: their files (they lose no rows; they are dropped whole);
   - an **open** history partition: its live rows, estimated as
     `sum(pg_column_size(data)) + 200 bytes × rows`. `pg_column_size` reads a TOASTed value's
     stored (compressed) size from its pointer without fetching it, so this is one scan of the
     heap (lifeos: 50 MB heap, 26 ms). 200 bytes covers the heap tuple and four index entries;
     TOAST chunk headers (~2%) are left out. On lifeos the estimate is 6.86 GB against 9.3 GB of
     files: the rest is TOAST free space left by earlier purges;
   - a **closed** history partition: its files (only a drop returns them).
   The estimate is computed only when the files are over the cap.

Product calls (made per the rules' "AI DBA" lens: keep pg_sage's footprint small, never
destroy what a kept row needs):
- Drop a closed history partition whole for the cap, even if its live rows alone would fit:
  the cap protects disk, and only a drop returns it. On lifeos this loses the 10-02 tail,
  10-03 and 10-04 snapshot rows (~0.8 GB) to return 9.3 GB about two days after the upgrade.
- 1 GiB per run trim budget (WAL), on top of the time budget.
- **For the coordinator:** on lifeos the trim of the open partition is mostly wasted work:
  ~5.9 GB of deletes (~6 GB of WAL) to bring the live data under the cap for the ~34 hours
  until the partition closes and is dropped anyway (only ~70 MB/day of new rows land there to
  reuse the space). It is what the brief requires, and it matters on a database that writes
  GBs a day before the cut. If preferred, the trim could be skipped when the partition closes
  within a day; that is a one-line condition in `capHistory`.

## Lifeos: time to get under the cap at default pacing

Inputs (read-only queries on lifeos, 2026-10-03 ~15:00 UTC): database 18,793 MB, so the cap is
5% = 939 MB. History live estimate 6.86 GB (07-20..07-28: 319 MB, 09-05..09-12: 4,994 MB,
10-02: 832 MB, 10-03 so far: ~710 MB). Since ~06:00 UTC today (snapshot dedupe on) new
snapshots add 1-4 MB an hour. Retention runs about every 10 minutes (analyzer interval + 5 s;
lifeos logs show runs at 12:48, 12:58, 13:10).

- Excess ≈ 6.86 GB − 0.94 GB ≈ 5.9 GB, all from rows before today (the boundary lands in the
  afternoon of 2026-10-02; all pre-10-03 rows are keyframes, so no keyframe pull-back).
- At 1 GiB per run: **6 runs, about 1 hour after deploy** until the live data is under the cap
  and the warning stops. Fixture throughput (240 MB of ~120 KB rows, default 50 ms pause):
  ~100 MB/s including pauses, so 1 GiB takes ~10 s plus lifeos's uncached TOAST reads; it fits
  the 30 s run budget. WAL: ~6 GB over that hour.
- Disk: stays 9.3 GB until the partition closes at **2026-10-05 00:00 UTC**; the first run
  after that (about 00:10 UTC, ~34 hours from now) drops it and returns the 9.3 GB. If the
  drop times out on its 2 s lock (a long reader of `sage.snapshots`), it is retried each run.
- After that, the table is the daily partitions (~70 MB/day at the current rate), so the cap
  keeps about 13 days of snapshots until `snapshots_max_pct` or the database grows.

## Tests

New tests (DB-backed, real partition and retention code paths; PG17 `pgsage-ag2`):

| Requirement | Test |
|---|---|
| Over the cap, oldest first, measured by live data (2nd run deletes nothing although the disk size is unchanged) | `TestSnapshotCap_TrimsTheHistoryPartitionOldestFirst` |
| Stops at the keyframe boundary (pre-1.8.3 chain across midnight), kept rows readable | `TestSnapshotCap_TrimStopsAtTheKeyframeBoundary` |
| No kept delta orphaned: `sage.snapshot_data(data, base_id)` non-NULL for every kept row readable before | `assertStillReadable` in the boundary, guard, chain and writer tests |
| Statement guard keeps bases past an unsafe boundary | `TestTrimSQL_KeepsBasesPastAnUnsafeBoundary` |
| Resumes across runs (50 rows a run under a 1 ns budget, oldest first) | `TestSnapshotCap_HistoryTrimResumesAcrossRuns` |
| Per-run byte budget, run not spent, no false "stuck" | `TestSnapshotCap_TrimStopsAtItsByteBudgetPerRun` |
| Dropped when all of it must go / closed and counted by its files | `TestSnapshotCap_DropsTheHistoryPartitionWhenAllOfItMustGo`, `TestSnapshotCap_ClosedHistoryCountsItsFiles` |
| Dropped once empty (not while it covers today) or wholly expired (snapshots, query_store) | `TestRunOnce_EmptyHistoryIsDroppedOnceItCoversNoCurrentTime`, `TestRunOnce_ExpiredHistoryIsDropped`, `TestRunOnce_ExpiredQueryStoreHistoryIsDropped` |
| Not dropped while a kept chain reaches it through a checkpoint | `TestRunOnce_HistoryKeptWhileAKeptChainReachesIt` |
| Guarded drop, lock timeout (55P03, names the partition, partition kept) | `TestDropAndDropHistory`, `TestDropHistory_GivesUpBehindALongReader` |
| WARN rate-limited and actionable | `TestSnapshotCap_WarningIsRateLimited`, `TestSnapshotCap_TrimWarningSaysWhatIsLeft`, `TestCapNotes_LogOncePerStateChangeOrDay`, `TestCapNotes_ConcurrentUseLogsOnce` |
| Under the cap: untouched, nothing logged (also cap off) | `TestSnapshotCap_UnderTheCapIsUntouched` |
| Concurrent writer never fails, its rows stay readable | `TestSnapshotCap_ConcurrentWriterNeverFails` |
| Plan: time index + TID scan, no seq scan of the history partition | `TestTrimSQL_UsesTheTimeIndexAndTIDs` |
| Error path: chain deeper than the writer makes, nothing deleted, warned | `TestSnapshotCap_UnwalkableChainTrimsNothing` |

Fails before the fix: on the v1.8.3 code (with compile stubs for the new identifiers), 14 of
the first 15 new tests failed, plus the 2 changed existing ones (the passing one, "under the
cap is untouched", is a regression guard). The byte-budget and closed-history tests were
committed before their fixes and fail without them (mutation run below). Mutation testing (each change must fail a test; all did): no boundary
walk, no keep guard, cap measured by files, no rate limit, one-deep `removable`, no closed
history drop, trimming today's rows, `DropHistory` without its guard, dropping a history
partition that covers today, no byte budget, closed history measured by live rows.

Test changes to existing tests (spec changes, explained in the commits): the history
partition is dropped, not truncated (`TestRunOnce_ExpiredHistoryIsTruncated` became
`...IsDropped`; the cap test expects it among the dropped; the query_store days test expects
the empty history partition dropped); `TestDropAndTruncate` became `TestDropAndDropHistory`
(`Truncate` is gone); `rebound` recreates a dropped history partition. One test logic error
was fixed: the oldest-first test's cap left out the empty partitions' index pages.

## Test Results

**Command:** `go test -cover -count=1 -p 2 ./...` (PG17, `--cpus=2`)
**Total:** 89 packages ok, 0 failed (first full run, exit 0). A second full run with `-json`
(to enumerate skips) passed 11,648 tests; 2 tests in `internal/analyzer` and
`cmd/pg_sage_sidecar` failed and 2 skipped on `dial error: timeout` to the test server while
mutation runs shared it; re-run alone, all three packages pass (analyzer 91.1%, api 78.7%,
cmd 81.4%).
**Coverage (touched packages):** `internal/retention` 86.6%, `internal/partition` 83.9%
(PG14: 86.1% / 83.9%, PG18: 86.1% / 83.9%, all pass). `-race`: both pass.
**E2E:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok (165 s), 20 passed, 13 skipped
(`SAGE_LLM_API_KEY not set`: live LLM tests).
**Perf gate:** `PG_SAGE_PERF_SCALE=small ... TestPerfGate`: PASS (162 s).
**Lint:** golangci-lint 2.11.4: 0 issues.

### Skipped Tests (full suite, all environmental, none in touched packages)
- agentdb (6), azure (2), llm (2), rca tier2 (1): live cloud/LLM tests, opt-in env vars.
- causal (2), ha (1), pooler (3): need a disposable standby / restartable server / PgBouncer.
- rca preflight helper (1), sre-bench (1), tuner fixture regen (1): helper or opt-in runs.
- logwatch (1): Windows path test, not applicable on Linux.
- analyzer (1), api (1): only in the `-json` run, DB dial timeout (see above); passed alone.

### Failures
- None attributable to this change (see the two dial timeouts above, passed on re-run).

### Coverage Gaps
- All touched packages meet thresholds (retention 86.6%, partition 83.9%; business floor 70%).

### Bugs Found This Session
1. [BUG] `retention/cap.go` (v1.8.3): the cap stopped at the history partition while it
   covered today: no action and a WARN every run for ~2 days after the conversion.
2. [BUG] `retention/partitions.go` `removable`: checked only direct references; dropping or
   truncating a partition could orphan a kept delta built on a checkpoint whose keyframe was
   in it (reproduced: "2 kept rows lost their base").
3. [BUG] The cap truncated the whole history partition when it needed only part, and left an
   empty partition; the expired path truncated instead of dropping.
4. [BUG] Test helper `rebound` (introduced and fixed on this branch): a lookup inside an open
   transaction on a one-connection pool deadlocked the package.

### Post-test audit
- Inputs not covered: retention across UTC midnight during a run; a lock timeout on the
  cap's own `DropHistory` (covered at the partition level, not through `RunOnce`); measure
  query errors (logged and skipped, same pattern as the other cap queries).
- Assertions that would pass if broken: none found; every behavior test checks exact rows,
  stats, relations or log lines, and the mutation run killed every mutant.
- Fakes: none. Every test runs the real SQL against PostgreSQL 14, 17 and 18.

## What is left
- Coordinator decision: keep or skip trimming an open history partition that closes within
  a day (see Design, product calls).
- `query_store` has no size cap path; its history partition drains by age only.
- The rule rotation can starve `sage.snapshots` if `sage.query_store`'s purge spends the
  whole budget run after run (seen on lifeos before this change); not changed here.
