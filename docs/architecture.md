# Architecture

pg_sage is a Go sidecar that connects to any PostgreSQL 14-18 over the network. The former C extension was removed (see the `c-extension-final` tag).

## Why a Sidecar

1. **No installation friction**: No `shared_preload_libraries`, no PostgreSQL restart, no matching PG major-version binaries. Works on managed services (RDS, Cloud SQL, AlloyDB, Aurora) where custom extensions are restricted or prohibited.

2. **Single implementation**: All collector, analyzer, optimizer, and executor logic lives in Go. No dual C/Go maintenance burden.

3. **Feature velocity**: The optimizer, web UI, briefing worker, and Prometheus exporter ship as a single binary with no database-side dependency.

---

## Pipeline

```
pg_sage sidecar (single Go binary)
  ├── Collector        [every 60s]
  │     pg_stat_statements, pg_stat_user_tables, pg_stat_user_indexes,
  │     pg_sequence (paged), pg_locks, pg_stat_replication,
  │     pg_stat_bgwriter, pg_stat_checkpointer, pg_stat_activity,
  │     pg_stat_io (PG16+), pg_database_size() (every 15 min)
  │
  ├── Analyzer         [every 600s]
  │   ├── Tier 1: Rules engine (25+ deterministic checks)
  │   └── Tier 2: Tuning agent (one per database; case-driven, read-only tools,
  │       typed proposals, optimizer admission + HypoPG, calibrated confidence)
  │
  ├── Executor         [trust-gated]
  │   ├── CONCURRENTLY DDL on raw pgx connection
  │   ├── DDL lock/rewrite/live-risk preflight + PR/CI script output
  │   ├── Incident playbooks for locks, runaway queries, connections, WAL,
  │   │   replication, and sequence exhaustion
  │   ├── Vacuum/bloat/freeze autopilot with IO and XID guardrails
  │   ├── Query tuning artifacts beyond hints: rewrites, statistics,
  │   │   parameterization, hint retirement, and role work_mem promotion
  │   ├── Rollback monitor (read + write latency regression)
  │   └── Emergency stop via sage.config
  │
  ├── API + Dashboard  [:8080]  REST API + React SPA (web UI)
  └── Prometheus       [:8187]  Metrics endpoint
```

---

## Component Details

### Collector

Gathers a snapshot every 60s (configurable) from the catalog and statistics views, under a
circuit breaker that skips cycles while the database is in trouble (active client sessions
over `max_connections` above `safety.cpu_ceiling_pct`). A cycle reads the catalog categories
in order (`catalogSteps`: `queries`, `tables`, `indexes`, `partitioned_indexes`,
`foreign_keys`, `system`, `locks`, `sequences`), then `replication` and the extra categories
(`extraSteps`: `io`, `prepared_xacts`, `archiver`, `partitions`, `index_bloat`,
`workload_sessions`, `wait_events`, `subscriptions`, `cdc_heartbeats` when
`replication.cdc_heartbeats` is set, `config_data`).

**Health.** A skipped cycle is logged with its cause: `load ratio 0.93 > ceiling 0.90
(active sessions / max_connections)`, or `load query failed: <error>` when the load query
itself fails (a permission or pooler fault, not load; it still counts toward dormancy).
`Collector.Status()` reports the breaker state (`closed`, `skipping`, `dormant`), the skip
reason and counts, the last load ratio, the last success and the last error, the latest
snapshot's unavailable categories, persist and `query_store` failure counts, and the
current capabilities. `GET /api/v1/databases` carries it per database as `collector`.

**Bounded reads.** Every catalog read runs under a statement and lock timeout and is
bounded by the size of the catalog it reads, not by the cycle:

- `tables` and `indexes` page through `pg_stat_user_tables` and `pg_stat_user_indexes` by
  oid. Index definitions (`pg_get_indexdef`) come from a cache and are read only for new
  or changed indexes, with a full refresh at least hourly.
- Only the 100 largest relations and the 100 largest indexes are sized exactly each cycle
  (`ExactSizeTopN`); every other size is `relpages` x block size.
- `sequences` reads `pg_sequence` in pages of 1,000 sequences per catalog transaction, never
  more than a quarter of the server's lock table, and at most 20,000 sequences a cycle;
  past that it rotates through the catalog across cycles (reading every sequence in one
  transaction held one lock per sequence and exhausted the lock table).
- `pg_database_size()` (a `stat()` of every file of the database) is measured every 15
  minutes and carried between measurements.

**Storage.** Snapshots are written through `internal/snapstore`: a category is stored as a
full row (a keyframe) when it has no usable keyframe, the keyframe is 6 hours old, or the
change is too large, and otherwise as a delta row on its keyframe or on the latest
checkpoint (changed fields by element identity, counters as increments). Every reader goes
through `sage.snapshot_data()`, which returns the document as the collector wrote it.
`sage.snapshots` and `sage.query_store` are range-partitioned by UTC day; the collector
creates each day's partition ahead, and retention (`retention.snapshots_days`,
`retention.query_store_days`, `retention.snapshots_max_pct`) drops whole days.

**Query store.** Each cycle the collector also writes per-query samples (calls, total time,
rows, statistics epoch, plan fingerprint) to `sage.query_store` through a
`querystore.Recorder`, which writes a query only when its counters or epoch moved since its
last sample, and at least once an hour (a keyframe). Readers take an absent sample as
"unchanged": a window starts at the query's newest sample before it, within two hours
(`AnchorLookback`). Samples of different statistics epochs (a counter decrease, a changed
`pg_stat_statements_info.stats_reset`, or an unknown epoch next to a known one) are never
differenced (R10). Both rules live once, in `internal/querystore` (`QueryIntervals` for
verification, `CaptureDeltas` for the SLO latency proxy).

**Hourly per-query rollup.** `sage.query_store` keeps 14 days; the snapshot `queries`
category (calls, execution time, rows, shared and temp blocks, WAL of the admitted top
`collector.max_queries`) keeps 90. A `querystore.RollupWorker`, started beside each
collector, sums per queryid and UTC hour the differences between consecutive `queries`
snapshots into `sage.query_rollup_hourly`, and writes one `sage.query_rollup_coverage` row
per hour: the snapshots it saw, the hour's sampled execution time against the database's
own statement time (`statements_total_exec_ms` of the system category, all entries but
pg_sage's own, whatever the cap admits; NULL when unknown), the epoch breaks, the
`pg_stat_statements` deallocations, and whether the epochs were recorded or, for snapshots
written before the system category carried `statements_epoch`, inferred from counters only.
A pair of snapshots counts only within one statistics epoch (R10, the same `EpochBreak`
predicate) and at most `AnchorLookback` apart; a breaking pair (a reset, a Neon or Lakebase
suspend) adds nothing and flags `epoch_break`, so a counter is never negative (a `CHECK`
refuses one) and an hour undercounts one interval at worst. The pair's later snapshot
decides its hour. Each hour is rolled in one transaction under a 30 s statement bound and
a per-database advisory lock and upserted, so rolling it again gives the same rows. The
worker backfills from the oldest snapshot, 24 hours per pass every 5 minutes (the first
90 days take about 7.5 hours per database), then rolls the previous hour at :05 and
re-rolls the one before it for late snapshots. `SnapshotIntervals` exposes the pairs to
other readers. The hourly rows follow `retention.query_rollup_hourly_days` (35, never past
`snapshots_days`) and the coverage rows `retention.query_rollup_daily_days` (400);
`pg_sage_query_rollup_hours_total{database,result}` and
`pg_sage_query_rollup_lag_hours{database}` report progress.

**Daily rollup and coverage.** Each worker pass then rolls every closed UTC day whose last
hour is rolled into `sage.query_rollup_daily` (`querystore.RollupDay`: per queryid the
day's sums, the hours with a row and with a break, the number of plan fingerprints and
the last one), day d-1 from 00:15, at most seven days per pass, so a backfilled day is
rolled as soon as its 24 hours are; the newest day is re-rolled until 01:15, after its
23:00 hour's late re-roll. A re-roll replaces a row only with a sum over at least as many
hours, and retention removes an hourly row only once its day's daily row exists (or the
hour is past `query_rollup_daily_days`, so an unrolled hour is never kept forever), so a
purge racing the roll never loses a day. `GET /api/v1/queries/coverage` sums the coverage
rows of a window (default 28 days): the covered share of execution time over the hours
whose total is known, the hours with an epoch break, missing hours, dealloc growth and
the rollup's measured bytes, with a status: `unknown` without statement totals (never
`ok` by default), `mostly_breaks` when more than a quarter of the hours with workload
broke (a compute that suspends), `learning` under 14 days of history, else `ok`. The
workload explorer shows it as one line under the top statements, silent when `ok` above
90%. A history migration (`pg_sage history migrate`) copies the daily and coverage rows
and rebuilds the hourly tier from the copied snapshots ("hourly rollup rebuilt").

**Workload evidence pack.** A pack (`internal/evidence/workpack`, format
`pg_sage-workload/1`) is one database's workload over a window of 14 to 100 whole UTC days
(default 28), as canonical JSON sections sealed with the evidence pack format:
`header.json` (database, window, hour-of-week timezone `baseline.timezone`, version,
platform, collector capabilities, and each day's tier: a day the rolling hourly purge
(`query_rollup_hourly_days`, with an hour's margin for the purge's time zone) has reached,
even in part, reads the daily rollup and is listed `daily`; a daily day the window only
partly covers is not counted), `coverage.json` (the coverage report of the window), `workload.json`
(families, each a statement's command plus its sorted referenced relations or
`unparsed:<queryid>`: totals, top 20 queryids and a 168-bucket hour-of-week shape from
the hourly days, a bucket seen under three weeks marked learning; at most 50 families and
500 queryids by execution time, with `truncated`) and `growth.json` (weekly calls and
execution time per family, daily database and 50 largest table sizes, each with slope,
R2 and the slope's standard error; under three points the trend is `unknown`, a day with
no snapshot is `null` and counted in `missing_days`). `content_sha256` is the SHA-256 of
the sorted `name sha256` lines, so the same window and data give the same hash; when and
by whom it was built is stored beside it, never in it. Every fact has an evidence id from
what it is about (`workload/family/<12 hex of the key's SHA-256>`, `workload/query/<queryid>`,
`coverage/window/all`, `growth/workload/<family>`, `growth/db/size`,
`growth/table/<schema.table>`), resolved against the pack that holds it. A build only
reads the history store, in one read-only transaction with a 30 s statement timeout, and
stores the pack once per database and hash in `sage.workload_packs` beside the history
(`history.store`); a stored pack is never changed, and
`retention.workload_packs_days` (180) deletes old ones. `POST
/api/v1/databases/{db}/workload-packs` (operator) builds one within 60 s, answering
`reused: true` for a window whose data is already stored; `GET
.../workload-packs`, `.../{sha}` and `.../{sha}/evidence/{id}` read them (viewer, scoped to
the database). `pg_sage_workload_pack_builds_total{database,result}` and
`pg_sage_workload_pack_bytes{database}` report builds.

