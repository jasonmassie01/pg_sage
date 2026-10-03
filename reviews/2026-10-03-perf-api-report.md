# perf-api report (v1.8.3 performance fix phase)

Branch `claude/perf-api` (from `claude/perf-base`). Scope: live-update events, list endpoints and
snapshot history, pg_sage self-visibility and self-cost. Brief:
`reviews/2026-10-03-perf/FIX-BRIEF.md`.

## What was built

### 1. Live-update events (`internal/api/events.go`, `events_poll.go`)

- The poller no longer runs `max(greatest(...)), count(*)` over `sage.findings`,
  `action_log UNION ALL action_queue` and `health_history` every 2 s.
- Each tick (only while at least one SSE subscriber is connected; zero queries otherwise) reads
  one change signature per watched table: the table's write counter
  (`pg_stat_get_tuples_inserted + updated + deleted`, O(1), no table access) plus its newest id
  (`ORDER BY id DESC LIMIT 1`: one primary-key index probe). An event is published when either
  part moves. The newest id catches inserts before the statistics flush (stats lag ≤ 1 s, up to
  ~10 s on PG15+ when idle); the counter catches updates and deletes (resolutions, verdicts).
- Per database: each fleet instance keeps its own last-seen marks; the first read is a baseline
  (no event). A table that does not exist is skipped (`to_regclass`).

### 2. List endpoints (`internal/api/keyset.go`, `findings_list.go`, `actions_list.go`,
`actions_ledger.go`, `snapshot_history.go`)

- Findings and actions lists page with an opaque `cursor` (base64url JSON of sort keys, source
  and id; `next_cursor` in each response; max 2 KB). The row comparison
  `(k1, ..., id) < (...)` uses one direction per sort, with `id` as tie-breaker, so pages never
  overlap or skip (the old ORDER BY had no tie-breaker). Fleet lists merge per-database pages in
  Go by (sort keys, database, id).
- `offset` still works for old clients but is limited to 1,000; `limit` to 200 (default 50).
- `total` is a capped count (`SELECT count(*) FROM (SELECT 1 ... LIMIT 1001)`), reported as
  1,000 with `total_capped: true` beyond that.
- Actions: attempts (`COUNT(*) OVER (PARTITION BY sql_executed)` over all of action_log) are now
  counted per distinct statement on the page, in a MATERIALIZED CTE, through
  `md5(sql_executed)` + `executed_at` and capped at 1,000 (`attempts_capped`). The same helper is
  used by the cases handler.
- Findings sorts: `severity` (the API default) pages over the new rank index, newest first by id
  within a severity; `last_seen` (the dashboard's five most recent) stays exact as a top-N sort of
  the rows matching the filter (one status); `created_at`, `category`, `title` and `impact` sort
  the matching rows as before. No findings index keys `last_seen` (see decision 7).
- New indexes, one idempotent migration (`internal/schema/api_list_index_migration.go`, reusing
  the decision-ledger catalog-check loop: create only if absent, rebuild if INVALID):
  `idx_findings_list_rank (status, severity rank, id)`,
  `idx_action_log_sql_md5 (md5(sql_executed), executed_at)`,
  `idx_action_queue_ledger (proposed_at, id) WHERE status <> 'executed' AND proposed_at IS NOT
  NULL`. The executed-actions page uses the existing `idx_action_log_time` with an incremental
  sort on id (a dedicated `(executed_at, id)` index was dropped as redundant).
- `/snapshots/history`: per-object categories (`tables`, `indexes`, `queries`, `sequences`,
  `foreign_keys`, `locks`, `partitions`, `config_data`) are refused with 400 and a pointer to
  `/snapshots/latest`; the response stops at 4 MiB with `truncated: true`. Points stay as raw
  JSON (no decode into Go maps).

### 3. Self-visibility (`internal/selfmonitor`, `internal/selfcost`, analyzer, cmd)

- `silenceSelfStats` (`SET pg_stat_statements.track = 'none'`) is gone, as is the perf gate's
  workaround for it.
- `selfmonitor.ConfigurePool` is applied to every pg_sage pool (meta DB, monitored DBs, fleet):
  `application_name = pg_sage` and a protocol-level tagger (`pgconn.Config.AfterNetConnect`
  wrapping the post-TLS `net.Conn`) that rewrites every Query and Parse message so the statement
  carries `/* pg_sage */` right after its first keyword (`SELECT /* pg_sage */ ...`). An existing
  leading `/* pg_sage ... */` comment (with its component label) is moved there rather than
  duplicated. This covers every code path, including pgx's own and batch/extended protocol.
- Exclusions added where pg_sage read activity or statements without filtering: connection-leak
  rule, clone/schema-family session detection, schema-guard related-statement lookup, optimizer
  query grouping and plan capture. Tuner candidates and the collector's top-query snapshot (which
  feeds optimizer, analyzer slow-query rules, query store and the SLO latency proxy) already
  filtered by text; new tests now prove each with statements sent through a real pg_sage pool.
