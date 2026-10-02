# Snapshot dedupe: compact catalog history in `sage.snapshots`

Date: 2026-10-02. Branch `claude/snapshot-dedupe`, based on `claude/dogfood-lifeos-1` @ c612e9a.

Problem (dogfood lifeos, read-only evidence from the coordinator): `sage.snapshots` is
9.3 GB on a personal database. The `indexes` category is 3.3 GB, `tables` 1 GB and
`sequences` about 0.5 GB. The collector writes the whole catalog list as one jsonb row per
category every cycle (46,552 rows since July). Most of each row is static definitions
(`indexdef`, `index_type`, flags, schema and relation names). Only the counters move.

## 1. Inventory: every reader of `sage.snapshots` (written first)

Method: `grep -rn "sage.snapshots"` and `grep -rn "snapshots"` over `sidecar/` (Go, SQL,
web), `cloudsqltests/`, `e2e/`, `scripts/` and `docs/`, then reading each hit. Readers of
the in-memory `collector.Snapshot` (`LatestSnapshot` / `PreviousSnapshot`) are listed
separately: they never read the table.

### 1a. Production readers of the table

| # | Reader | Categories | Fields | Time resolution |
|---|--------|------------|--------|-----------------|
| R1 | `api.querySnapshotLatest` (`GET /api/v1/snapshots/latest`, web Database page, e2e walkthrough) | any (default `system`; walkthrough reads `queries` and every metric) | the whole jsonb document, unchanged shape | newest row of the category |
| R2 | `api.querySnapshotHistory` (`GET /api/v1/snapshots/history`), the only history export | any category in `validateMetric` (`tables`, `indexes`, `queries`, `sequences`, `foreign_keys`, `system`, `io`, `locks`, `config_data`, `partitions`) | the whole document per point | every row in the window, newest 500, re-sorted ascending |
| R3 | `forecaster.QueryDailySystemAggs` | `system` | `db_size_bytes`, `active_backends`, `total_backends`, `max_connections`, `cache_hit_ratio`, `total_checkpoints` | every row in the lookback, grouped by day |
| R4 | `forecaster.QueryDailyQueryAggs` | `queries` | per element `queryid`, `calls` | first and last non-empty row of each day |
| R5 | `forecaster.QueryDailySeqAggs` | `sequences` | per element `schemaname`, `sequencename`, `pct_used`, `max_value` | last non-empty row of each day |
| R6 | `analyzer.buildHistoricalAverages` (query_regression rule) | `queries` | per element `queryid`, `mean_exec_time` | all rows in `analyzer.regression_lookback_days`, downsampled to 100 evenly spaced rows |
| R7 | `verify.PostgresObservationSource.WriteMeasurements` | `system` | `blk_write_time` | first and last row between two instants, and the row count |
| R8 | `optimizer.CheckColdStart` | all | none (`COUNT(*)` against `llm.optimizer.min_snapshots`) | row count |
| R9 | `retention.Cleaner` (`purgeRules`) | all | `collected_at` | deletes rows older than `retention.snapshots_days` |

### 1b. Readers that do not touch the table (checked, no change needed)

| Consumer | What it actually reads |
|----------|------------------------|
| Unused-index rule (`analyzer.ruleUnusedIndexes`) | The in-memory current snapshot's `idx_scan`, plus `extras.FirstSeen` (in memory) and the live `pg_stat_database` stats epoch (`loadStatsEpoch`). It does not read history. |
| Stats-epoch logic (`collector.markStatsReset`, `collectStatementsEpoch`) | The in-memory previous snapshot and `pg_stat_statements_info`. `StatsEpoch`/`StatsReset` are never persisted to `sage.snapshots`. The query store keeps its own epoch in `sage.query_store`. |
| Rollback regression checks (`executor/rollback_eval.go`) | `sage.action_log.before_state` / `after_state` and live `pg_stat_database` / `pg_stat_statements`. |
| RCA (`internal/rca`) | The in-memory snapshots handed over by the analyzer cycle. |
| Runways (`internal/runway`, `sre/probes/catalog_runway.go`) | `sage.runway_samples` and live catalog probes. |
| MCP (`internal/mcp`) | `sage.findings`, `sage.action_log`, the SRE tables. No snapshot reads. |
| Analyzer snapshot rules, advisor, tuner, optimizer prompts | The in-memory `collector.Snapshot`. |
| `cloudsqltests/*.go` (manual scripts) | `count(*)` by category, and a `data::text LIKE` scan for sage objects. Row counts are unchanged, and every catalog object still appears in full in some row. |
| `seed_snapshots.sql`, `tests/integration/seed_snapshots.sql`, tests | Insert legacy full rows. These stay valid: a legacy row is a full row. |