Packs built since evid-02b hold nine more sections, each fact with an id. Nothing
pg_sage could not measure is ever zero or left out: it is `unknown` (or `absent`,
`unreadable`, `unavailable`).
- `indexes.json` (`index/idx/<schema.index>`): every index of the user schemas, from
  `pg_index`. Each has its size, definition, the unique, primary, constraint, valid and
  partial flags, and exact-duplicate and left-prefix overlaps (same table, method,
  predicate, keys, operator classes, collations and options). It also has the window's
  scans and its table's writes, from the first and last `indexes` and `tables` snapshots.
  Both are `unknown` when the relation-statistics epoch moved in between. There is no
  advice.
- `settings.json` (`setting/<name>`): the last config snapshot's settings, with their
  changes by day. Beside them are the measures they are judged by: temp bytes per hour,
  requested and timed checkpoints and WAL bytes per hour, each only within one
  statistics epoch, and peak client backends against `max_connections` less the
  reserved slots. Hardware (vCPUs, memory) comes from `sage.host_usage_hourly` for the
  database's cloud resource.
- `autovacuum.json` (`table/av/<schema.table>`): autovacuum and autoanalyze runs over the
  window, the last times, a daily dead-tuple series and the reloptions (one that does not
  parse is kept as text).
- `schema.json` (`schema/table/<schema.table>`): tables and partition roots, with size,
  partition key and partition count, columns, constraints and index definitions. At most
  2,000 tables and 20,000 columns, with `truncated`; a table whose constraint or index
  list reached its bound is left out with the tables after it, never listed half.
  pg_sage's and the system schemas are left out.
- `plans.json` (`plan/query/<queryid>`): the latest captured plan before the window's
  end of each of the top 50 queryids. It is shape only (`planhash.Shape`: structure,
  plan rows, total cost, actual rows and loops), with its fingerprint.
- `connections.json` (`connections/hour/<0-167>`): mean client sessions per hour of week,
  by state and by connection age (under 1 min, 1-10 min, 10-60 min, 1 h or more, and
  `unknown` for a session whose start the monitoring role cannot see).
- `provider.json` (`provider/<fact>`): instance class, vCPUs, memory, storage and the
  serverless flag from the cloud telemetry, or `provider/status = unknown`. It is never
  inferred.
- `stats.json` (`stats/column/<schema.table.column>`, `stats/table/<schema.table>`):
  `null_frac`, `n_distinct`, `correlation` and `avg_width` of the referenced tables'
  columns.
- `context.json` (`context/brief`, `context/fact/<type.kind.subject>`): the confirmed
  owner brief and binding facts, with who confirmed them and when.

Each catalog section reads the monitored database in its own read-only transaction with
the safety statement and lock timeouts. A failed section fails the build and names the
section and the bounds. The exception is `provider`, which records `unavailable` with its
error class. A build's body may set `include_conditions` (each full plan, every string's
literals redacted) and `include_value_samples` (`pg_stats` most common values and
histogram bounds, into `stats.json` only). Both are off by default. The manifest's
`completeness` gives, per section, whether every fact is known, and the share of
sections that are. Packs built before keep their hash and still open.

Since evid-02c a pack also holds `texts.json`: the statement text of each listed queryid
(at most 16 KB, `truncated` past it). It has no evidence ids, so resolving facts never
returns a text. A pack has three redaction tiers:
- **local**: the stored pack. It never leaves pg_sage.
- **model** (`workpack.ForModel`, `GET .../workload-packs/{sha}?tier=model`, viewer; MCP
  `get_workload_pack`): what a model may read. Statement texts go through
  `llm.SanitizeForLLMStrict`, which redacts literals and comments and long compared
  numbers whatever `llm.redact_numeric_literals` says, and are cut at 2,000 characters.
  `stats.json` keeps only its four numeric fields. Plan conditions are dropped.
  Constraint, index and partition definitions go through `llm.RedactSQLLiterals`, and so
  do owner notes, cut at 300 characters. Who confirmed a fact (an email) is dropped.
  Settings whose name holds `conninfo`, `password`, `passphrase`, `command`, `secret` or
  `token` read `redacted`. With `hash_identifiers=true`, every schema, table, index and
  column name becomes `t_<10 hex>` of an HMAC-SHA256 keyed by the install's pseudonym key.
  That key is 32 random bytes in `sage.workload_pseudonym_key` on the control database,
  sealed under `encryption_key`, so a name keeps its pseudonym across packs, restarts and
  key rotations. The map back to names is returned beside the tier, never inside it.
  `tier.json` names the local pack the tier came from. `charts/<family>.png` and
  `charts/<family>-growth.png` are 336 x 64 PNG sparklines of the top 10 families'
  hour-of-week exec time and weekly growth. A leak guard checks every string of the tier
  before it is returned. It refuses the tier (`422 model_leak`, naming the section and
  field, never the value) when a string holds any of these:
  - a most-common value or histogram bound the build read;
  - a string literal of the local tier's texts, plans, definitions or notes;
  - a credential-shaped token (`sk-`, `AKIA`, `ghp_`, `xox`, `-----BEGIN`).

  The guard skips values under 6 characters, values without a letter, names of the pack
  and words of its statement structure. A value matches only as a whole token
  (`is_active` does not hold `active`). Credential-shaped tokens are refused everywhere,
  but values are not searched for in map keys (session states, option names), in strings
  that are exactly a name of the pack, or in fixed-vocabulary fields such as a setting's
  `source`, a plan's `Node Type` or the header's `timezone`. Without these exemptions,
  a common literal such as `'active'` or `'default'` would refuse nearly every tier.
- **export** (`GET .../workload-packs/{sha}/export`, admin): the local tier without value
  samples, plus the charts, as a tar.gz in format `pg_sage-workload/1`. It is signed with
  the evidence key and checks with `evidence.Verify` or `sha256sum -c SHA256SUMS`. It
  holds the statement texts, so it is admin only and recorded as `evidence_exported` in
  `sage.auth_audit`. Without a signing key it is refused (`409 no_signing_key`), never
  sent unsigned.

The database page's "Evidence packs" card builds packs (operator), shows "What would
leave" (the model tier) and downloads the signed export (admin). Nothing sends the model
tier to a model yet. Decision D3 decides which providers may receive it and whether
identifiers are hashed by default.

**The workload rule.** The collector keeps every statement. Advice paths (findings, the
tuning agent, plan capture, verification targets, the briefing) read only application
workload as `internal/workload` classifies it: pg_sage's own tagged statements,
`EXPLAIN`, maintenance, statistics resets and backup `COPY ... TO` are not workload, and
neither are utility statements (transaction control such as `BEGIN`, `COMMIT`,
`SAVEPOINT`; `SET`, `RESET`, `SHOW`, `DISCARD`, `DEALLOCATE`, `LISTEN`/`NOTIFY`, `LOCK`)
or DDL and privilege changes (`CREATE` other than `CREATE INDEX`, `ALTER`, `DROP`,
`TRUNCATE`, `COMMENT`, `GRANT`, `REVOKE`): no index or hint fixes a slow `COMMIT`.
Procedures (`CALL`, `DO`) stay workload, so a slow one is still reported. Capacity, I/O
and incident rules (RCA, SRE probes) still see every statement. The one check for what a
planner may `EXPLAIN` (plan capture, the HypoPG what-if, the perf gate) is
`workload.Plannable`: one `SELECT`, `VALUES`, `TABLE`, `WITH`, `INSERT`, `UPDATE`,
`DELETE` or `MERGE` statement after any leading comments; the what-if plans only the
read-only ones.