- Self-cost: `selfcost.Read` samples pg_sage's pg_stat_statements entries (per entry, so an
  evicted or reset entry does not make the window negative), sage-table rows read/written and
  sage schema bytes once per analyzer cycle; costs are normalized to one collector cycle.
  Metrics: `pg_sage_self_db_time_ms_per_cycle`, `_statements_per_cycle`, `_blocks_per_cycle`,
  `_rows_read_per_cycle`, `_rows_written_per_cycle`, `_schema_bytes`,
  `pg_sage_self_db_time_budget_ms`. Finding `sage_self_cost` (warning) when DB time per cycle
  exceeds `analyzer.self_cost_budget_ms` (default 3000, 0 disables; hot-reloadable).

## Product decisions (made here)

1. **Tag after the first keyword, not as a prefix.** PostgreSQL 18's pg_stat_statements stores
   the text from the first token, dropping leading comments (verified on matrix-18). A prefix tag
   would make pg_sage invisible to its own exclusions on PG18. All readers match the tag
   anywhere.
2. **Tag at the wire, not in source.** Hundreds of statements lacked the tag; a pgx connection
   wrapper is the only way to make it universal and keep it so.
3. **Exclusion by text and application_name.** Statement readers exclude
   `query ILIKE '%pg_sage%'` or the self regex; activity readers exclude
   `application_name ILIKE '%pg_sage%'`. The SLO connection-saturation proxy still counts
   pg_sage's backends: they occupy real slots.
4. **Self-cost budget 3000 ms per collector cycle** (5% of one core at a 60 s cycle), same as the
   perf gate budget B. Unknown windows (first cycle, stats reset, no pgss) never raise or resolve.
5. **Capped totals at 1,000, offset ≤ 1,000, limit ≤ 200.** The UI shows "1000" (with
   `total_capped`) instead of an exact count; cursors are the way past 1,000 rows.
6. **Snapshot history refuses per-object categories** instead of aggregating them: the
   per-object data belongs to `/snapshots/latest`; history is for small categories.
7. **No findings index on `last_seen` (coordinator note from perf-storage).** The analyzer
   rewrites `last_seen` (and the counters and text) on every refresh of every open finding;
   perf-storage made those refreshes heap-only and tests it. The first version of this branch
   had `(status, rank, last_seen, id)` and `(status, last_seen, id)`; both are replaced by
   `(status, rank, id)`. Consequences: within a severity the list is newest-first by id (for open
   findings last_seen is the last cycle for all of them, so little changes), and `sort=last_seen`
   is a top-N sort of one status's rows instead of an index walk. A test pins that no findings
   index this migration creates keys a refreshed column.
8. **Live updates may lag up to the stats flush (≤ ~10 s on PG15+) for updates/deletes.** Inserts
   are immediate through the newest-id probe. NOTIFY was rejected: it needs triggers or writer
   changes on every write path and a dedicated listening connection per database.