### 1c. What each consumer needs from storage

- R1, R2 need the exact document the collector wrote for each stored instant, element
  order included (the history API is the export path).
- R4, R5, R6 need exact per-element values at a few chosen instants per day or window.
  They also need an "is this sample empty" test that does not read the payload.
- R3, R7 read `system` only. That category is a single small object and is not a catalog
  list, so it keeps its current format.
- R8 needs one row per category and cycle. R9 needs `collected_at` on every row.
- **Unused-index proof over N days.** `idx_scan` must be recoverable for every stored
  sample in the window, so a counter reset (a decrease) or a drop and recreate (an object
  that disappears and comes back with a different definition) stays visible. Today the
  rule uses in-memory state only. The storage must still keep the evidence an operator or
  a future history-based proof would use.

## 2. Design

### Storage format

`sage.snapshots` keeps one row per category and cycle (R8 and R9 are unchanged). A row
is either a **full row** (`base_id IS NULL`: a legacy row or a keyframe, the document as
the collector wrote it) or a **delta row** (`base_id` names the row it is built on, and
`data` holds only what changed).

- **Delta categories.** These are the catalog lists `tables`, `indexes`, `sequences`,
  `foreign_keys`, `partitions`, `queries` and `io`. `config_data` is an object of mostly
  static members and is encoded as a list of one. `system`, `locks` and `replication`
  stay full: they are small, everything in them moves, and R3/R7 read `system` raw over
  long windows.
