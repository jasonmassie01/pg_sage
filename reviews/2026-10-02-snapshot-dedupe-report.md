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

_Filled in below after the tests were written._