## Before / after

### Perf gate, small scale (500 tables, 1500 indexes, 500 sequences, 20k rows per history
table; warmup 45 s, steady 90 s, dashboard subscriber connected)

`before` = `claude/perf-base`; `after` = final HEAD (gate-final). Raw reports are in the agent's
scratchpad (`perfapi/gate-before.md`, `gate-final.md`, `gate-nosub.md`).

| measure (steady 90 s) | before | after |
|---|---|---|
| sage.findings seq scans / rows read | 48 / 960,048 | 2 / 40,002 |
| sage.action_log seq scans / rows read | 55 / 1,100,000 | 8 / 141,001 |
| sage.health_history seq scans / rows read | 46 / 920,182 | 0 / 0 |
| rows read sequentially, these three tables | 2,980,230 | 181,003 (-94 %) |
| events poll statements | 3 statements x 45 calls, 165 ms, all seq scans | not in the top 15; index/stats only |
| gate offenders | 15 | 13 (none from this branch's scope, see below) |
| /api/v1/findings | 19.4 ms | 6.2 ms |
| /api/v1/actions | 17.6 ms | 11.2 ms |
| /api/v1/cases | 12.8 ms | 11.1 ms |

With and without a subscriber (`PG_SAGE_PERF_LIVE_SUBSCRIBER=0`, gate-nosub): the same 13
offenders and the same seq-scan counts on findings, action_log and health_history, so none of
the remaining scans is the event poller; with no subscriber the poller issues no statement
(`action_queue` seq scans drop from 52 to 6, all on an empty table). The remaining offenders on
those tables belong to other branches: the `/value` toil aggregate and recommendation reconcile
(action_log), the open-findings `DISTINCT object_identifier` lookups (findings), decision /
verification / incident / runway retention purges, and the analyzer's snapshots `WITH ranked`
query (statement mean time).

### Micro-benchmark (150,000 rows per table, PG17, `EXPLAIN (ANALYZE, BUFFERS)`, final HEAD)

| statement | before | after |
|---|---|---|
| events poll (findings / actions / health) | 20.1 / 23.2 / 7.5 ms; 3,354 / 3,593 / 1,402 buffers | all three: 4.3 ms, 15 buffers |
| findings count (resolved) | 240 ms, 3,354 buffers | capped count 13.7 ms, 23 buffers |
| findings page offset 0 / 1,000 / 100,000 | 267 / 289 / 340 ms (temp spill at 100k) | first page 3.7 ms, 10 buffers; page after 100k rows (cursor) 5.9 ms, 10 buffers |
| findings `sort=last_seen`, limit 5 (no index by design) | n/a | open (3,000 rows): 18.9 ms, 3,004 buffers; resolved: 1.5 ms, 4 buffers (an index walk; the base still has a resolved last_seen index, which perf-storage drops) |
| actions count | 9.4 ms | capped 1.7 ms |
| actions page offset 0 / 1,000 | 307 / 358 ms, temp spill 3,031 blocks | first page 4.4 ms; queued ledger page 1.5 ms |
| listActions / listFindings end to end | count + page above | 10.0 / 10.7 ms, total 1000 capped |

### Snapshot history

Before: `metric=indexes|tables|sequences` accepted; up to 500 full documents decoded into Go
maps (static audit: 24 h of indexes on lifeos = 412 rows, 666 MB stored, ~6.5 GB of text: one GET
could OOM the sidecar). After: 400 for per-object categories; any response ≤ 4 MiB of point data
(`truncated: true`), proven by `snapshot_history_cap_test.go` with oversized fixture rows.

## Tests (written first; test commits 6d1b23a6, 403569e9, d4134b9f, 25938512, a39fc78d,
4194edf8; each committed before its implementation commit)