Each snapshot holds one row per category: `queries`, `tables`, `indexes`,
`partitioned_indexes` (the partitioned tables' own indexes, relkind `I`), `foreign_keys`,
`system`, `locks`, `sequences`, `replication`, `io` (PostgreSQL 16 and later), `partitions`,
`prepared_xacts` (this database's two-phase-commit transactions from `pg_prepared_xacts`),
`archiver` (the server's one `pg_stat_archiver` row with the current WAL file and
`wal_segment_size`, for the WAL archiver findings),
`index_bloat` (the largest btree indexes' bloat estimated from planner statistics, at most 200),
`workload_sessions` (this database's client sessions by role, application and state, at most
200 groups, its 50 longest-running sessions with their sqlcommenter tags, and per-role
statement time and cluster-wide session counts, which workload budgets read, and its
client sessions by state and connection age: under 1 min, 1-10 min, 10-60 min, 1 h and
over, and by application and whole minute of connection age, at most 500 rows, the
biggest first, for Sage Review's recycle herds; snapshots written before carry no ages),
`wait_events` (this database's active client sessions grouped by wait class and event, a
session without a wait event counted as `CPU`: one aggregate of `pg_stat_activity` per
cycle, with no query text and no pid, and without pg_sage's own sessions; it is sampled
each collector cycle, not active session history, and is the workload explorer's wait-load
chart),
`subscriptions` (this database's logical replication subscriptions, their apply workers and,
from PostgreSQL 15, their error counters; `subconninfo` is never read; at most 200)
and `config_data` (the advisor's settings, reloptions, connection and WAL snapshot, collected only
when an advisor will consume it). A category that could not be read in a cycle is
**unknown, not empty**: it is listed in the snapshot's `unavailable` map with the error, no
row is written for it, and rules that would read its absence as a fact stay quiet for that
cycle. Only the `system` category, the snapshot's spine, fails the whole snapshot. A category
pg_sage does not collect (`io` below PostgreSQL 16, `config_data` without an advisor) is not
collected, which is different from unavailable.

The `tables`, `indexes`, `foreign_keys`, `partitions` and reloptions reads leave out temporary
relations (`relpersistence = 't'`: other sessions' and pg_sage's own), which no other session
can vacuum, analyze or index; unlogged tables are still collected.

`queries` admits 2 x `collector.max_queries` (N) statements of this database on their
`pg_stat_statements(false)` counters, without texts, through four arms, each recorded in the
statement's `admitted_by`: `total`, the top statements by total time; `mean`, the K slowest
by mean time with two calls or more (a 20 s report run 50 times a day ranks below hundreds
of OLTP statements by total time); `io`, the K heaviest by blocks read plus temp blocks
written; and `pinned`, the queryids something depends on: the targets of open index
verifications and in-memory watches, at most 64 a cycle, oldest first. K is N/10, at least 1
and at most 50; the total arm keeps 2N - 2K - pins rows, never fewer than N, so the row
count, and the text read after it, stay at 2N. The mean and io arms leave out the queryids
the text cache already knows as pg_sage's own. Pinned rows, then the other arms' rows, are
kept first when the list is cut to N, and the list is published by total time. An N below 2
uses the total arm alone. These bounds are internal constants, not configuration.
`pg_stat_statements(true)` loads the
whole query-text file whatever rows are kept, so each collector keeps the texts in a cache
of its own, keyed by `queryid` of its own database (no sharing between databases, also in
fleet mode). The cache is bounded at 4 x `collector.max_queries` entries and 32 MiB of text
per collector, evicting the least recently seen. A cycle reads texts, in one statement
tagged `/* pg_sage text */` for exactly the missing ids, only when a candidate is new to the
cache or its text is an hour old: a steady workload reads them about once an hour. A new
statistics epoch (`pg_stat_statements_reset()`, a restart) clears the cache, and rising
`pg_stat_statements_info.dealloc` refreshes it on the next cycle. pg_sage's own statements
are left out in Go on the texts (`selfmonitor.IsStatementText`, the same predicate as the
SQL filter), then the list is cut to `collector.max_queries`; a candidate whose text is NULL
or gone cannot be attributed and is left out. A failed text read makes `queries`
unavailable: its counters are never published without their texts. Near
`pg_stat_statements.max`, the utility and `COPY ... TO STDOUT` classification (which reads
every text) runs at most hourly, and only when the entry or deallocation count moved.

The catalog categories and the extras (`io`, `prepared_xacts`, `archiver`, `partitions`,
`config_data`) share one retry budget per cycle: a read that hit a statement or lock timeout
is read once more, and at most two reads are retried per cycle. `config_data` is all or nothing: if any of
its sub-reads (`pg_settings`, `set_privileges`, `reloptions`, `connection_states`,
`wal_position`, `extensions`, `database_owner`) fails or returns a row that does not
scan, the whole category is unavailable and the reason names the sub-read. On a hot
standby the WAL position is the last WAL received (or replayed), since `pg_current_wal_lsn()` fails there.

`prepared_xacts` is stored as changes on a keyframe, paired by `gid`; a database with no
prepared transaction stores `[]`, which differs from the missing row of an unknown cycle.

`foreign_keys` holds one element per constraint: `schemaname`, `table_name`,
`ref_schemaname`, `referenced_table`, `constraint_name`, the constraint `oid` and `columns`,
the referencing columns in declared (`conkey`) order. Partition clones (`conparentid <> 0`)
are left out, so a key on a partitioned table, or one referencing a partitioned table, is one
element naming the partitioned parent. The category is stored as changes on a keyframe,
paired by schema, table and constraint name; no keys stores `[]`. Rows written before this
shape held one element per column with no schema; `/api/v1/snapshots/latest` regroups such a
row per constraint, with an empty schema.

### Analyzer (Tier 1 -- Rules Engine)

25+ deterministic rules across 6 categories. No LLM required:

| Category | Rules |
|----------|-------|
| Index health | unused_index, invalid_index, duplicate_index, missing_fk_index |
| Query performance | slow_query, high_plan_time, query_regression, seq_scan_heavy |
| Sequences | sequence_exhaustion |
| Maintenance | table_bloat, xid_wraparound |
| System | connection_leak, cache_hit_ratio, checkpoint_pressure |
| Replication | replication_lag, inactive_slot, slow_replication_slot, subscription_errors, subscription_lag, cdc_consumer_stalled, cdc_heartbeat_stale |

Each rule produces findings with severity (critical/warning/info), recommended SQL, and rollback SQL.

**Seasonal baselines (`internal/baseline`).** Fixed thresholds cannot tell a report that is slow every Monday at 09:00 from a real slowdown. Each cycle the analyzer folds what the collector already read (statement interval means, active and total backends, WAL bytes per second, lock-graph sessions per wait class) into one of 168 hour-of-week buckets per week in `sage.baseline_samples`, keyed by database, and scores the new value against the same bucket in the previous `baseline.weeks` weeks with a robust z over the median and MAD. `workload_anomaly` opens after 2 consecutive anomalous cycles and resolves after 2 normal ones; a bucket with fewer than 3 weeks is learning. `query_regression` judges against the bucket median when it is active. Scoring is deterministic (no LLM) and adds no query on the monitored database; the fixed-threshold rules remain the floor. A critical anomaly is a Sage SRE trigger (`workload_anomaly`), one read-only investigation per episode.

### Tuning agent (Tier 2)

Lives in `internal/tuning/` (roadmap 2.2). One agent per database replaces the former
optimizer, advisor vacuum/memory and tuner hint prompts:

- **Workload classification** (deterministic): application, tenant-family, test-fixture,
  diagnostic and pg_sage statements; confirmed facts give each table its routes.
- **Cases**: a top statement by interval time (at least 5% of the workload), a regression
  (mean time at least doubled), or write amplification (index maintenance on an unused or
  covered index, or dead-tuple churn). No case, no model call.
- **Read-only tools**: statement, table, explain (cache or `EXPLAIN` without `ANALYZE`,
  generic plans on PG16+), whatif_index (HypoPG), write_cost, extended_stats, and rehearse
  (clone provider only). Bounded results, wrapped as untrusted data, numbered as evidence.
- **Typed proposals only**: index create/drop, server setting, storage parameter,
  `CREATE STATISTICS` (`sage_stx_*`), query hint, each with cited evidence and a predicted
  effect. pg_sage generates every statement.
- **Validation**: index creates go through the optimizer's admission (`internal/optimizer`:
  canonical form, 8 validators, rejection memory in `sage.optimizer_rejection`, HypoPG
  what-if; a verified measurement replaces the model's estimate; on PostgreSQL 18 a
  single-column candidate an existing low-cardinality composite already serves through
  btree skip scan, shown by the generic plans of its top statements, is rejected as
  `served_by_skip_scan:<index>`, see
  [PostgreSQL 18 skip scan](findings.md#postgresql-18-skip-scan)); configuration through the
  advisor's allowlists and ranges; hints through the tuner; confirmed facts redirect bound
  changes to source-fix packets; operator rejections are not re-proposed.
- **Calibrated confidence**: per action class and prediction method from
  `sage.action_outcome` (reliability bins by predicted improvement, Wilson intervals);
  "uncalibrated" below `tuning.calibration_min_outcomes`; a lower bound under the
  confidence threshold needs operator approval.
- **Budgets**: per database per cycle (`tuning.max_requests_per_cycle`,
  `tuning.max_tokens_per_cycle`), at most `tuning.max_cases_per_cycle` cases; a case whose
  recent answers were all wasted is not asked again until it changes.

### Executor (Tier 3 -- Trust-Gated)

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Configured | No actions -- cases and recommendations only |
| **advisory** | Configured | Auto may execute eligible SAFE actions; higher risk queues |
| **autonomous** | Configured | Auto may execute eligible SAFE and MODERATE actions; HIGH queues |

HIGH-risk actions always require manual approval. Every action carries a typed
contract: risk tier, guardrails, expiration, rollback or mitigation, policy
decision, lifecycle state, and verification state. Execution outcomes are
logged to `sage.action_log`; pending work and approval outcomes are tracked in
the action queue.

Execution mode is an independent operator control. `manual` disables all
background queueing and execution at every trust level. `approval` queues
supported actions whenever trust is advisory or autonomous. `auto` executes
eligible SAFE actions at advisory trust, executes eligible SAFE and MODERATE
actions at autonomous trust, and queues higher-risk supported actions. Neither
trust nor its ramp can promote `manual` to `auto`. Emergency Stop and a
per-database disabled executor are hard mutation blocks.

**Verification v2 (attribution).** Today's judge compares the targets'
call-weighted time per call with the same-length window just before the action.
Version 2 (`verify.Attribute`; it decides a verdict only under `verify.method: v2`,
see below) asks a narrower question: did the targets' work change because of this step?

- **Metric.** Shared blocks (hit plus read) per call, which moves with the plan and
  not with cache warmth or a busy host. Time per call is a guardrail: a rise beyond
  `verify.regress_pct` whose confidence interval is above zero vetoes a gain, but
  time alone never makes one.
- **Frozen weights.** The targets are pooled with their call shares from before the
  step, stored with the watch, so traffic moving toward a cheap target after the
  step does not look like an improvement. Each target is read against its own
  level before the step, so a low-traffic target that runs in only some buckets
  does not move the pooled value either. A target with no calls after the step is
  dropped from the pool, and the evidence says so.
- **Seasonal match.** When at least 3 of the 4 earlier weeks have the same
  hour-of-week in the hourly rollup (`sage.query_rollup_hourly`), the after-period
  is compared with those weeks (`basis: seasonal`); otherwise with the pre-window
  (`basis: pre_window`), and the evidence gives the weeks found.
- **Control group.** For an index or reloptions step, statements in the admitted
  top set that do not touch the changed table are controls: the effect is the
  targets' change minus the median control change (`method: did`), so a deploy or a
  noisy neighbour that slows everything is not charged to the step. With fewer
  than 3 controls the evidence says `controls<3` and there is no control term
  (`method: its`); a global setting has none by design ("no control group: global
  setting").
- **Confidence.** Welch's comparison of per-bucket log ratios (the same machinery as
  v1) gives the interval; the effect is reported in log space and as a percentage.
- **Evidence gaps.** A statistics epoch change inside a window (a reset, or a Neon
  or Lakebase suspend and resume) gives no measurement, as do no blocks before the
  step or too few buckets; each says why.

The counters come from the snapshot `queries` category through the same reader as
the hourly rollup, in the database's history store. Watches stored before v2 carry
no frozen weights and are judged by v1 only.

Three guards sit in front of the v2 estimate:

- **Mechanism required** (`verify.MechanismCheck`). An index step is credited for a
  target only when the index can have caused the change: the target's plan
  fingerprint in the query store changed within one collector cycle of the step,
  and its newest plan in `sage.explain_cache` names the index in an `Index Name`
  node (a name in a filter or condition does not count). For a dropped index the
  plan must change and no longer name it. A target that improved without that is
  "unexplained" and left out; with no target left the result is
  `insufficient_evidence` with reason `no_mechanism`. A setting's mechanism is the
  metric the executor already maps to it (temp spills for `work_mem`, dead tuples
  for autovacuum knobs, requested checkpoints for the WAL bounds); a setting with
  no mapped metric is `no_mechanism`. A plan without actual row counts (the
  generic `EXPLAIN` that is the only evidence on Neon and Lakebase) is labelled
  "estimated plan". If the plans cannot be read, the reason is
  `mechanism_unavailable: <cause>`: insufficient evidence, never a revert.
- **Contamination** (`verify.DetectContamination`). Each target's residual and the
  median control's residual after the step are scanned with a two-sided CUSUM
  (k = 0.5 and h = 5 robust deviations, the scale `baseline.Judge` uses, floored
  at 5%). The step's own effect is the window's level, so a break that starts more
  than one collector cycle after the step (a deploy at hour 5 of a 24-hour window)
  marks the window contaminated: `insufficient_evidence` with reason
  `contaminated_at=<time>`, and the next look starts after the break. An alarm
  counts only when the levels on either side differ by at least 3 standard errors,
  so slow drift is not called a break.
- **Repeated looks** (`verify.SpendLook`). A watch is looked at 2, 4, 8, 16, 32, 64
  and 72 hours after the step with the defaults, and seven tests at 5% each would
  call about one in five unchanged steps a change. Each look instead spends a share
  of the 5% by an O'Brien-Fleming-type schedule of the information so far (calls
  seen over the calls expected by the hard maximum): early looks need
  overwhelming evidence, and the last look spends exactly what is left. The shares
  add up to 5%, the per-target Bonferroni split stays on top, and the spend is
  stored with the watch (`alpha_spend` in its baseline) so a restart continues it.

**Verification v2 in the executor** (`verify.method`). `v1` judges by v1 only;
`shadow` computes v2 beside v1 at every look and acts on v1; `v2` acts on v2 and
keeps v1 for comparison; `auto` (the default) is `v2` on a database whose latest
verifier back-test passed within 35 days and `shadow` otherwise. The back-test
(`internal/verify/backtest`, [verification.md](verification.md)) replays placebo
steps over the database's stored history every week on the leader and stores its
rates in `sage.verifier_backtest`; until a passing row exists `auto` is `shadow`
and no verdict changes. The choice is read at each look through a 10-minute cache, so a new
back-test takes effect without a restart. `verify.JudgeV2` maps v2's evidence to
a verdict: a confidence interval below zero with the effect at or beyond
`verify.min_gain_pct` and the mechanism present is `improved`; an interval inside
both bars is `neutral` ("No change", and the index is kept); an interval above zero
beyond `verify.regress_pct` on blocks, or the time guardrail, is `regressed`; a
contaminated window, no mechanism or too wide an interval is
`insufficient_evidence`, extended until the last look, which reverts as
unverifiable as v1 does. `sage.action_outcome.verifier` names the verdict that was
acted on, and `other_verdict` / `other_evidence` hold the other one; the trust
ledger reads `verdict` only. The post-action monitor stores v2 beside v1 for
index drops, settings and storage parameters, and acts on v1 in every mode.

**Up-front unverifiable.** Before a step runs, its targets' hourly history
(`sage.query_rollup_hourly`) gives the calls expected within
`verify.window_max_minutes`: per hour, the median of the same hour of the week over
the last 4 weeks. Fewer than `verify.min_samples` expected calls, or fewer than 3
active hours, mean no watch could judge the step: it needs an operator's approval at
every trust level, and the card says "cannot be verified within 72 h: the targets
run N times a week". No history (a new database) is `unknown`: recorded with the
finding and never inferred either way. VACUUM, ANALYZE and REINDEX are judged at
once and exempt.

**Expired monitors** (`verify.on_expired`). A monitor whose window ended while
pg_sage was not watching keeps its `verification_expired` record, and then: `ask`
(the default) queues one "Revert action N" item through the revert card (approving
it runs the recorded rollback; rejecting it records "kept by operator"); `keep`
keeps the change; `revert` rolls it back through the operator rollback. Each path
writes the decision ledger.

**Verification ladder** (lab-03a). Each queued change records what it was tested
on before production, in `sage.action_queue.ladder` (one record per tier, appended
by each producer with a single `UPDATE ... SET ladder = coalesce(ladder, '[]') ||
$1`): tier 1 `plan_only` (the optimizer's HypoPG what-if for a create, generic
plans on PG16+; the HypoPG hide what-if for a drop; a hint's plan comparison; or
a reloadable setting's target plans under `SET LOCAL` in a read-only transaction
that is always rolled back, plan cost only), tier 2 `clone` (a clone rehearsal,
where a clone provider is configured), tier 3 `canary` (the production verdict,
recorded when it lands; a later verdict of the same action replaces it) and tier 4 `fleet` (a staggered fleet rollout). A tier
that could not run is recorded as skipped with why ("no clone provider
configured", "HypoPG cannot model gin indexes"); a method that cannot model the
index type, or an unreadable result, is never a pass. The entry rule: every
action class says whether it is cheap to revert (create index, reloadable
setting, storage parameters, hint, extended statistics: yes; drop index, index
replace, ALTER TABLE, a setting that needs a restart: no). A finding's change
whose class is not cheap to revert and has no passed tier 2 is queued for approval
at every trust level, and its card says "not rehearsed on a clone"; automatic
rollbacks are not gated, and a rehearsal that is both passed and inconclusive is
no pass. A bounded retention delete is classed costly too, but its authority stays
the owner's declared retention contract (it does not run through the finding
gate). At execution the ladder is copied into the action's
prediction (`sage.action_outcome.predicted.ladder`), so the outcome ledger row
shows the highest tier passed. The approval card lists each tier, or "No
pre-production test recorded" for an item queued before the ladder existed.

High-risk schema changes are handled as migration-safety cases before direct
execution. The case projector attaches deterministic DDL preflight evidence,
including lock/rewrite classification plus live table-size, activity, pending
lock, replica-lag, and lock-timeout checks. It also attaches generated
migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI
metadata.

The live values come from the migration advisor's risk assessment, written
into the finding detail when the DDL is detected (`table_size_bytes`,
`estimated_rows`, `stats_known`, `active_queries`, `longest_query_sec`,
`pending_locks`, `replication_lag`, `estimated_lock_ms`, `lock_level`,
`requires_rewrite`, `affected_table`, `measured_at`). They are
detection-time values, not a fresh probe: the preflight notes "measured at
detection" and the dashboard shows their age. A live read that failed is left
out, so its check reads `unknown` rather than passing on a zero (with every
read failed there is no `measured_at` and no lock-hold estimate), and table
size is `unknown` when the table was never analysed (`stats_known = false`).
Findings written before these keys existed keep rendering `unknown`. Lock
level and rewrite come from the finding first (the candidate's own
`CONCURRENTLY` SQL takes its own lock), then from the one DDL rule catalog
(`internal/migration/ddlrules`) that the classifier also uses. A measured
warning (for example an active query on the table) blocks the candidate for
review. `active_queries` leaves out the detected DDL's own backend, which is
not load it competes with; its own queued lock still counts in
`pending_locks`. `estimated_lock_ms` is a heuristic estimate, so its check
says "estimated at detection", never "measured".

An in-flight DDL is analysed once per backend and statement but re-evaluated
on every activity poll, and each poll re-persists its finding, so a DDL still
running after 24 hours keeps its finding open. The detector adds no blocking
query of its own: it reads the lock-chain fast path's latest observation
(the `pg_blocking_pids` walk, which keeps each session's direct waiters) and,
while a tracked DDL waits on `Lock`, nudges the fast path (at most once per
10 s). When `migration.queue_waiters_threshold` (default 3) sessions wait
directly behind the DDL in an observation at most two fast-path intervals old,
the finding turns critical with the DDL's backend identity and an
approval-only, HIGH-risk guarded cancel of the DDL, followed by a retry under
`SET lock_timeout = '5s'`; the lock-chain finding and the RCA lock-contention
incident for that queue keep their evidence but no longer offer a signal to
the root reader. An unqualified table name resolves through the DDL role's
`search_path` (`pg_db_role_setting`, else the server default `"$user", public`),
then `public`, then a unique relation name; several matches are reported as
ambiguous with statistics unknown. These artifacts are shown in Cases and
Actions so teams can review
schema work through their normal change-control process.

Migration enrichment is asynchronous. Detection never waits for a model:
both detectors store the deterministic finding first, then queue the model
work on one worker per database, with a queue of 16 that drops the oldest
request when full (`pg_sage_migration_enrich_dropped_total{database}`). The
worker asks for a migration script for a classified statement, or runs the
LLM fallback for one no rule classified; the fallback's finding is stored only
when it scores above the 0.3 threshold. One answer per statement identity
(sha256 of the rule, the sanitized statement and the table's schema text) is
cached for 24 h, up to 256 entries, so the activity and log detectors share
one call and a schema change asks again. Schema context is read through the
bounded catalog reader with `to_regclass(format('%I.%I', ...))`, so
mixed-case names resolve.

Model text is shown as model output and is never `recommended_sql`: only a
rule's deterministic template (`RecommendedSQLSource = deterministic`) is,
and only as one statement the executor runs alone. A model script that
pg_sage validates is `detail.llm_script` (the fallback's alternative is
`detail.llm_safe_alternative`); one it cannot validate, including one that
runs `CONCURRENTLY` inside `BEGIN`/`COMMIT`, is `detail.llm_unvalidated_text`
with the reason in `detail.llm_script_invalid`. A re-detection keeps these
`llm_` keys. RCA Tier 2 diagnostic SQL is kept only once proven read-only
(see [Model-authored SQL](security.md#model-authored-sql)).

Incident playbooks follow the same typed-action model. Read-only diagnostics
can inspect blocker graphs, runaway queries, connection pressure,
WAL/replication state, standby conflicts, vacuum pressure, and freeze
blockers. Each diagnostic proposes the SQL of the Sage SRE catalog probes
for its family (lock_graph; long_transactions and temp_file_holders;
connection_saturation; replication_slots and replication_lag;
standby_replay_state; xmin_horizon and wraparound_tables; stat_tables,
xmin_horizon and vacuum_progress), rendered with the probe's row cap as a
literal LIMIT and its `/* pg_sage sre:<probe> <version> */` comment, so it is
bounded and scoped to `current_database()`. Run it read-only with a
statement_timeout; temp_file_holders reads `pg_ls_tmpdir()`, which needs
`pg_monitor`. The playbook is chosen from the incident's signal ids through one
table that records a decision, "none" included, for every signal RCA, the log
classifier and the SRE detector emit; the root-cause wording never selects
one. Analyzer and executor findings route the same way: replication lag and
inactive slots get the read-only WAL diagnostic (never a slot drop), a
sequence near its limit gets the reviewed migration script, and a runaway
query gets the diagnostic while its cancel stays with the executor's runaway
policy. Backend
cancel/terminate candidates come only from the structured blocker identity
RCA records on a lock_contention incident (pid, backend_start, query_start
and a hash of the first 200 characters of the query), never from a PID in
incident text, and only when the incident has exactly one root blocker.
They are script only: the script re-checks that identity, the session state,
`datname = current_database()` and the protected-session guards the executor
uses, so it signals zero sessions once the backend has moved on. An active
blocker gets a cancel (moderate) and, on a critical incident, a terminate
(high); an idle-in-transaction blocker gets a terminate only, because
`pg_cancel_backend` does not end an idle session. Idle-in-transaction
saturation, runaway-query and older incidents without that identity show the
diagnostic and a blocked cancel. Sequence
capacity changes are treated as forward-fix migrations with script and
verification output rather than autonomous DDL.

Vacuum, bloat, and freeze autopilot turns maintenance findings into bounded
actions. The analyzer's `table_bloat` rule proposes a guarded `VACUUM` (none
while IO is saturated) and its `autovacuum_tuning` rule a per-table scale
factor; its wraparound rules only diagnose. The freeze custodian is the single
producer of freeze actions. On each database's autonomy cycle (every
`analyzer.interval_seconds`, at least a minute apart) it answers a red table
with a ladder: a response to the xmin holder that pins it (a session signal
that always waits for approval, or a parked plan), then `VACUUM (FREEZE)`, a
parked plan for red system catalogs, and reviewed `pg_repack` plans for large
bloated heaps instead of `VACUUM FULL`. Concurrent reindex handles index-bloat
remediation. See
[Vacuum, bloat and freeze autopilot](configuration.md#vacuum-bloat-and-freeze-autopilot).

Query tuning has multiple paths. `pg_hint_plan` remains useful for reversible
planner experiments, but pg_sage can also generate application-query rewrite
artifacts, retire broken hint experiments, recommend `CREATE STATISTICS`, draft
parameterization changes, and promote repeated per-role `work_mem` patterns
into reviewed role settings. Those actions are modeled as typed cases with
rollback class, verification steps, and PR/script output so they can move
through change control like schema work.

The executor checks: trust level, trust ramp, per-tier toggles, maintenance window, emergency stop flag, and replica status before acting.

#### Plans

A plan (`internal/changeplan`) is durable, multi-step work in which every step
is an ordinary typed action. `sage.plan` holds the goal, scope, origin
(generator, model, person or agent), the evidence pack it was built from and
its status; `sage.plan_step` holds each step: its action type and SQL, its
rollback, the finding or pack evidence ids it cites, its target query ids and
metric, the expected direction and size and where that size comes from, its
guardrails, minimum evidence, deadline, checkpoint flag and one outcome action
per verdict (keep, revert this step, revert to the checkpoint, or halt and
ask; an undeclared unverifiable step and an expired monitor halt and ask).
`sage.plan_step_edge` says which step must finish before which, and
`sage.plan_approval` records who approved which plan hash, with at most one
live approval per plan. Plans live in the monitored database's `sage` schema,
beside `sage.action_queue`; the link from a step to the ledger (queue item,
action log row, decision) is held on the plan side, so no chained table
changes.

A plan is refused unless it validates: its edges form a DAG over its own
steps, each action step's type has an executor contract, its SQL passes the
executor's allowlist, it carries a rollback exactly when the contract is
reversible, a check step (which changes nothing) carries no SQL, every executable
step cites a finding that exists or evidence ids
in the plan's pack, and a step whose targets ran fewer than its minimum calls
in the last 14 days declares itself unverifiable. The plan hash (`plan-v1`,
SHA-256 over a NUL-separated canonical encoding of the goal, scope, origin,
pack hash, every step's hashed fields in ordinal order and the sorted edges;
the runtime columns are outside it) binds an approval to what runs, as the
card hash does for one action. Editing a step or the edges voids a live
approval in the same transaction and returns the plan to `proposed`; an
approval at a hash the rows no longer have is refused; and `Verify`
recomputes the hash from the stored rows, so a row edited by direct SQL
reports `hash_mismatch`. Terminal plans age out with
`retention.actions_days`; open plans are kept, and so is a workload pack an
open plan, or a terminal plan younger than `actions_days`, cites.

The plan engine (`changeplan.Engine`) runs approved plans, one step at a
time, through the executor. Each database's runtime ticks it after the
executor cycle, on a writable primary and only on the fleet leader. A tick
advances each approved or running plan by at most one decision. The tick holds
an advisory lock and the plan row (`FOR UPDATE SKIP LOCKED`), so two sidecars
never move one plan and never start conflicting steps at once.

- **When a step may start.** Its predecessors must be kept, no other step of
  the plan may be in flight, and the plan's rows must still hash to the live
  approval. If they do not, the plan is voided with `hash_mismatch`.
- **Waiting on other changes.** A step waits while a step of another plan on
  a shared target query, or any setting step, is in flight. A setting step
  (`ALTER SYSTEM`, or `ALTER DATABASE`, `ROLE` or `USER ... SET`) waits until
  no plan step and no executor change is in flight. The executor's rule of one
  change per object also applies to every step.
- **Plan caps and peak windows.** The standing policy's `plans` block bounds
  the indexes per table, the bytes of new indexes and the restart-bound steps
  of the running plans, and names peak windows (docs/policy-gate.md). A plan
  over a cap is refused when it is proposed and when it is approved. A step
  that would cross a cap, or start inside a peak window, waits. If it waits
  past its deadline it ends `expired`.
- **Steps that are never automatic.** Approving the plan never covers some
  steps: a rewrite, dropped data, a type change, a restart-bound setting, a
  revoke, a topology or pooler change, or a contract that cannot be undone.
  The engine stops before each one with `awaiting_approval`.
- **Approval modes.** A plan is approved `all` (every step except the
  never-automatic ones), `up_to` N (ordinals 1..N except those; N equal to
  the step count is stored as `all`) or `step` (no step by itself). A step
  starts only when the plan's live approval covers it or the step has its own
  approval (`sage.plan_step_approval`) at the current plan hash; otherwise it
  stops with `awaiting_approval`, and its approval sets it back to pending and
  runs it as that approver. An edit voids every approval of the plan, step
  approvals included. Each approval records where the person decided (`ui`,
  `api`, `chat` or `cli`).
- **Starting a step.** The engine writes `starting` first. It then queues the
  item `plan_step:<plan>:<step>` (proposed by `plan:<id>`), approves it as the
  plan's approver with exactly the step's SQL, and calls `RunApprovedAction`.
  The gate re-authorizes the step and it runs through Apply.
- **The plan runner.** It is one of the executor's approved-action runners,
  each owning disjoint identity-key prefixes. It runs a plan step only when its
  own engine hands it over for a queued, claimed step with that item and SQL.
  Approving the item on the Actions page or through the scheduled runner runs
  nothing, and no approval card is sent for it.
- **Waiting for the verdict.** After the run the step is `judging`. It waits
  for the verifier's outcome (`neutral` becomes `no_change` and
  `insufficient_evidence` becomes `inconclusive`), a rollback by the verifier,
  an expired monitor, or the step's deadline. An expired monitor queues no
  `verify.on_expired` revert item, because the step's `on_expired` decides.
  A verdict decided after the deadline counts as `expired`.
- **Acting on the verdict.** The engine then carries out `keep`,
  `revert_step`, `revert_to_checkpoint` or `halt_ask`. A step the verifier
  already reverted is recorded `reverted_by_verifier` and never rolled back
  again. A step after a reverted step is skipped (`predecessor_reverted`).
- **Reverting.** Every revert goes through `RollbackAction`, which runs the
  stored rollback SQL under the DDL slot and the lock timeout (an index
  replace goes to `RollbackIndexReplace`). The engine then reads the revert
  back: the index is gone, or restored and valid, or the setting is back at
  the prior value the executor recorded. A revert that does not read back
  fails the step and halts the plan. A rollback that this check cannot read
  back fails the same way, and so does one that names another index or
  setting than the one the action changed. An action someone already rolled back outside the
  plan is not rolled back again. Each revert is a `step_reverted` event that
  carries the action_log id.
- **Reverting to a checkpoint.** `revert_to_checkpoint` reverts the failed
  step. It then walks back to the latest step before it marked
  `checkpoint`, or to the plan's start if none is marked, and reverts kept
  steps newest first. The walk never reverts a step that a kept step
  outside the walk depends on; it stops there. A step whose action needs no
  rollback (`ANALYZE`, `VACUUM`) stays kept with `no_rollback_needed`. Any
  other class of action (`forward_fix_only`, `not_reversible`, an operator's
  step) stops the walk. The plan then halts, and the halted event lists what
  was reverted and what was kept, and why. The walk resumes after a restart,
  and the emergency stop holds it between steps. Validation refuses a step
  whose checkpoint its edges make run after it (`checkpoint_not_before`).
- **Halting.** A failed or regressed step halts the plan, with one `halted`
  event and one notification (an `approval_needed` event naming the plan, the
  step and the verdict). A halted plan starts nothing until a person resumes
  or cancels it. Resuming (at the approved hash) marks each step that halted
  it `resumed_at`, so it no longer halts the plan: a failed step becomes
  `skipped` (its dependents are skipped) and a halted step `kept` (its change
  stays in place). `Store.ListNeedingPerson` lists proposed and halted plans, and steps
  awaiting their operator or their own approval.
- **Restart and leader change (plan-02e).** The first tick after a start, and
  after gaining fleet leadership, resumes each step left in flight before
  anything else moves. It reads what may have happened, as the index replace
  does, and never guesses. A step's start records the ledger's highest
  action id (`action_log_floor` on its `step_started` event), so a run of
  the step is a later `sage.action_log` row with its SQL that no step has
  recorded. `starting` with no queue item starts again, and with its own
  pending item adopts that item. `queued` runs its item through
  `RunApprovedAction` only while the ledger holds no run of its SQL.
  `running` with its one ledger row, and `executed`, go on to `judging`
  (a failed run fails the step). `reverting` reads its revert back and
  finishes or fails. Anything else (`running` with no ledger row, two
  candidate rows, an item in an unexpected status, a state the table does
  not know) halts the step and the plan with `interrupted_unknown` and one
  notification: a person reads it. A read that fails leaves the step as it
  was, and resume runs again on the next tick. Step claims are the fleet
  leader's holder id with the elector's lease
  (`fleet_learning.leader_lease_seconds`, 30 s by default). A new leader
  takes over a step only after the old claim expired, and every engine
  transition refuses a step another engine holds, so an old leader that
  wakes up moves nothing.
- **Emergency stop.** While the stop is set nothing moves, and running plans
  show `pause_reason` "emergency stop".
- **Steps without a finding.** A queue item is approvable only for an open
  finding, so a step citing only pack evidence fails at its approval and
  halts the plan.
- **Operator steps ("you run this").** On a managed platform pg_sage never
  applies a parameter-group or flag change, so a plan can hold an `operator`
  step instead. It carries the command for the operator (copied from a managed
  change proposal, linked by `managed_change_id`, or free text), an optional
  revert command, and a landing check: a setting and the value `pg_settings`
  (or `SHOW`) must read with no restart pending, the linked proposal reaching
  `applied`, or both. pg_sage runs nothing for it. It runs alone like a
  setting step and never inside a peak window. The engine moves it to
  `awaiting_operator` and sends one notification that says what to run.
  It reads the landing each tick. A failed read is recorded on the step and
  the step keeps waiting; it never counts as landed. Once the check passes, the
  step is `judging` from `landed_at`. A readback step is decided by its
  landing. A measured step gets one look, a verification window
  (`verify.window_minutes`) after landing, by verification v2's interrupted
  time series with no control group (a global setting moves every query), and
  its verdict says so. A step not landed by its deadline (counted from the
  ask) ends `expired`. pg_sage cannot revert an operator step: validation
  refuses `revert_step` and `revert_to_checkpoint` on it
  (`operator_not_revertible`), a regression halts and asks with the operator's
  revert command in the event and the notification, and a checkpoint walk
  stops at it. A restart-bound operator step counts against
  `max_restart_bound_steps`.

The engine runs only plans a person approved, so it is on with no
configuration.

**Plan API and CLI (plan-03a).** Each plan lives in its monitored database, so
`?database=` names it (optional with one database), as for approval cards:

| Route | Who | Body | Does |
|---|---|---|---|
| `GET /api/v1/plans` | any role | `?status=`, `?limit=` (1-500) | Plans of every visible database, newest first. |
| `GET /api/v1/plans/{id}` | any role | | The plan, steps, edges, hash, approvals, step approvals and each step's coverage. |
| `POST /api/v1/plans/{id}/approve` | operator | `{plan_hash, mode, up_to}` | Approve all, up to step N, or step by step. |
| `POST /api/v1/plans/{id}/steps/{step}/approve` | operator | `{plan_hash}` | Approve one step (by step id). |
| `POST /api/v1/plans/{id}/reject` | operator | `{reason}` | End a proposed plan as rejected. |
| `POST /api/v1/plans/{id}/resume` | operator | `{plan_hash}` | Run a halted plan again. |
| `POST /api/v1/plans/{id}/cancel` | operator | `{reason}` (optional) | End an approved or halted plan, or a running one with no step in flight (one stopped before a step awaiting its approval or its operator). |

A hash that is no longer the plan's is 409 `content_changed`; a decision the
plan's status does not allow is 409 (`not_pending`, `not_halted`,
`not_cancellable`, `step_not_pending`); bad input is 400 with its code
(`invalid_mode`, `invalid_up_to`, `plan_hash_required`, `reason_required`,
`step_not_in_plan`, `invalid_via`). Outside a Databricks App each route also
takes an operator API token as `Authorization: Bearer`: the read scope reads,
the approve scope decides, an agent token is refused whatever scopes it lists,
a session cookie beside the token is ignored, and the decision is the token
owner's, with their current role and database grants.

`pg_sage_sidecar plan list|show|approve|reject|resume|cancel` uses these
routes with `SAGE_API_URL` (https, or http to localhost only) and
`SAGE_API_TOKEN` (or `SAGE_API_TOKEN_FILE`): `plan approve <id> --hash <h>
--all | --up-to N | --step N`, and `--json` prints the answer as JSON. It
exits 0 when decided, 1 when the sidecar refused (stale hash, not pending, a
server error), 2 on a usage error and 3 when the token was refused.

**Plan review in the dashboard (ux-03).** Actions has a **Plans** tab, first in
its tab bar, listing the plans of the selected databases;
`#/actions?tab=plans&plan=<id>&database=<db>` opens one plan's review (the
Inbox's `plan_approval` item and a case link there; `plan=<id>` alone also
opens the Plans tab). The header shows the
goal, scope, expected effect of each stage, risk (which stages always need
their own approval), origin (generator, model, person or agent, with the
model and who made it) and the evidence pack hash. Each stage, in ordinal
order, shows its status, evidence ids (linking to the outcome ledger
filtered by that evidence), the exact SQL as text, the expected effect and
the ladder tier it was tested at (the ledger row's highest passed tier once
it ran, else its expected source), the measured effect and verdict from the
outcome ledger row of its action (`not measured yet` until there is one,
`could not be read` with the error when the ledger read failed),
the gate to the next stage (automatic on the verdict, or a person when the
next stage needs its own approval) and its rollback with the stages that
depend on it, which reverting it would also revert. An operator stage reads
"You run this" with the exact command and pg_sage's observed landing, and
offers no rollback.

A proposed plan offers Approve all, Approve up to stage N (only stages
before the first never-automatic one; up to the last stage is all) and Step
by step, each sent with the plan hash shown; editing is not offered. A
stale hash (409 `content_changed`) reads "This plan changed after approval;
approval voided" and reloads the plan; a plan whose approvals are all voided
or at another hash says the same. After a step-by-step approval each stage
not yet covered offers its own Approve. A halted plan shows the stage it
halted at and why (after a revert to a checkpoint, the stage that
regressed, not the earlier stages the walk reverted), and which stages were
reverted, kept and never started.
Viewers see plans; operators and admins decide them.

A finding case whose finding is a step of a live plan (not draft, rejected,
voided or cancelled) carries `plans: [{id, goal}]` in `GET /api/v1/cases`
(at most five, newest first) and shows "Part of plan: <goal>" linking to the
plan's review; `plans_unavailable: true` says the plans could not be read.

### API + Dashboard (Web UI)

REST API and embedded React SPA on `:8080` (configurable). The v1 UI is
organized around Overview, Cases, Actions, Fleet, and Settings. The legacy
`#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and
`#/incidents` routes open Cases with the appropriate source context. The API
includes case projection, shadow decisions, action queue, provider readiness,
findings, snapshots, config, forecasts, query hints, alerts, emergency stop,
and fleet management. UI and `/api/v1/*` routes are session-authenticated.

**Cases ranking and urgency.** Cases from every database are ranked by
severity first; no warning ever outranks a critical. Within a severity, a case
whose producer reports its limit already reached (a zero `days_until_full`,
`days_to_warn_pct` or `seconds_to_limit`) comes first, still showing no number;
then a case with an assessed time to impact, the nearest impact first, then
cases without one, then the most recent observation, then database and id.
Each case carries `rank_basis`, for example "severity warning; time to impact
about 3 days". The time to impact (`urgency`) is read only from the fields the
producer wrote, never computed by Cases: `days_until_full` with `r_squared` and
`data_points` (storage growth), `days_to_warn_pct` with `r_squared` (connection
saturation) and `seconds_to_limit` with `rate_per_s`, `samples`, `span_s` and
`r2` (disk, WAL slot, sequence and wraparound runways). The case shows the
formula as its basis sentence, for example "About 3 days until the disk is
full (seconds_to_limit 259200 from 48 samples over 6 h, R² 0.97)". A missing,
non-numeric, non-finite or non-positive value, a zero rate, or a storage or
connection regression fit below R² 0.5 leaves the case "Urgency not assessed";
it never shows a reassuring number. XID wraparound and sequence exhaustion
findings that carry no rate show their headroom instead ("xid_age 1.2B of
the 1.6B critical threshold") and stay unassessed. A recurring incident (one with
`previous_incident_id`) carries `recurrence`: the previous incident, the
number of occurrences in its chain and when the previous one was resolved,
read in one batched lookup per database, shown as "4th occurrence; previous
resolved 2026-11-02". When that lookup fails the case says the history is
unavailable rather than calling it a first occurrence.

**Cases and the recommendation lifecycle.** A finding case carries
`recommendation`, the newest `sage.recommendation` head of its finding (read
in one query per database over the `recommendation_finding` index): its id,
state, revision, verdict, reason, attempt count, `next_attempt_at`, when it
was applied, `current_content` (the head carries the content hash the case
proposes now), a one-line `next_step` and `url`
(`/api/v1/recommendations/{id}?database=...`). The case's next step follows
the head: "pg_sage applied this on 2026-11-10 and reverted it: <reason>",
"Stopped after 3 failed attempts", "Retrying after 14:05 UTC" and so on. A
reverted or abandoned head of the same content blocks the candidate as
`recommendation_reverted` or `recommendation_abandoned` (the store never
re-proposes that content); a failed head in backoff sets the candidate's
`retrying_after`. A head of other content blocks nothing and says the current
proposal differs. When the heads cannot be read, every finding case of that
database is marked `recommendation_unknown`, never "no recommendation", and
the response is still 200. A candidate past its `expires_at` is blocked as
`evidence_expired` ("Evidence expired; waiting for a fresh observation"),
replacing any other restriction except a recommendation refusal; the policy's
own reason stays in `policy_decision`. Each candidate shows how long its
evidence is valid. A head state the dashboard does not know is shown as
"state unknown", never as an attached change.

**Header and context strip.** The header says which database you are looking
at, its mode and what pg_sage may do there right now. With one database
selected, the trust badge shows its level, and its tooltip lists the action
families that run without approval now ("Auto-executes now: analyze_table"),
or "Nothing auto-executes right now". The list comes from each family's
policy decision, which the server takes from the executor's gate on every
status read, so it already accounts for the trust ramp, a disabled executor,
replicas and the emergency stop. A database that is emergency-stopped never
lists a family. In the all-databases view, a fleet on one level shows that
level, and a fleet on several shows every level in use, for example
"Mixed · 9 autonomous · 1 observation" (a database that has not reported a
level counts as "unknown", never as autonomous). The tooltip then names, per
family, the databases that run it unattended. The context strip under the
header repeats this in words ("Can run without approval now: create_index,
analyze_table. Everything else queues for approval or is blocked by policy.")
and, in the all-databases view, says how many databases are emergency-stopped.
While some databases have not reported their action policy, the strip and the
fleet tooltip add "2 databases have not reported their action policy" and the
strip limits its claim to the databases that did, since one that has not
reported may still auto-execute.

**Approval cards.** Each pending action in "Needs your review" is a card
(`GET /api/v1/approvals`): why it needs a person, the cited evidence, the
rationale, the predicted effect, the exact SQL and rollback, and the risk. A
`pg_hint_plan` card (`apply_query_hint`, `retire_query_hint`) also carries
`hint_evidence`, the plan comparison that gated the hint, with the finding's key
names: "Plan comparison: changed, cost 412.0 to 254.6 (-38.2%)", the reason when
the verdict is not `changed`, and the plans without and with the hint in
collapsed blocks (see [hint verification](hint-verification.md)). A stored plan
over 64 KB is left out and named in `plain_plan_omitted` or
`hinted_plan_omitted`; a malformed `hint_evidence` shows no section and logs one
WARN with the finding id. The section is read-only: the card hash, which binds
an approval, and the chat and Slack card text do not change.

The pending-approvals badge counts the selected database, or the whole fleet
in the all-databases view, the same scope as the Actions list. When a
database could not be counted (not connected, or its read failed) the badge
shows "3+" and its tooltip names the databases left out.

**Data freshness.** The header shows how current the page is. "Live" means the
`/api/v1/events` stream is open and pages refresh as changes are pushed.
"Polling, updated 40 s ago" means the stream is not live yet, or dropped; pages
keep their normal 30-second polling and the age is that of the last good fleet
poll. "Offline, data from 14:02" means three reconnects in a row failed; the
data on screen is from that time. Each tab keeps one stream for every database,
also in fleet mode. The server ends each stream after 45 seconds and the browser
reconnects on its own. When a reconnect is refused (for example a proxy 502 or
504 while the sidecar restarts) the browser gives up, so the dashboard opens a
new stream after 3 s, doubling up to 60 s with 20% jitter, and goes back to
"Live" when it connects, without a page reload.

**Background tabs and outages.** A dashboard tab that is not shown sends no
polls: each page fetches once when the tab is shown again, so the
pending-approvals badge in a background tab catches up when you return to it
(notification channels still carry urgent approvals). While an endpoint keeps
failing, its polls slow down to 2x, 4x and so on of the normal interval, up to
5 minutes, and return to normal after one success; Retry buttons still fetch at
once. Live events refresh a page only for the database it shows (or for all of
them in the all-databases view), and a burst of events refreshes it once, 2 s
after the last one. The "Getting started" checklist polls every 10 s until setup is
complete, then every 60 s.

Provider readiness uses adapters for Postgres, Cloud SQL, AlloyDB, RDS, and
Aurora. Adapters describe extension enablement paths, log access, managed
service limitations, and supported action families before policy evaluation
decides whether a specific database can execute, queue, or only observe.

### Alerting

pg_sage delivers notifications through one engine, the notification outbox (alerting-06b).
Channels and rules come from two places, both stored in `sage.notification_channels` and
`sage.notification_rules` of the control database: the Notifications page (`managed_by =
api`) and the config file's `alerting:` block, which the alerting owner reconciles into
`config:<kind>:<name>` channels and `finding_opened`/`finding_resolved` rules (`managed_by =
config`) at startup and on every live change ([alerting](configuration.md#alerting)). Each
finding therefore posts once per channel and pages PagerDuty under one key, with the outbox's
retries, cooldown, quiet hours and reminders. Config rows show a "From config file" badge and
the API refuses to change them (409 `managed_by_config`).

`alerting.engine: legacy` keeps the old per-database alerting manager (below) for one
release instead; at startup it removes the config rows a notify run left, so the block is
delivered once, and it posts a second time to any destination the Notifications page routes to, because the two stacks share no state and their dedup keys
differ. It is removed by alerting-06d.

| | Notify engine (default) | Legacy engine (`engine: legacy`) |
|---|---|---|
| Configured in | the Notifications page and the reconciled `alerting:` block | the config file's `alerting:` block |
| Destinations | Slack, PagerDuty, email, Telegram, webhooks, Microsoft Teams, Opsgenie, incident.io | Slack, PagerDuty, webhooks |
| Triggers | the event types below, each rule with a `min_severity` | every new finding at or above the route's severity, polled per database |
| Retries | queued in `sage.notification_outbox`, up to 6 attempts | up to 3 attempts within one check |
| Cooldown, quiet hours, reminders | the `alerting.*` values, for the finding events `finding_critical`, `finding_opened` and `finding_resolved` ([notification delivery](configuration.md#notification-delivery)) | `cooldown_minutes`, `quiet_hours_*`, `repeat_minutes` |
| Log | `sage.notification_log`, the Notifications page's Log tab | `sage.alert_log` of each database, `GET /api/v1/alert-log` (deprecated) |

**One notification log.** Every delivery the notify engine makes is a row of
`sage.notification_log` naming the database it was for and the event's dedup key; a
fleet-wide event, and every row written before the upgrade that added the column, has no
database and shows as `unknown`.
The Notifications page's Log tab, open to admins and operators, lists them for the database
selected in the header (all databases when none is), through
`GET /api/v1/notifications/log?database=<name>`. The old Alert log is folded into it: the
`#/alerts` dashboard URL redirects to the Log tab, keeping `?database=`, and
`GET /api/v1/alert-log` still serves `sage.alert_log` history (rows age out as before) with
`Deprecation: true` and a `Link` to its successor. Channels and rules stay admin-only.

**Cutover.** After the switch to the notify engine, each database runtime resolves the
PagerDuty pages the legacy engine opened on it (newest `sage.alert_log` row `sent` on the
`pagerduty` channel, no resolve since) through the reconciled PagerDuty channel, under the
legacy key `<database>:<category>:<object>`, once the finding resolves. Each attempt is
logged in `sage.alert_log` (`resolved` or `resolve_error`, at most 5 failures per finding);
the sweep runs every minute and stops when no legacy page is left.

#### Notifications (channels and rules)

Routes events to the channels of the matching rules. A `webhook` channel posts each event as
JSON (`event`, `severity`, `subject`, `body`, `database`, `dedup_key`, `resolve`, `data`,
`sent_at`) to its `webhook_url`, with its optional `headers`, a JSON object of header names to
values (`Host` cannot be set). Both are sealed like every channel secret, and the URL passes
the notification target policy when the channel is saved and again at connect time. Event types: `finding_opened` and
`finding_resolved` (any finding that opens, reopens or escalates, and its resolution, at the
finding's own severity, with the dedup key `finding:<database>:<category>:<object>`),
`finding_critical`, `action_executed`, `action_failed`, `approval_needed`, `query_rewrite_suggested`, `incident_detected`, `incident_escalated`, `incident_resolved`, `custodian_escalation` (critical: a red custodian proposal exhausted its failure backoff, raised once per failure streak and recorded as a `custodian_backoff` decision), `fact_expired` (a confirmed binding fact expired and no longer binds; see [facts](facts.md#expiry-and-re-verification)), `canary_interrupted` (warning: a sidecar restart interrupted a fleet canary, which is not resumed; see [configuration](configuration.md)), `digest_ready` (info: the scheduled digest of what pg_sage did, verified, saved, what needs a person and what it held back, sent at once and only when it changed; see [configuration](configuration.md#yaml-config-file)). A Sage SRE diagnosis is not a rule event: `investigation_concluded` goes only to the channels an incident's page reached, once per incident (threaded under the Telegram page, never to PagerDuty; see [configuration](configuration.md#sage-sre-investigations)).

**Destinations.** Every channel type is a `notify.Sender`, registered once in
`notify.NewDefaultDispatcher`, and delivered through the outbox with its retries:

| Type | Protocol | Open and resolve |
|---|---|---|
| `slack`, `telegram`, `email` | incoming webhook, Bot API, SMTP | a resolve is an ordinary message |
| `webhook` | JSON post | `resolve: true` with the same `dedup_key` |
| `pagerduty` | Events API v2 | trigger and resolve on the dedup key |
| `teams` | Workflows webhook, Adaptive Card | a resolve is a green "Resolved:" card |
| `opsgenie` | Alert API v2 (`us` or `eu`) | create with `alias` = dedup key, close by alias |
| `incidentio` | HTTP alert source | `status` `firing`, then `resolved`, on one `deduplication_key` |

Jira is not a destination but a case link: a `jira` channel holds the sealed credentials,
`sage.case_tickets` links one case to one issue, and a bounded worker reads the issue status
back as evidence ([case tickets](configuration.md#case-tickets-jira)).

A `Retry-After` on a 429 or 5xx answer holds the next attempt back by that long, at most an
hour ([configuration](configuration.md#notification-channel-types)).

**PagerDuty lifecycle.** Each pg_sage incident is one PagerDuty incident with the
`dedup_key` `incident:<database>:<incident id>`. `incident_detected` and
`incident_escalated` trigger it (an escalation joins the open PagerDuty incident instead of
opening a second one) and `incident_resolved`
resolves it, so a PagerDuty channel must have rules for all three events; without an
`incident_resolved` rule the page stays open. A recurrence is a new incident id and opens a
new PagerDuty incident. The resolution of an incident last seen longer ago
than `rca.stale_after_hours` sends no notification, so close its PagerDuty incident by hand. A
key longer than PagerDuty's 255-character limit is shortened to its head plus a hash, so
the trigger and the resolve still match. A critical finding pages with the key
`finding_critical:<database>:<category>:<object>`. When the analyzer resolves the finding (it
is no longer detected), it sends a resolve on that key as a `finding_critical` event, so the
rule that paged also closes the page; a re-opened finding pages again. Slack, Telegram and
webhooks receive the resolves as ordinary messages ("Incident resolved: ...",
"Resolved: Critical finding: ..."). The channel's `dedup_key_prefix`
is still prepended to every key. PagerDuty incidents opened before v2.5.1 used other keys and
do not auto-resolve: close them by hand once after the upgrade.

#### Legacy alerting engine (`alerting.engine: legacy`)

Polls each monitored database for new findings and routes them by severity to Slack,
PagerDuty and webhooks, with cooldown, quiet hours (a deferred alert is delivered when they
end) and reminders for findings that stay open.

**Alerting manager keys.** The `alerting.*` manager runs once per monitored database and
names that database in every Slack header, PagerDuty `source`/`component` and webhook
`database` field. Its PagerDuty `dedup_key` is `<database>:<category>:<object>`, for the
trigger and the resolve alike, so identical schemas in a fleet (shards, tenants, staging
and prod) that share a routing key get one PagerDuty incident each, and one database's
resolve never closes another's page. The database name is kept verbatim (a `:` inside it
is fine; keys are opaque), and a key over 255 characters is shortened to its head plus a
hash. Without a database name the key stays `<category>:<object>`. Cooldown, quiet hours
and `sage.alert_log.dedup_key` (`<category>:<object>`) are unchanged: each database keeps
its own `sage` schema and its own throttle. Alerts open at upgrade time used the old key,
so they re-page once under the new key (standalone included) and the old PagerDuty
incident must be closed by hand.

### AutoExplain Collector

Stores plans for slow statements in `sage.explain_cache`, each with its true
`source`:

- `explain_estimate`: an on-demand plain `EXPLAIN` (never `ANALYZE`) in a
  read-only, rolled-back transaction bounded by a 5 s statement timeout and
  the probe lock timeout. It is an estimate, with no execution time.
  Parameterized statements are planned with `GENERIC_PLAN` on PostgreSQL 16+
  (`generic_plan`) and skipped on older servers. A statement that already has
  a plan from the last day is not explained again.
- `auto_explain_log`: the plan of a statement that ran, as `auto_explain`
  wrote it to the server log (with actual rows and timing only when
  `auto_explain.log_analyze` is on, which adds overhead to every statement), where pg_sage reads that log: the
  Supabase log sink, and on self-managed servers the cluster's log watcher
  (`logwatch.enabled` auto or true, with RCA or migration log detection on), where each
  database subscribes to its own records. A plan needs
  `auto_explain.log_format = 'json'` and a query identifier
  (`compute_query_id = on` and `auto_explain.log_verbose = on`: only verbose
  output prints it). The log watcher stores at most
  `auto_explain.max_plans_per_cycle` plans per poll and keeps plan records of
  up to 1 MiB. Records
  that are not stored are counted by reason (`invalid_json_plan`,
  `missing_query_identifier`, `pg_sage_self`, `other_database`,
  `over_cycle_cap`, `store_failed`) and logged with the setting to change.
- `lakebase_plan_history`: Lakebase Advanced Telemetry plans, kept apart.

Readers that choose between cached plans prefer an executed plan over a newer
estimate. Rows stored as `auto_explain` by versions before rca-04 were
estimates and are renamed to `explain_estimate` on upgrade.

### Forecaster

Analyzes historical trends to predict disk growth, connection exhaustion, sequence depletion, and cache ratio degradation. Generates proactive findings before problems occur.

The runway monitor (`internal/runway`) samples the series a hard limit is approached along
(XID and multixact age, WAL, slots, disk, sequences) on the collector tick and projects each
against its limit. In fleet mode a process-wide `runway.ClusterOwners` elects, per cluster
(system identifier and postmaster start), the one database runtime that reports the
cluster-scope series; the others sample them too but report only their own tables and
sequences, and close their copies once the owner has reported. The forecaster's connection
and checkpoint forecasts use the same election per system identifier and address. See
[Cluster runways and forecasts](walkthrough-fleet.md#cluster-runways-and-forecasts).

### Tuner

Per-query optimization via `pg_hint_plan` (if available). Detects plan-level symptoms (disk sorts, hash spills, bad joins) and applies per-query GUC overrides without modifying application queries. The tuning agent's hints go through it: it validates the pg_hint_plan syntax, clamps `Set(work_mem)`, records the hint and keeps one hint per statement.

### Retention

Automatic cleanup of aged snapshots, findings, actions, and explain plans based on configurable retention windows.

### History store

Telemetry history (`sage.snapshots`, `sage.query_store`) lives in each monitored
database by default. With `history.store: meta` (meta-db mode) it lives in the
metadata database, each row tagged with the database's meta-db record id.

- Every history statement is written once with scope markers (`{db:alias}`,
  `{dbcol}`, `{dbval}`) and runs through `internal/histstore`, which binds them for
  the database's store. A marker is not SQL, so a statement sent without a store
  fails instead of reading an empty or mixed history; a static test checks that every
  statement naming the two tables carries a marker.
- Each runtime resolves its store before it starts and registers it for its monitored
  pool; readers find the store from the pool they already hold (`histstore.Resolve`).
  With `history.store: meta` the process refuses a pool no runtime registered: every
  statement on it fails with `histstore.ErrNotRegistered` instead of reading that
  database's own (wrong) history, so no reader can be wired to the wrong database.
- Reads that joined history with the monitored database's catalog are split: the
  query store's plan fingerprints are read from the monitored `sage.explain_cache`
  and stored with the samples; verification reads history in the store and
  `pg_index` on the monitored database; the first-run check reads `sage.config` on the
  monitored database and the newest snapshot in the store.
- A runtime refuses to start when its history would be split (rows in the old place
  that the migration did not copy); `pg_sage history migrate` copies them.
- Retention of the store runs once per process; the snapshot cap is the sum of every
  registered database's cap.

### Prometheus Exporter

Metrics endpoint on `:8187`. Exports findings count by severity, the LLM circuit breaker
state (`pg_sage_llm_circuit_open`), connection stats, cache hit ratio, database size, and
LLM usage. Collector health is exported per database (label `database`) in every mode: the
collector circuit breaker state (`pg_sage_collector_breaker_state`, 0 closed, 1 skipping,
2 dormant), `pg_sage_collector_breaker_skips_total{reason}` (`load`, `load_query_error`),
`pg_sage_collector_last_success_timestamp`,
`pg_sage_collector_category_unavailable{category}`,
`pg_sage_collector_persist_failures_total` and
`pg_sage_collector_query_store_failures_total`. pg_sage's own database time per hour by
the component its statement tag names is
`pg_sage_self_component_db_time_ms_per_hour{database,component}` (see
[sage_self_cost](findings.md#footprint-api)). Every `pg_sage_self_*` family (what pg_sage
itself costs, and its budgets) is listed under
[pg_sage's own metrics](#pg_sages-own-metrics).

---

## Own Footprint and Self-Configuration

pg_sage runs on the database it guards, so it meters what it costs, holds itself to declared
budgets, and sizes its own cadences and deadlines from what it measures. Code:
`internal/selfmonitor` (tag and exclusion), `internal/selfcost` (meter),
`internal/selfbudget` (budgets and the process watch), `internal/selfconfig` (derived
settings).

### The statement tag

Every pool and connection pg_sage opens goes through `selfmonitor.ConfigurePool` (or
`ConfigureConn`): it sets `application_name` to `pg_sage` (`pg_sage action:<decision id>`
for an executor action's session) and wraps the network connection with
`selfmonitor.TagConn`. The wrapper rewrites the protocol's Query and Parse messages, so
every statement from every code path, pgx's own included, carries the tag:

- The tag sits right after the statement's first keyword (`SELECT /* pg_sage */ ...`),
  or after the opening parenthesis of a parenthesized query, because PostgreSQL 18's
  `pg_stat_statements` drops comments in front of the first token. Each statement of a
  multi-statement Query gets its own tag; a leading pg_sage comment is moved there.
- The tag names the component that sent the statement (`/* pg_sage collector */`,
  `/* pg_sage sre:probe v1 */`), so the meter can charge each component.
- pg_sage does not hide from `pg_stat_statements`: a DBA sees and prices its work there.

pg_sage's own readers leave that work out of the workload they analyze with one rule in
SQL and in Go. `selfmonitor.StatementExclusionSQL` keeps a statement whose text has no
`pg_sage` tag (`NOT ILIKE '%pg_sage%'`) and no reference to the `sage` schema
(`selfmonitor.IsStatementText` is the Go twin); `selfmonitor.ActivityExclusionSQL` keeps
a `pg_stat_activity` session whose `application_name` is not pg_sage's
(`selfmonitor.IsApplicationName`). Reading a statistics view is not pg_sage's signature:
another monitoring agent's `SELECT ... FROM pg_stat_statements` stays in the workload.
Findings about pg_sage's objects are left out of advice; the three findings about its
cost (`sage_self_cost`, `sage_self_budget`, `sage_footprint`) are always kept. See
[which statements and findings are pg_sage's own](configuration.md#collection-analysis).

### The meter, the budgets and the three findings

Each analyzer cycle reads the tagged `pg_stat_statements` entries of its database through
the bounded catalog reader (`safety.query_timeout_ms` and `safety.lock_timeout_ms`) and turns the
change since the previous cycle into a rate per cycle of that database's own collector
(`selfcost.Cost`): database time (execution plus planning), calls and shared blocks,
rows read and written in `sage` tables, the `sage` schema's size, and the time per
component the tag names. The first cycle after a start, a statistics reset or a missing
`pg_stat_statements` leave a figure unknown: an open finding stays open and a metric is
left out, never reported as `0`.

| Finding | Measures | Budget (default) | Unit |
| --- | --- | --- | --- |
| [`sage_self_cost`](findings.md#sage_self_cost) | database time of pg_sage's statements | `analyzer.self_cost_budget_ms` (3000, 5% of one core at a 60 s cycle) | ms per collector cycle |
| [`sage_self_budget`](findings.md#sage_self_budget) | database time | `self_budget.db_time_ms_per_hour` (0: inherit `analyzer.self_cost_budget_ms` at the database's collector period) | ms per hour |
| | shared blocks hit or read | `self_budget.blocks_per_hour` (18,000,000) | 8 KB blocks per hour |
| | size of the `sage` schema, plus the database's share of a separate history store | `self_budget.storage_mb` (10240) | MB |
| | sidecar process CPU, once for all databases | `self_budget.cpu_ms_per_cycle` (600, 1% of one core at 60 s) | ms per global collector cycle |
| [`sage_footprint`](findings.md#sage_footprint) | `sage` schema's share of the database | `retention.sage_size_warning_pct` (10, and at least 256 MB) | percent |

`0` disables a budget. `sage_self_budget` names the top consumers: the five costliest
statements of the window and the busiest loops (`component:database`, busy time without
LLM waits). `GET /api/v1/self-cost` serves the same footprint per database
([Footprint API](findings.md#footprint-api)) and **Settings > General > Footprint** shows
it.

### pg_sage's own metrics

`/metrics` exports the meter and the budgets per database (label `database`) unless
noted:

| Family | Meaning |
| --- | --- |
| `pg_sage_self_db_time_ms_per_cycle` | database time of pg_sage's statements per collector cycle (ms) |
| `pg_sage_self_statements_per_cycle` | statement executions per collector cycle |
| `pg_sage_self_blocks_per_cycle` | shared blocks hit or read per collector cycle |
| `pg_sage_self_rows_read_per_cycle` | rows read from `sage` tables per collector cycle |
| `pg_sage_self_rows_written_per_cycle` | rows inserted, updated or deleted in `sage` tables per collector cycle |
| `pg_sage_self_schema_bytes` | size of the `sage` schema (tables, TOAST, indexes) in bytes |
| `pg_sage_self_db_time_budget_ms` | `analyzer.self_cost_budget_ms`, no label (absent when `0`) |
| `pg_sage_self_db_time_ms_per_hour` | database time per hour (ms) |
| `pg_sage_self_blocks_per_hour` | shared blocks per hour |
| `pg_sage_self_component_db_time_ms_per_hour{component}` | database time per hour by the component the tag names |
| `pg_sage_self_cpu_ms_per_cycle` | sidecar CPU per global collector cycle (ms), no label |
| `pg_sage_self_budget{resource}` | declared `self_budget` per resource, no `database` label |
| `pg_sage_self_budget_database{resource}` | budget a database inherits (`db_time` at its own collector period) |
| `pg_sage_self_budget_exceeded{resource}` | 1 when pg_sage used more than its budget of the resource (`cpu` has no `database` label) |
| `pg_sage_self_loop_busy_seconds_total{loop}` | wall time of pg_sage's loops, LLM waits excluded (counter; `database` only on a per-database loop) |
| `pg_sage_self_loop_runs_total{loop}` | runs of pg_sage's periodic loops (counter; `database` only on a per-database loop) |
| `pg_sage_self_loop_errors_total{loop}` | runs that failed or produced nothing, such as a skipped or failed collection (counter, 0 included; `database` only on a per-database loop) |
| `pg_sage_self_loop_last_success_timestamp_seconds{loop}` | Unix time the loop's last successful run ended (gauge, absent until the first success; `database` only on a per-database loop) |

[sage_self_cost](findings.md#sage_self_cost) and
[sage_self_budget](findings.md#sage_self_budget) describe the windows and labels in full.

### Derived settings (self-configuration)

For a few `derivable` keys the operator leaves unset, pg_sage derives a value per database
(`internal/selfconfig`, run at startup before any worker reads the key and then every
hour):

- **Keys.** `collector.interval_seconds` (live: its reader re-reads it every cycle),
  `safety.query_timeout_ms`, `sre.runways.sequence_interval_seconds`,
  `sre.detectors.temp_file_mb` and `sre.detectors.lwlock_waiters` (restart-bound: a value
  promoted while running is reported as pending restart and applies at the next start).
- **Evidence.** The cost rules read the collector's measured statement time (p90 of its
  cycles' statement time and of their slowest statement, and the statements a deadline
  cut off), cited with source `collector`; before the collector's first cycle they read
  timed probes of the catalog and `pg_stat_statements`, cited with source `probe`. The
  other rules read the sequence scan time, the temp-file rate and `max_connections`.
- **Bounds.** A value never goes past the product default in the direction that spends
  more or widens authority, except where a fixed ceiling is recorded with its reason.
- **Shadow and soak.** A new value is recorded in shadow and soaks for
  `self_config.soak_hours` (default 24); it is promoted only when the comparison with the
  active value's measured outcome is not worse.
- **The operator wins.** A key set in YAML, a fleet field or an API override is never
  derived; an admin can pin or unpin a derived value. Every derivation, promotion, hold
  and pin is recorded in `sage.config_derivation`. `self_config.enabled: false` turns it
  off.

The rules, their bounds and evidence are generated in
[derived settings](generated/derived-settings.md); `GET /api/v1/derived-settings` and the
Configuration page show each database's values.

### The performance gate

`TestPerfGate` (build tag `perfgate`, `cmd/pg_sage_sidecar`) runs the real runtime, every
component once per cycle, against a large synthetic catalog with pre-seeded `sage`
history, and fails the build on any offender. Its limits live in
`internal/testsupport/perfgate/budgets.go`: (A) no sequential scan of a `sage` table over
5,000 rows; (B) a 100 ms mean per statement and 3000 ms of pg_sage database time per
cycle; (C) at most 250 rows written per `sage` table per cycle; (D) 500 ms for the slowest
catalog statement, and none cut off by a timeout; (E) 1000 ms per API list endpoint; (F) at
least 50% HOT updates on a `sage` table updated 5 times or more; (G) 600 ms of sidecar CPU
per cycle; (H) the product meter within 10% (at least 5 ms) of the database time the gate
measures, and its cycle within 10% of the collector period. Pull requests run the small
scale and the nightly run the large one (`PG_SAGE_PERF_SCALE`). `TestPerfGateFirstLook`
times the catalog-only first look.

---

## Data Flow

1. **Collector** gathers `pg_stat_statements`, `pg_stat_user_tables`, `pg_stat_user_indexes` every 60s into `sage.snapshots` (in the metadata database with `history.store: meta`).
2. **Analyzer** runs rules every 600s, then the **tuning agent** if an LLM is usable.
3. The agent classifies the workload, detects cases and asks the model about each case
   with read-only tools.
4. The model answers with typed proposals; pg_sage generates and validates the SQL
   (optimizer admission and HypoPG, configuration gates, tuner hint checks, facts).
5. Each admitted proposal gets a confidence calibrated on the outcome ledger.
6. Findings are persisted to `sage.findings`.
7. **Case projection** combines findings, incidents, and action state into DBA
   cases and shadow-mode proof.
8. **Executor** queues, blocks, approves, or executes typed actions based on
   trust level, policy, evidence freshness, maintenance window, and guardrails.

---

## Schema Bootstrap

On startup, pg_sage acquires advisory lock `710190109` (`hashtext('pg_sage')`), then creates the `sage` schema and tables if they do not exist. See [SQL Reference](sql-reference.md) for the full schema.

---

## C Extension (Removed)

The C extension, frozen at v0.6.0-rc3, was removed from the repository. Its last source is
preserved at the `c-extension-final` tag. The sidecar needs only the catalog views,
`pg_stat_statements`, and optional extensions such as HypoPG and pg_hint_plan.