- **Delta content** (`internal/snapstore/encode.go`). Elements are paired with their base
  element by identity (for example schema plus index name; the identity is the raw JSON
  of those fields). A delta carries:
  - `n`, the element count;
  - `i`, integer fields that moved, as increments;
  - `g`, a global increment for a counter that moved alike on most elements (every
    table's `xid_age`);
  - `u`, other changed fields as values;
  - `a`, added elements in full;
  - `d`, removed base indices;
  - `o`, an explicit order, only when the order is not "base order then added".
  Patches are addressed by base index. A field that disappears (a patch cannot delete)
  and any document that is not a list of uniquely keyed objects are stored in full.
  Pairing only decides what a delta carries. Every differing field is carried, so any
  pairing decodes to the exact document.
- **Bases at most two deep** (`writer.go`):
  - A keyframe is a full row. It serves for at most 6 h and is replaced when the delta on
    it would be over half its size.
  - A checkpoint is a delta on the keyframe.
  - Each cycle is a delta on the latest checkpoint while that is at most a quarter of the
    delta on the keyframe. Otherwise the cycle becomes the new checkpoint.
  - Bases live in the Writer's memory, so a restart starts with a keyframe. A base newer
    than the document (a clock step) is never used. A failed transaction does not advance
    the bases.

### One accessor

`sage.snapshot_data(data, base_id)` is a plpgsql function from the new migration. It
walks to the full row and applies the deltas oldest first through
`sage.snapshot_apply(base, delta)`, and returns the document byte for byte as collected.
It returns NULL when a base is missing or the chain is deeper than 8 (manual edits);
it never guesses. Go code uses `snapstore.DataSQL(alias)` and `snapstore.NonEmptySQL(alias)`.
The second one tests emptiness without detoasting a full row: a delta carries `n`.

Consumers changed:
- `api.querySnapshotLatest` and `querySnapshotHistory` decode only the capped rows.
- `forecaster` query and sequence aggregates decode only each day's first and last
  samples.
- `analyzer.buildHistoricalAverages` (`query_history.go`) lists the lookback's ids,
  downsamples to 100 and decodes only those. The old code loaded every row of the
  lookback.

R3, R7, R8 and the in-memory consumers did not need to change.

### Migration

`internal/schema/snapshot_delta_migration.go` is registered last in `bootstrap.go`, with
no `schema_version` bump. It runs `ALTER TABLE ... ADD COLUMN IF NOT EXISTS base_id
bigint`, which is a catalog-only change with no table rewrite. It creates a partial index
on `base_id` and `CREATE OR REPLACE`s the two functions. It is idempotent: bootstrapping
twice is tested. Existing rows are untouched. They are full rows, read as before, and
they age out with `retention.snapshots_days`. Nothing is rewritten at startup.

### Retention

The snapshots purge rule keeps a row while a retained row is built on it, directly or
through a checkpoint. A base used only by expired rows goes in the same run. The oldest
kept rows can therefore be up to 6 h older than the window.

### Size guard

The `sage_footprint` finding (`analyzer/rules_sage_footprint.go`) works like this:
- Each analyzer cycle measures `pg_total_relation_size` of every sage table against the
  `db_size_bytes` collected that cycle. Above `retention.sage_size_warning_pct` (default
  10, 0-100, 0 disables, API-settable) it raises a warning with the share, the limit and
  the five largest sage tables.
- The object is the database, not `sage`, so the self-monitoring filter, which drops
  findings about sage objects, does not hide it.
- An unknown database size or a failed query keeps an open finding open. A disabled guard
  resolves it.
- Documented in `docs/findings.md` and `docs/configuration.md`, with the key added to
  `config.example.yaml`.

## 3. Product decisions (made under the AI-DBA lens)

1. **Lossless, not sampled.** Every collected document stays readable exactly, so no
   consumer loses signal. Thinning the history (for example hourly snapshots) would have
   been simpler and smaller, but it would weaken unused-index evidence and the history
   export. Rejected.
2. **The accessor lives in SQL.** The forecaster and the operator's own queries are SQL.
   A SQL accessor lets them read history without the sidecar, and `docs/sql-reference.md`
   shows how.
3. **Two-level bases, not a chain per cycle.** Measured: deltas on the keyframe alone gave
   7.1x for indexes and 5.0x overall, because they grow with every counter that moved
   since the keyframe. A per-cycle chain is smallest but makes a read replay up to 360
   rows. Two levels give 21-26x for indexes, and a read applies at most two deltas.
4. **`system` stays full.** It is small and fully moving, and long-window SQL reads it
   (forecaster, verify). Encoding it would save about 2% and cost every one of those reads
   a decode.
5. **The size guard defaults to on (10%)** and is advisory only (no SQL, nothing executed).
   pg_sage must not bloat the database it guards, and a warning is the trust-earning
   first step. It tells the operator which knob to turn.
6. **No backfill.** Rewriting gigabytes of legacy rows at startup would lock and churn
   the guarded database. They age out with retention instead (90 days by default).

## 4. Measured reduction (bytes written per hour, one cycle a minute)

| Fixture | Category | Legacy | Delta | Reduction |
|---------|----------|-------:|------:|----------:|
| Real collector, 250 tables with 5,000 real indexes, light workload (`collector/snapshot_dedupe_db_test.go`) | indexes | 5,825,236 B | 223,420 B | 26x |
| | tables | 221,263 B | 44,197 B | 5.0x |
| | config_data | 90,646 B | 6,808 B | 13x |
| | io | 60,569 B | 12,219 B | 5.0x |
| | queries | 244,036 B | 102,993 B | 2.4x |
| | **`sage.snapshots` relation (heap, TOAST, indexes)** | **6,995,968 B** | **638,976 B** | **10.9x** |
| Synthetic, 5,250 indexes, 3% of indexes scanned per minute, every counter class moving (`snapstore/bytes_db_test.go`) | indexes | 9,362,153 B | 440,114 B | 21.3x |
| | tables | 618,267 B | 89,999 B | 6.9x |
| | **relation** | **11,255,808 B** | **1,032,192 B** | **10.9x** |

Both tests assert at least 10x for indexes and for the whole relation. They also assert
that every stored document reads back identical. A few points on these numbers:
- The relation ratio is lower than the indexes ratio for two reasons. Every row has a
  fixed cost (tuple header and three index entries, about 340 B); the row count is
  unchanged because readers rely on one row per category and cycle. And `system`,
  `locks` and `replication` stay full.
- The synthetic fixture moves every query and sequence counter every minute, so those
  two categories barely shrink (1.2-1.4x). That is honest: a delta cannot beat data that
  really changes.
- On lifeos (35k indexes, 15k tables, mostly idle) the per-row fixed cost is negligible
  next to the 700 kB index rows, so the ratio there should be far higher. That is
  inferred, not measured: I may not query lifeos.

Read cost (measured once, synthetic 5,250 indexes): decoding one `indexes` document takes
about 60-70 ms through the accessor, against 9-11 ms to read a legacy row; the newest
row takes about 90 ms. Each decode explodes and re-aggregates the 1.9 MB list once or
twice. Readers decode only what they use: the latest row, the history API's capped
points, two samples per day for the forecaster, and 100 samples for the regression
baseline (`queries`, about 500 elements). A 500-point `indexes` history export would take
about 30 s (it was about 4.5 s plus a 1 GB response before).

## 5. Spec CHECKs

None of the AI-SRE spec CHECKs targets snapshot storage. This work supports the spec's
cross-cutting rule that pg_sage must not harm the database it guards, through the
size guard and the bounded storage. It grants no autonomy: no action class, gate or
executor path changed, and `sage_footprint` is advisory (no SQL).

## 6. Test Results

**Command:** `go test -count=1 -cover ./...` in `golang:1.25`, repo root mounted, own
PG17 `pgsage-ag8` (:55478). Touched packages were also run with `-race -p 2` on PG17,
PG14 (:55414) and PG18 (:55418).

**Total:**
- Full suite: 82 packages with tests.
  - At default parallelism, 54 passed and 28 failed. Every failure was infrastructure on
    the shared Docker host: `drop fixture database ...: context deadline exceeded` (39
    occurrences), statement timeouts, and one nil-pool panic after a fixture timeout.
  - The 28 packages rerun with `-p 2`: 27 passed. `internal/executor` failed
    `TestQueueModeTimeoutParksSelfInitiated` (timing: "queued for 194ms, want about the
    300ms queue bound"), a package this branch does not touch. It passed when rerun
    alone.
- Touched packages with `-race`: 3,104 tests passed on PG17, 3,102 passed with 2 skipped
  on PG14, 3,104 passed on PG18. 0 failed, no data race.
- New tests on this branch: 65 test functions.

**Coverage (touched packages, PG17 `-race` run):**

| Package | Coverage |
|---|---|
| internal/snapstore (new) | 98.6% |
| internal/retention | 97.9% |
| internal/config | 90.8% |
| internal/forecaster | 87.4% |
| internal/collector | 87.1% |
| internal/analyzer | 85.8% |
| internal/schema | 81.6% |
| internal/api | 76.2% |
| internal/store | 74.5% |
| internal/testsupport/snapfixture (new, test support) | no own tests; exercised by the snapstore, api and analyzer goldens |

Lint: `golangci-lint run ./...` gives 0 issues.

### Skipped Tests (must be zero or justified)
- PG14 only: collector `TestCollectIO` and `TestPhase2_CollectIO_FieldsPopulated`.
  `pg_stat_io` exists from PG16; these are existing skips.
- No new test skips. Without `SAGE_TEST_DATABASE_URL` the DB tests skip through
  `testdb.SkipUnlessLive`, like every other package.

### Failures
- None attributable to this branch (see Total).

### Coverage Gaps (packages below threshold)
- All touched business packages are at or above 70%. `snapfixture` is a test-support
  package without its own tests.

### Bugs Found This Session
1. [DESIGN] The first encoding (every delta on its keyframe, absolute values) measured
   only 7.1x for indexes and 5.0x overall: deltas grow with every counter that moved
   since the keyframe. It was replaced by two-level bases, increments and the global
   increment (section 3).
2. [BUG, caught in test] `toast_tuple_target = 128` plus `STORAGE MAIN` was meant to
   compress small delta rows. It does nothing: PostgreSQL only invokes the toaster above
   the compile-time 2 kB threshold. Removed before commit.
3. [TEST] Four tests were logically wrong and were fixed, not weakened (commit
   `bb3b1fb`): an import cycle, documents too small for the documented size rule, a
   `time.Time` map key, and a wrong claim about single-element global increments.
4. [EXISTING] The old `buildHistoricalAverages` loaded every `queries` row of the
   lookback (up to `regression_lookback_days` x 1,440 full rows) to keep 100. It now
   reads 100.

### Manual Checks Remaining
- MANUAL: on lifeos, after deploy, compare `pg_total_relation_size('sage.snapshots')`
  growth per hour before and after. I was not allowed to query lifeos.

## 7. Post-test audit

- **Mutation testing.** I broke the key logic on purpose; all 12 mutants were killed:
  - increment sign flipped;
  - SQL global increment subtracting instead of adding;
  - retention ignoring the checkpoint level;
  - a new keyframe keeping the old checkpoint;
  - footprint boundary made inclusive;
  - emptiness ignoring the delta count;
  - exponent numbers allowed in a global increment;
  - rebase rule inverted;
  - SQL keeping removed elements;
  - chain limit set to 1;
  - explicit order dropped;
  - the analyzer reading its sampled documents out of order.
- **Inputs not covered.**
  - Two sidecars writing the same `sage.snapshots` (HA). Each writer keeps its own bases
    and names them explicitly, so reads stay exact. It is not exercised by a test.
  - A collector restart in the middle of a keyframe window (first cycle is a keyframe
    again) is covered only through `NewWriter` state. There is no process-restart test.
- **Assertions that would pass if broken.** The golden comparisons compare against the
  legacy rows, and both stores read through the same accessor, which returns legacy rows
  unchanged. A broken decoder therefore cannot pass, and the delta-row count is asserted
  non-trivial so that "everything stored full" cannot pass either.
- **Fakes.** None. Every storage and consumer test runs against real PostgreSQL, the
  real SQL functions and real `pg_stat_*`.
- **Not covered by an assertion.**
  - The analyzer `cycle()` wiring of `checkSageFootprint` (one line) is not driven
    end to end; the check itself is tested against the real schema.
  - The no-change fast path in `snapshot_apply` is a speed optimization. Its result is
    asserted, not its speed.

## 8. What is left / what the coordinator must decide

1. **Read cost of bulk history.** Decoding a 5,000-element `indexes` document costs
   60-70 ms instead of about 10 ms. A full 500-point `indexes` history export becomes
   about 30 s. If that endpoint matters, the follow-up is a Go-side streaming decoder
   for history, which loads the keyframe once and applies small deltas. The SQL accessor
   stays the single definition.
2. **`system` stays full.** It is the largest remaining fixed cost (about 22 kB/h) and
   is read raw by the forecaster and verify. Encoding it would save about 2% and make
   those reads decode. My recommendation is to leave it.
3. **Unused-index proof across stats epochs.** The rule uses the live
   `pg_stat_database` epoch and in-memory first-seen times; neither was ever persisted.
   History now keeps every `idx_scan` sample exactly, so resets are visible as
   decreases. A reset that happens between two samples with no scans before it remains
   invisible, as before. Persisting the stats epoch in `system` would close that gap. It
   is a small collector change and a product call.
4. **Merge note.** The migration is registered last in `bootstrap.go` (one line).
   `CHANGELOG.md`, `config.go` (one field plus a default line), the store, the API config
   registries and the generated `config_meta.json` / `config-lifecycles.md` each got
   small additive edits. If other branches add config keys, the `114 -> 115` count
   assertions in the store config consistency test need summing at merge.
5. **Lifeos rollout.** No backfill runs at startup. The 9.3 GB of legacy rows shrink only
   as they age past `retention.snapshots_days` (90 days by default). The new
   `sage_footprint` finding will fire on lifeos right away if sage is over 10% of the
   database. Decide whether to shorten retention there once, or accept the finding until
   the legacy rows age out.