Each failed before its fix, except the tuner and collector exclusion tests, which pin
behavior those packages already had now that pg_sage is tracked:

- `events_change_test.go`: the change-signature plan touches no table (no Seq Scan, PK index
  only), writes move the signature, apply logic (baseline, missing table, publish on change),
  update without scan end to end. "No query without subscribers" was already pinned on the base
  branch by `events_watch_test.go` (perf-gate commit 771486b1) and still passes.
- `findings_keyset_test.go`, `actions_keyset_test.go`, `keyset_test.go`: every row once with
  tied keys, both directions, severity order, fleet walk, offset bounds, cursor validation,
  capped totals, EXPLAIN asserts index use and no Sort/WindowAgg/Seq Scan, attempts tally runs
  once with ≤ 1,001 rows read (ANALYZE).
- `api_list_index_migration_test.go`: no findings index keys a column the refresh rewrites;
  indexes exist and a rerun keeps them (same oid), a dropped index is rebuilt, no index
  duplicates another migration's definition.
- `snapshot_history_cap_test.go`: categories refused, 4 MiB cap.
- `selfmonitor` (`tagconn_test.go`, `pool_test.go`, `pool_integration_test.go`): every protocol
  path tagged and tracked by pgss, tag after the first keyword (PG18), session not
  `track = none`, split writes, byte-identical non-query messages, error propagation.
- Self-exclusion: analyzer (leaks, clones), autonomy (schema statements, sessions), optimizer,
  tuner, collector; each sends a slow/indexable statement through a pg_sage pool and asserts
  pg_sage never recommends an index or hint for it while the same application statement is
  still analyzed.
- `selfcost` unit + integration (eviction, reset, unknown windows), `rules_self_cost_test.go`,
  `self_cost_metrics_test.go`, config/store/api config consistency.

## Test Results

**Command:** `go test -p 2 -count=1 -cover -v ./...` (PG17, pgsage-ag2, repo root mounted in
`golang:1.25`, `--cpus=2`) at cc5c91ec
**Total:** 11,373 passed, 2 failed, 21 skipped (84 packages ok, 2 packages FAIL). Both failures
are outside this branch's packages and pass in isolation. They are load timeouts and a
concurrency assertion on a loaded host (three other agents were running suites at the same time):
- `cmd/pg_sage_sidecar` `TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident`:
  `bootstrap: acquiring advisory-lock connection: context deadline exceeded` (30 s). Passes 3/3
  in isolation.
- `internal/executor` `TestConcurrentWithheldAdmissionsRecordOneRow`: `rows=1 occurrences=7,
  want 1/8`. Passes 15/15 on this branch and 15/15 on `claude/perf-base` in isolation. This
  branch does not touch the executor. See coordinator items (a swallowed upsert error is the
  likely cause).

Earlier full PG17 run at c64ef9cf: 85 packages ok, 1 FAIL
(`TestFleetReloadConcurrentRetriesConverge`, `db "b": ping: context deadline exceeded`, the 5 s
fleet ping under load; passes 3/3 alone).

**Other runs (all `-count=1`):**
- PG14 (:55414), touched packages (selfmonitor, selfcost, api, analyzer, autonomy, optimizer,
  tuner, collector, config, store, schema, cmd/pg_sage_sidecar): all ok. api, schema and
  analyzer were rerun at final HEAD: ok.
- PG18 (:55418), same packages: all ok except `TestCheckSelfCost_TwoCyclesAgainstPostgres`, a
  test bug (below) fixed in 9c398b1d. api, schema and analyzer were rerun at final HEAD: ok.
- `-race`, same packages, PG17: all ok except `TestFleetReloadAddRemoveCyclesLeakNothing`
  (`closing instance pool: context deadline exceeded`). The fleet reload tests then passed
  `-race -count=2` alone.
- e2e (`go test -tags=e2e -count=1 -timeout 900s ./e2e/`): ok, 20 passed, 13 skipped (live LLM
  tests: no `SAGE_LLM_API_KEY`, and `PG_SAGE_LIVE_LLM` stays unset by rule).
- Perf gate, small scale: see Before / after (FAIL on 13 offenders, none in this scope).
- golangci-lint: 0 issues. gofmt clean (CR-stripped). Function, file and line limits checked.
- Merge test against `origin/claude/perf-storage` (43d0c1b4) in a scratch worktree: only
  CHANGELOG.md conflicts textually. `go vet -tags=perfgate ./...` is clean. store, schema,
  analyzer (including perf-storage's `TestFindingRefreshIsAHeapOnlyUpdate`), api, config,
  selfcost and selfmonitor all pass on PG17. The scratch worktree was removed.

**Coverage (touched packages, PG17 final run; cmd from the PG14 run):**

| package | coverage |
|---|---|
| internal/selfcost | 95.9 % |
| internal/config | 90.9 % |
| internal/selfmonitor | 89.7 % |
| internal/analyzer | 88.2 % |
| internal/collector | 87.1 % |
| internal/tuner | 85.0 % |
| internal/optimizer | 83.9 % |
| internal/autonomy | 83.6 % |
| internal/schema | 82.8 % |
| cmd/pg_sage_sidecar | 81.5 % |
| internal/api | 78.7 % |
| internal/store | 74.5 % |

### Skipped Tests (must be zero or justified)
None in a touched package. All 21 suite-wide skips need an external resource or an opt-in flag:
- live provisioning: AWS RDS, Cloud SQL, Lakebase, Azure (5), plus the gauntlet chains (3)
- live LLM (3, `PG_SAGE_LIVE_LLM`)
- HA standby, restartable server or causal standby containers (3)
- PgBouncer (3)
- one each: a Windows-only path test on Linux, the RCA child-process helper, plan fixture
  regeneration, and the full incident bench (CI runs it separately)

In e2e: 13 live-LLM skips.

### Failures
Only the load flakes above. None reproduces in isolation.

### Coverage Gaps
All packages meet coverage thresholds. The lowest are store at 74.5 % and api at 78.7 %, both
above 70 %.

### Bugs Found This Session
1. [BUG] The connection-leak rule in `analyzer/analyzer_checks.go` scanned an `interval` into a
   `*string`, so it never fired. Fixed with `::text`.
2. [BUG] PostgreSQL 18's pg_stat_statements drops comments before the first token, so every
   leading `/* pg_sage */` tag in the code base was invisible on PG18. The tag now goes after
   the first keyword (b65f9b83).
3. [BUG] The self-cost window went negative (unknown) whenever pg_stat_statements evicted one of
   pg_sage's entries. Deltas are now taken per entry (1505b661).
4. [BUG] The first keyset version of the actions page took 1,008 ms in the gate: it counted
   attempts per row over 20k identical statements (99 seq scans). It now runs one capped tally
   per distinct statement.
5. [BUG] On PG14 the events poll planned `max(id)` as an Aggregate. It now uses
   `ORDER BY id DESC LIMIT 1`.
6. [BUG] (coordinator) The first findings list indexes keyed `last_seen` and would have made
   every findings refresh non-HOT. Replaced in cc5c91ec.
7. [TEST BUG] The probe in `TestCheckSelfCost_TwoCyclesAgainstPostgres` shared a
   pg_stat_statements entry with `preflight_evidence_test`'s untagged `SELECT pg_sleep(0.02)`,
   because pgss ignores aliases and constants. On PG18 the measured time was therefore 0. The
   probe now has its own shape, and the window is retried in case another package calls
   `pg_stat_statements_reset()` meanwhile.
8. [TEST BUG] The existing `TestEventBrokerPollOncePublishesActionQueueChanges` needed the
   newest-id probe because of statistics flush lag. That probe became part of the design.

### Manual Checks Remaining
- CHECK-M1: MANUAL — check the Findings page and dashboard against a live sidecar:
  `total_capped` shows "1000", and the dashboard's "recent findings" still uses
  `sort=last_seen`. The web dist was not rebuilt here (`node_modules` missing);
  `config_meta.json` was regenerated.

## Post-test audit

- **Mutation testing.**
  - Rounds 1-2 (api, analyzer, autonomy, optimizer, selfmonitor, selfcost): 18/18 compile-valid
    mutants killed after the capTotal refactor.
  - Round 3, on the late changes: 9/9 killed. The mutants were:
    - per-entry delta without reset handling
    - delta computed from sums
    - selfcost matching the tag only at the start
    - tag as a prefix
    - leading tag not moved
    - `IsTagged` anchored to the start
    - duplicate tag after the keyword
    - severity sort with a `last_seen` key
    - rank index keyed on `last_seen`
- **Inputs not tested:**
  - A statement whose first token is a quoted identifier or `$$`: the tag goes in front, where
    PG18 strips it. Such statements are rare and pg_sage sends none at top level.
  - Cursors from another sort order: rejected with 400 (that case is tested).
  - Multi-statement simple queries: the tag is placed once, after the first keyword. pgss
    stores each statement separately, so only the first one is tagged. pg_sage sends
    multi-statement strings only in schema bootstrap.
- **Assertions that could pass with the feature broken:** none. The EXPLAIN tests assert
  specific index names and the absence of Sort, Seq Scan and WindowAgg, plus ANALYZE row and loop
  bounds. The self-exclusion tests assert both that pg_sage's statement is absent and that the
  same application statement is present.
- **Test doubles hiding failures:** none for SQL. Every exclusion and plan test runs against
  PostgreSQL 14, 17 and 18. The events apply test uses a fake signature, but the integration test
  drives real writes through the poller.

## What is left / coordinator items

1. **Collector system stats** count pg_sage's own backends in `active` and
   `idle in transaction`. Collector catalog reads were off limits for this branch.
2. **SRE probe:** the pg_stat_statements temp-spill read does not filter out pg_sage. SRE probes
   were off limits.
3. **Nested statements** inside pg_sage's DO blocks and functions keep their own untagged text in
   pg_stat_statements when `track = all`. The gate shows one: the `sre_family_autonomy` migration
   UPDATE. Their time is counted inside the tagged top-level entry, so self-cost is right, but an
   advisor could still see the inner statement text. Only schema migrations do this.
4. **Same-role identical statements share one pgss entry**, because aliases and constants are
   ignored. If pg_sage and an application run the same statement shape as the same role, the
   entry's text is whichever ran first. This is documented; the recommendation is a dedicated
   pg_sage role.
5. `api/database_helpers.go` opens one untagged one-off connection. PgBouncer admin connections
   are untagged on purpose: the admin console rejects comments.
6. `/value` aggregates all successful actions in Go, which leaves action_log seq scans in the
   gate.
7. Live update events for updates and deletes can lag the statistics flush (up to ~10 s when
   idle on PG15+).
8. `sort=last_seen` is a top-N sort of the matching rows: ~19 ms for 3,000 open findings. With
   `status=resolved`, after perf-storage drops the resolved `last_seen` index, it becomes a scan
   of all resolved findings. The UI never requests that combination.
9. **Integration:**
   - CHANGELOG.md conflicts with perf-storage; keep both bullets.
   - The store and api config key-count tripwires (116) may need the sum of every branch's new
     keys.
   - This branch's test DBs may still carry the superseded `idx_findings_list_severity` and
     `idx_findings_list_last_seen` indexes. They were never released, so there is no drop
     migration.
10. `internal/executor` `recordWithheldAdmission` swallows its upsert error (it logs and returns
    true). Under load, one of 8 concurrent withholds was not counted. Worth a look by the
    executor owner.
11. The `reviews/2026-10-03-perf/` directory (the coordinator's brief) is left untracked.
