# Findings Catalog

pg_sage detects issues using deterministic rules (Tier 1) plus LLM-powered index optimization (Tier 2). Every category it stores, with its subsystem, producer, severities, config keys and executor contract tier, is in the [generated finding category catalog](generated/finding-catalog.md), built from the code's category registry; this page describes them. Each finding includes a severity level, human-readable title, detailed description, recommendation, and (where applicable) remediation SQL with rollback SQL.

Severities: **critical** > **warning** > **info**

**When a snapshot category could not be read.** A category the collector could not read in a
cycle is unknown, not empty (see the snapshot's `unavailable` map). The rules and features
that depend on it stay quiet for that cycle instead of reading the absence as a fact:

| Category unavailable | What stays quiet |
|---|---|
| `indexes`, `foreign_keys` | index-health rules (unused, duplicate, invalid, missing FK index) |
| `tables` | table rules (bloat, autovacuum tuning, wraparound) |
| `replication` | unused-index drops (a standby may use the index) |
| `prepared_xacts` | the RCA `orphaned_prepared_tx` signal; an open orphaned 2PC incident is held open, not counted as cleared |
| `partitions` | index recommendations (the tuning agent gets no table context, since any table might be a partitioned parent or a partition) |
| `index_bloat` | the `index_bloat` rule's resolutions (it also holds them when the reading was truncated at its 200-row cap) |
| `config_data` | the advisor's config features and the RCA idle-in-transaction signal |

The next cycle that reads the category resumes them.

!!! tip "See also: [Hint Verification](hint-verification.md)"
    EXPLAIN before-and-after output for every optimization pg_sage applies, proving each hint and index change produces the intended plan improvement.

---

## Source filter and risk fields

`GET /api/v1/findings?source=<subsystem>` filters by the producer that emitted a finding:
`rules`, `advisor`, `optimizer`, `query_tuning`, `forecaster`, `incident`, `schema_lint`,
`tuning`, `cloud`, `governance`, `migration`, `sre`, `facts` or `custom`. The categories each value matches are
in the [generated finding category catalog](generated/finding-catalog.md), and
`GET /api/v1/findings/categories` (any signed-in role) serves the same registry: each
subsystem with its categories, and each category's producer, severities, config keys and
actions. Each row reports the same value as `subsystem`, or `""` for a category the
registry does not know. An unknown `source` value applies no filter, and
`migration_advisor` is reserved and returns no rows.

In a fleet, `GET /api/v1/findings?database=<name>` also returns `cluster_refs`: the open
cluster-scope findings (`detail.scope = "cluster"`) that another database reports as the
owner of `<name>`'s cluster, each marked `cluster_ref` and `read_only` and named by
`database_name`. They are references, not part of the page or its `total`; see
[Cluster runways and forecasts](walkthrough-fleet.md#cluster-runways-and-forecasts).

Each finding row (list and detail) also carries display-only action fields:

| Field | Meaning |
|---|---|
| `action_type` | The executor action the `recommended_sql` classifies as, or `""` when it has none |
| `contract_tier` | That action contract's base risk tier (`read_only`, `safe`, `moderate`, `high`), or `none` when the SQL is empty or has no contract: the finding is advisory only |
| `action_risk` | The stricter of the producer's risk (`detail.action_risk`, with `high_risk` reported as `high`) and the contract tier; `none` when there is no SQL. An unrecognised producer risk is reported as `high` |
| `approval_required`, `approval_reason` | Whether, and why, the change may run only with an operator's approval: a producer mark, a restart-required or non-allowlisted setting, or high risk |

The upsert stores the producer's risk in `detail.action_risk`. These fields never gate
execution: the executor re-derives risk and approval under policy for every action.

---

## Lifecycle

Each analyzer cycle persists its findings, publishes them to the executor, the API and MCP,
and resolves the open findings a fully evaluated category no longer reports.

- **Identity.** A finding is one open row per `(category, object_identifier)`. Two writers
  racing on a new identity end with one open row; neither reports an error.
- **Partial failures.** A finding that cannot be persisted (for example a statement
  timeout) does not stop the cycle. The rest are still persisted, and the cycle logs one
  error naming each failed finding's category and identity. Resolving cleared findings
  also continues past a failed row. A lost connection (a connect or network error,
  SQLSTATE class 08, a server shutdown) stops the batch instead, so an unreachable
  database costs one timeout per cycle rather than one per finding.
- **Suppression.** Before publishing, one query drops every identity under an active
  suppression (`status = 'suppressed'` with `suppressed_until` empty or in the future),
  including identities that came after a failed row. If that query fails, the cycle keeps
  the previously published findings, records no recommendations and resolves nothing.
  Suppression is per database in fleet mode.
- **After an action.** When an action resolves a finding, the snapshot's collection time
  decides what the next sighting means. Evidence collected before the action resolved the
  finding is stale and is ignored. Evidence collected up to 2 × max(analyzer interval,
  collector interval) after it (20 minutes with the defaults) reopens the same row with
  `detail.persisted_after_action` set to the action's `action_log` id: the action did not
  clear the condition. A critical finding reopened this way pages once, subject to the
  usual one-hour cooldown per identity. Later evidence opens a new row. Writers outside
  the analyzer cycle (runway, schema lint, audit, cloud telemetry) carry no collection
  time; their evidence is treated as two default collector intervals (2 minutes) old.
  The comparison uses the sidecar's snapshot clock against the database's `resolved_at`,
  so keep both clocks synchronised (NTP).

---

## Index Health

### unused_index

Detects indexes with zero scans over the observation window.

**Severity:** warning

**What it detects:** Indexes that consume disk space and slow down writes but are never used for reads. Evaluated against `pg_stat_user_indexes.idx_scan` over `analyzer.unused_index_window_days` (default 7) of clean evidence.

A statistics reset makes zero scans meaningless, so it restarts the clock. Resets come from
`pg_stat_reset()`, a single-relation reset or a server restart. Each snapshot records the
database's statistics epoch (`pg_stat_database.stats_reset` or the postmaster start). The
window never starts before the last reset. A counter that went down, or an index dropped
and recreated under the same name (new oid), also restarts it. The finding shows
`unused_since` and `stats_epoch`.

The clock survives sidecar restarts. The window is measured from the first persisted
zero-scan sighting, kept in `sage.unused_index_clock` (one row per index at zero scans, in
each database's own sage schema in fleet mode; PG 14-18). A row counts only while the
index has the same oid. Until the analyzer has read that table, no `unused_index` finding
resolves. An open zero-scan finding from before the table existed gets its clock from the
earliest day of snapshot history that shows the same index continuously at zero scans
(one document a day, at most window + 1 days). Without such history the clock starts at
the restart and the finding stays open until the clock reaches the window. A scan, a
dropped or recreated index, a counter that went down or a statistics reset still resolve
it. Before an autonomous `DROP INDEX`, the executor re-checks
live that the index still has zero scans and that no reset happened inside the window.
If either check fails, it does not drop the index.

**Example output:**

```
Unused index public.idx_old on public.orders (zero scans)
```

**Recommended action:** Drop the index.

```sql
DROP INDEX CONCURRENTLY public.idx_old;
```

**Partitioned tables:** an index attached to a partitioned index is never flagged:
PostgreSQL refuses to drop it on its own ("index ... requires it"). Each attached index
keeps its clock, and when every one under a partitioned index has been unused for the
window, one finding is raised on the partitioned index itself ("unused on N of M
partitions"; `detail.partitioned_index`, `partitions_unused`, `partitions_total`). It is
advisory: no `recommended_sql`, a plain `DROP INDEX` in `detail.manual_sql` (a partitioned
index cannot be dropped `CONCURRENTLY`, and the drop locks every partition) and
`approval_required`. A sub-partitioned tree rolls up to its top partitioned index. A
standalone index on a partition (never attached, or left by `DETACH PARTITION`) is
evaluated like any other. The rule does not run in a cycle where the partition tree or
the partitioned indexes could not be read. On upgrade, open findings on attached
partition indexes resolve once.

---

### duplicate_index

Detects indexes with identical column sets on the same table.

**Severity:** critical

**What it detects:** Two or more indexes covering the same columns in the same order on the same table. Wastes disk space and write throughput.

**Example output:**

```
Duplicate index public.idx_orders_dup2 matches idx_orders_dup1
```

**Recommended action:** Drop the duplicate, keeping the one referenced by constraints or with the most specific definition.

```sql
DROP INDEX CONCURRENTLY public.idx_orders_dup2;
```

**Partitioned tables:** an index attached to a partitioned index is never the one to
drop. Two attached duplicates (from two partitioned indexes on the same key) give no
finding; a standalone index on a partition that duplicates an attached one is the one
dropped.

---

### invalid_index

Detects indexes that failed creation and are in an invalid state.

**Severity:** warning

**What it detects:** Indexes where `pg_index.indisvalid = false`, typically from a failed `CREATE INDEX CONCURRENTLY`.

**Example output:**

```
Invalid index public.idx_orders_status_ccnew (indisvalid=false)
```

**Recommended action:** Drop the invalid index and re-create it.

**Partitioned tables:** an invalid index can be attached to a partitioned index (a failed
`CREATE INDEX CONCURRENTLY` on a partition, then `ATTACH PARTITION`). PostgreSQL refuses to
drop it on its own, so the finding is advisory (`detail.attached_to_partitioned_index`, no
`recommended_sql`) and recommends `REINDEX INDEX CONCURRENTLY` on it, which keeps it
attached, or dropping the partitioned index.

```sql
DROP INDEX CONCURRENTLY public.idx_orders_status_ccnew;
CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status);
```

**First look.** The first look reads `pg_stat_progress_create_index` before it judges an
invalid index:

| Case | Severity | Suggested SQL |
|---|---|---|
| A build is in progress (the index, or its table when `index_relid = 0` or the build is `REINDEX CONCURRENTLY`) | info | none |
| `_ccnew`/`_ccnewN` copy left by a failed `REINDEX CONCURRENTLY` | warning | `DROP INDEX CONCURRENTLY`, then re-run the reindex of the original |
| `_ccold`/`_ccoldN` copy left by a failed `REINDEX CONCURRENTLY` | warning | `DROP INDEX CONCURRENTLY` |
| Unique index | warning | `SELECT ... GROUP BY <keys> HAVING count(*) > 1 LIMIT 20` when every key is a plain column; none for expression keys. Rebuild only after removing the duplicates |
| Any other | warning | `REINDEX INDEX CONCURRENTLY` |

If the progress view cannot be read, or hides other roles' builds (the role lacks
`pg_read_all_stats`), the items carry a caveat. The check itself is not degraded.

---

### missing_fk_index

Detects foreign key columns that lack a supporting index on the referencing table.

**Severity:** warning

**What it detects:** Foreign keys where the referencing column(s) have no matching index. This causes sequential scans on JOINs and cascading DELETE operations.

The rule is schema-aware: each key is checked against the indexes of its own schema's table,
so in a schema-per-tenant layout an unindexed `tenant_b.orders` is reported even when
`tenant_a.orders` is indexed. An index whose leading columns are the key's columns in any
order supports it; the recommended index lists a composite key's columns in declared order.
A key on a partitioned table is checked on each leaf partition (PostgreSQL enforces it there,
and only the leaves' indexes are visible), and the finding names the partition. Nothing is
guessed: a key whose table is not in the snapshot gives no finding. An index that is the only
support of a key is never recommended for drop by `unused_index`. That includes a partition's index
for a key on its partitioned parent; when the partition list could not be read, `unused_index`
does not run while a key is on a partitioned table or one missing from the snapshot.

**Example output:**

```
Missing index on public.order_items(order_id) for FK to public.orders
```

**Recommended action:** Create an index on the FK column(s).

```sql
CREATE INDEX CONCURRENTLY idx_order_items_order_id ON public.order_items (order_id);
```

**Partitioned tables:** PostgreSQL rejects `CREATE INDEX CONCURRENTLY` on a partitioned
table, and its indexes are not in `pg_stat_user_indexes`. The rule checks a partitioned
table's keys against its partitioned indexes (valid, non-partial, key columns leading).
An uncovered key gives one advisory finding on the parent with no `recommended_sql` and,
in `detail.partition_plan`, the statements that build the index without blocking writes:
`CREATE INDEX ... ON ONLY` the parent, then for each partition `CREATE INDEX CONCURRENTLY`
and `ALTER INDEX ... ATTACH PARTITION`. The parent index becomes valid once every partition
is attached. A parent with sub-partitioned partitions gets the note only. The copies of
the key that PostgreSQL clones onto each partition are covered by that one finding; a key
declared on a partition alone is evaluated as on a plain table. The rule does not run in
a cycle where the tables, the partition tree or the partitioned indexes could not be read.

---

### index_bloat

Estimates btree index bloat from planner statistics and recommends an online rebuild.

**Severity:** info from `analyzer.index_bloat_threshold_pct` (default 30), warning from twice it

**What it detects:** The collector's `index_bloat` snapshot category estimates, once per
cycle, how many leaf pages each btree index needs: its `reltuples` at its fillfactor (default
90), with each leaf tuple the 8-byte-aligned index tuple header plus the key columns'
`pg_stats.avg_width` net of `null_frac`, and a 4-byte line pointer (the estimate
check_postgres and pgsql-bloat-estimation use). The excess of `relpages` over that is the
bloat. It is a single bounded catalog statement: no relation is opened or `stat()`ed and
`pgstattuple` is not needed, so it runs on managed providers too. Only valid btree leaf
indexes of at least `analyzer.index_bloat_min_bytes` (default 100 MiB) are read, the 200
largest per cycle. Left out:

- invalid indexes (`invalid_index` owns them);
- partitioned indexes (relkind `I`): each leaf partition's index is judged on its own, since a
  `REINDEX CONCURRENTLY` of the parent rebuilds every partition;
- indexes whose table or index has no `reltuples` (never vacuumed or analyzed), or whose key
  columns have no `pg_stats` row (never analyzed, or the role cannot read the column: the
  estimate needs `SELECT` on the indexed columns, and for an expression index ownership of the
  index or `pg_read_all_data`). An open finding on an index read without an estimate is
  kept, not resolved;
- the system schemas, `sage`, TOAST and temporary tables.

`relpages` and `reltuples` are as of the last `VACUUM`, `ANALYZE` or rebuild, so rows deleted
since then show as bloat only after the next vacuum. B-tree deduplication can make an index
smaller than the estimate; that only under-reports. When more indexes are above the floor than
the 200 read, the reading is truncated: open `index_bloat` findings are then kept, not
resolved, until a complete reading (raise `analyzer.index_bloat_min_bytes` if this persists).

**Example output:**

```
Index public.orders_created_idx is an estimated 70% bloated (164.1 MB of 234.4 MB)
```

**Recommended action:** Rebuild the index online. The statement runs under the
`reindex_concurrently` contract (risk tier moderate, approval required, maintenance window
enforced). The rebuild needs free disk for a second copy of the index while it runs. The
finding's detail carries `estimated_bloat_pct`, `bloat_bytes`, `index_bytes`, `relpages`,
`estimated_pages`, `fillfactor`, `reltuples` and `estimate_source` (`catalog_statistics`);
confirm with `pgstatindex()` where `pgstattuple` is installed.

```sql
REINDEX INDEX CONCURRENTLY "public"."orders_created_idx";
```

The rebuild resets `relpages`, so the next cycle's reading resolves the finding.

---

### PostgreSQL 18 skip scan

Not a finding category: how `missing_index` candidates (from the tuning agent or the
deterministic floor) are judged on PostgreSQL 18 and later. Btree skip scan lets an
index on `(a, b)` serve `WHERE b = $1` when `a` has few distinct values, so a new
single-column index on `b` beside it would pay its write cost for nothing.

A candidate is a skip-scan candidate when the server is 18 or later, the candidate is a
single plain-column, non-unique btree on `b`, and a valid btree on the same table has `b`
as its second key (same collation and operator class) behind a plain leading key whose
`pg_stats.n_distinct` is known and at most 200 (a negative `n_distinct` is a fraction of
the live rows). That index must also serve the candidate's `WHERE` and `INCLUDE`
columns. pg_sage then plans the candidate's top workload statements naming `b` (at most
3, by calls) with `EXPLAIN (GENERIC_PLAN, FORMAT JSON)`, read-only, on one connection
under the what-if's statement and lock timeouts. Only when every plan already scans the
existing index with an `Index Cond` on `b` is the candidate dropped, with the reason
`served_by_skip_scan:<index>`. The rejection is kept in rejection memory, so the idea is
not planned again each cycle; once that index is dropped (or no longer qualifies) the
candidate is judged again. Missing evidence never drops a candidate: no such statement,
a statement that cannot be planned read-only, an `EXPLAIN` error (logged as a WARN
naming the table) or a plan that does not use the index all leave it to the usual
HypoPG what-if. When the what-if rejects a skip-scan candidate for too little
improvement, the reason adds "already served by `<index>` through skip scan". The tuning
agent's table context carries a one-line PostgreSQL 18 note on skip scan.

Majors before PostgreSQL 18 behave as before: no extra `EXPLAIN`, the same candidates. The
server version is read with the database collation, so it costs no extra round trip.
Managed providers get the same plans; fleet mode judges each database on its own.

---

## Query Performance

Query findings are about application workload only. pg_sage's own statements,
diagnostic tooling (`EXPLAIN`, maintenance, statistics resets, backup `COPY ... TO`),
utility statements (`BEGIN`, `COMMIT`, `SAVEPOINT`, `SET`, `DISCARD ALL`, `DEALLOCATE`,
`LOCK`, `LISTEN`/`NOTIFY`) and DDL or privilege changes (`ALTER`, `DROP`, `TRUNCATE`,
`GRANT`, ...) are never the subject of `slow_query`, `high_total_time` or other advice:
no index or hint can fix them. A slow `CALL` or `DO` is still reported.

There is no slow-`COMMIT` finding: `pg_stat_statements` cannot see one. Its `COMMIT`
entry times only the statement, which marks the transaction for commit; the commit
itself (deferred constraint triggers, the WAL flush, the wait for a synchronous standby)
runs after it and is never counted. A `COMMIT` held 30 ms by a deferred trigger shows
about 0.001 ms (measured on PostgreSQL 14 and 17), and so did one that waited minutes for
a synchronous standby (PostgreSQL 17). A standby whose replay falls 30 seconds or more
behind shows up as `replication_lag`; shorter per-commit synchronous waits, and a
synchronous standby that is not connected at all, are not reported. The deferred
triggers' own statements appear under `pg_stat_statements.track = all`.

### slow_query

Detects queries whose mean execution time over the last collector interval is above the threshold.

**Severity:** warning; critical when the interval mean is more than 5x the threshold and the query used at least 1% of the interval's wall time

**What it detects:** For each statement in both of the two newest snapshots, the interval mean: the `total_exec_time` it added between them over the `calls` it added. The finding needs the interval mean above `analyzer.slow_query_threshold_ms` (default 1000ms) and at least `analyzer.slow_query_min_calls` calls in the interval (default 5), so a single ad hoc query is not a finding and never critical. The cumulative `mean_exec_time` since the last `pg_stat_statements` reset is not used: it hides a statement that became slow under a long history of fast calls, and keeps flagging one that was slow long ago. The interval is not judged on the first cycle (no previous snapshot), across a statistics reset or epoch change, or for a statement whose counters fell; the category is then not evaluated and its open findings stay open. A statement without a measured interval in a cycle (idle, below the call floor, new to the snapshot or evicted) has no verdict, so its open finding stays open; it resolves on a measured interval at or below the threshold, when the statement leaves the snapshot, or when it has made fewer calls than the floor in total. The detail carries `interval_mean_ms`, `interval_calls`, `interval_seconds`, `pct_wall_clock` and `cumulative_mean_ms`.

**Example output:**

```
Slow query (3500.0ms mean over 42 calls in the last 60s, 3.5x threshold)
```

**Recommended action:** Examine the query plan, add indexes, or rewrite the query.

---

### high_plan_time

Detects queries where planning time is disproportionately high.

**Severity:** warning

**What it detects:** Queries where `mean_plan_time` is a significant fraction of total execution time, suggesting complex JOINs or excessive partitions.

**Example output:**

```
High plan time: 450ms mean_plan_time for query with 50ms mean_exec_time
```

**Recommended action:** Simplify the query, reduce partition count, or materialize subqueries.

---

### query_regression

Detects queries whose latency in the last collector interval regressed against their own hourly history.

**Severity:** warning; critical above +200%

**What it detects:** A statement whose interval mean (see `slow_query`, with the same `analyzer.slow_query_min_calls` floor) exceeds its cumulative mean by `analyzer.regression_threshold_pct` (default 50%) is a candidate; at most 50 candidates per cycle, the largest interval time first. For each, pg_sage reads its `sage.query_store` samples over `analyzer.regression_lookback_days` (default 7), excluding the last hour, as hourly buckets. The baseline is the median of the bucket interval means, using buckets with at least the call floor and no statistics reset; it needs at least 6 such buckets. The finding needs the interval mean above the baseline by the threshold. A plan flip from 2 ms to 20 ms on a query with a million earlier calls moves the cumulative mean by about 9%, below the threshold; the interval mean against the hourly median shows +900%. Without a previous snapshot, or when the history read fails, the category is not evaluated and its open findings stay open. A statement without a measured interval, a candidate without a baseline and a candidate past the cap of 50 have no verdict that cycle, so their open findings stay open. Coverage depends on which statements `sage.query_store` samples. When `baseline.enabled` is on and the statement's hour-of-week bucket is active (data from at least 3 of the last `baseline.weeks` weeks, see `workload_anomaly`), the baseline is that bucket's median instead: a report that is 10x slower every Monday at 09:00 is judged against earlier Mondays at 09:00, so it neither fires every week nor hides a real slowdown. The detail carries `interval_mean_ms`, `interval_calls`, `cumulative_mean_ms`, `baseline_ms`, `baseline_method` (`hour_of_week` for the bucket median, `median_hourly_interval` for the flat one) and `baseline_buckets` (weeks for `hour_of_week`, hourly buckets otherwise).

**Example output:**

```
Query regression: 20.0ms now vs 2.0ms hourly median (+900%)
```

**Recommended action:** Investigate recent changes -- schema modifications, data volume changes, or configuration updates.

---

### workload_anomaly

Detects workload that is far above its usual value for this hour of the week ("slower than usual").

**Severity:** warning; critical at `baseline.z_critical`

**What it detects:** Every cycle, pg_sage folds what it already collected into one of 168 hour-of-week buckets (Monday 00:00 is bucket 0, in `baseline.timezone`, by the local wall clock across daylight-saving shifts) of `sage.baseline_samples`, one mean per database, metric, key, week and bucket. The metrics are each statement's interval mean (`query_mean_ms`, the same interval and `analyzer.slow_query_min_calls` floor as `slow_query`, for at most `baseline.max_query_keys` statements, the largest interval time first; needs `pg_stat_statements`), active and total backends (`active_backends`, `total_backends`), the cluster's WAL bytes per second (`wal_bytes_per_s`) and the lock-graph sessions per wait class (`wait_sessions`). No new query reaches the monitored database. A value is scored against the same bucket in the previous `baseline.weeks` weeks with a robust z, (value - median) / (1.4826 x MAD), the MAD floored at 5% of the median. A bucket with data from fewer than 3 weeks is learning and never fires. The value must also rise over the median by an absolute floor: `baseline.min_delta_ms` for statements, 5 sessions for backends, 1 MiB/s for WAL, 3 sessions for a wait class. A warning needs z at or above `baseline.z_warning` (default 4) for 2 consecutive cycles; critical needs `baseline.z_critical` (default 6), and for a statement at least 10% of the interval's DB time. An open finding resolves after 2 consecutive normal cycles; a cycle without a verdict (an idle statement, a learning bucket) keeps it open, a statement no longer in `pg_stat_statements` resolves, and when the baseline cannot be read the category is not evaluated and open findings stay open. A `pg_stat_statements` reset or a statement evicted and re-entered adds nothing to its bucket. In fleet mode every database keeps its own baselines. Detection is deterministic and uses no LLM; the fixed-threshold rules stay in place. A critical anomaly starts one Sage SRE investigation per episode (trigger `workload_anomaly`, idempotency key `anomaly:<metric>:<key>:<episode_start>`), which triages the lock, connection and plan mechanisms read-only. The detail carries `metric`, `key`, `how`, `observed`, `baseline_median`, `mad`, `z`, `weeks`, `method` (`hour_of_week_mad`) and `episode_start`, and for a statement `queryid`, `query` and `db_time_share`.

**Example output:**

```
Workload anomaly: query_mean_ms of queryid:4711 is 50.0, usually 5.0 at this hour of the week (z 60.7 over 4 weeks)
```

**Recommended action:** Compare with the same hour in earlier weeks: look for a new plan, a data or traffic change, or a deploy since the last normal cycle.

---

### seq_scan_heavy

Detects tables that are scanned sequentially far more than through indexes right now.

**Severity:** warning

**What it detects:** Tables with at least `analyzer.seq_scan_min_rows` live rows whose scans in the interval since the previous snapshot are more than 100 sequential scans and more than ten times their index scans. Only the interval counts: an old seq-scan storm stops being flagged once the scans stop, and a new storm on a table with many lifetime index scans is caught. The rule needs two snapshots in the same statistics epoch: on the first cycle after a start, after `pg_stat_reset()` or a single-table reset, or after a server restart it is not evaluated and open findings stay open. A table whose counters went down, or that is new since the previous snapshot, is judged on the next interval. The finding detail carries the lifetime `seq_scan` and `idx_scan`, and `interval_seq_scan`, `interval_idx_scan` and `interval_seconds`.

**Example output:**

```
Table public.events: 1840 seq scans vs 12 idx scans in the last 60s (2500000 rows)
```

**Recommended action:** Analyze query patterns and add appropriate indexes. When no tuning agent is active, the deterministic index floor proposes `missing_index` candidates for these tables (see Tier 2 -- LLM Index Optimizer below).

---

## Sequences

### sequence_exhaustion

Detects sequences approaching the end of their range.

**Severity:** warning at 75% consumed, critical at 90%

**What it detects:** Sequences that have consumed a high share of their own range, `[MINVALUE, MAXVALUE]`, in the direction they move: ascending sequences toward `MAXVALUE`, descending sequences toward `MINVALUE`. A custom `MINVALUE` or `MAXVALUE` is honoured, so a descending sequence with `MINVALUE -1000` is judged against -1000, not the data type's minimum. Integer sequences get bigint migration advice.

`CYCLE` sequences are skipped: they wrap to the start of their range by design, so reaching the end is not an outage. Open findings on `CYCLE` sequences resolve on the next analyzer cycle.

**Detail:** `current_value`, `min_value`, `max_value`, `increment`, `cycle`, `usage_pct`, `data_type` and `runway_category` (`forecast_sequence_runway`). The time to the limit at the current rate, and the owning column's type limit, are in the `forecast_sequence_runway` finding for the same sequence; this finding reports only how much of the range is used.

**Example output:**

```
Sequence public.orders_seq is 93.1% consumed (integer)
```

**Recommended action:** Alter the sequence to use `bigint`, or reset with a higher ceiling.

```sql
ALTER SEQUENCE public.orders_seq AS bigint;
```

Sequence exhaustion causes `INSERT` failures. Address critical findings immediately.

---

## Maintenance

### table_bloat

Detects tables with excessive dead tuple accumulation or bloat.

**Source:** `rules_vacuum.go`

**Severity:** warning (SAFE-tier `VACUUM`); info, with no SQL, while IO is saturated

**What it detects:** Tables whose dead tuples exceed
`analyzer.table_bloat_dead_tuple_pct` (default `20`) percent of their live plus
dead tuples, on tables with at least `analyzer.table_bloat_min_rows` (default
`1000`) rows: VACUUM is not keeping up.

**Example output:**

```
Table public.events has 15% dead tuples (450,000 dead / 3,000,000 live)
```

**Recommended action:** Run manual VACUUM or tune autovacuum settings.

```sql
VACUUM (VERBOSE) public.events;
```

---

### autovacuum_tuning

Recommends a per-table `autovacuum_vacuum_scale_factor` for large, write-heavy tables.

**Source:** `rules_autovacuum_tuning.go`

**Severity:** warning (`moderate`-tier `set_table_autovacuum` action)

**What it detects:** Tables with at least `analyzer.autovacuum_tune_min_rows` live rows (default 1,000,000) whose lifetime updates plus deletes exceed their live rows. The target scale factor is 0.05, 0.02 from 10M rows, and 0.01 from 50M rows.

**Respects per-table tuning:** The rule reads each table's own `pg_class.reloptions`, collected with the table statistics every cycle, so it works the same with or without an LLM. It never recommends a value above an explicit per-table setting: a table already at or below the target is left alone, including after pg_sage applies its own setting. A table with `autovacuum_enabled = false`, or with reloptions the rule cannot parse, is skipped.

**Recommended action:**

```sql
ALTER TABLE public.events SET (autovacuum_vacuum_scale_factor = 0.05);
```

The rollback is `RESET (autovacuum_vacuum_scale_factor)`, or `SET` back to the table's explicit prior value, as stored (`0.10` stays `0.10`), when it had one.

This rule is pg_sage's only autovacuum tuner. The freeze custodian does not tune autovacuum: a table nearing its freeze maximum (amber) gets no custodian action, and a red one is frozen.

---

### xid_wraparound

Detects a database approaching the XID wraparound threshold.

**Source:** `rules_vacuum.go`

**Severity:** warning from `analyzer.xid_wraparound_warning` (default
500,000,000), critical from `analyzer.xid_wraparound_critical` (default
1,000,000,000)

**What it detects:** The database's `age(datfrozenxid)` approaching the wraparound limit, risking database shutdown.

**Example output:**

```
XID age is 1800000000 (wraparound risk)
```

**Recommended action:** Find what holds the xmin horizon (long transactions,
replication slots, orphaned prepared transactions). The finding carries a
read-only holder query in `detail.diagnostic_sql` and no SQL to run: the freeze
custodian freezes red tables (see
[the freeze ladder](configuration.md#vacuum-bloat-and-freeze-autopilot)).

---

### wraparound_freeze

**Source:** `rules_wraparound_freeze.go`

Diagnoses tables that have used `analyzer.wraparound_freeze_share` (default
0.75) of their effective freeze maximum: the lower of
`autovacuum_freeze_max_age` and the table's own `autovacuum_freeze_max_age`
reloption (and likewise for multixacts). A server tuned to
`autovacuum_freeze_max_age = 1000000000` is judged against that setting, not
against a fixed 150 million.

**Severity:** warning from the share, critical once the table is past its
maximum (anti-wraparound autovacuum is due).

**What it detects:** The horizon the freeze custodian reads (one reader for
the custodian, the runway advisor and this rule): the most advanced
relations first, a TOAST relation reported as the table that owns it, and
system catalogs (`detail.catalog: true`). Partitioned parents have no
`relfrozenxid` and are never read; each partition is assessed on its own.
pg_sage's own schema is not diagnosed.

**Example output:**

```
Table public.events_2026_10 has used 82% of its freeze maximum
```

**Recommended action:** None from the analyzer. The finding is diagnosis only
(no SQL, no action risk); `detail.custodian_decision` names the freeze
custodian's decision for the table. The custodian is the only producer of
freeze actions: a `VACUUM (FREEZE)` (or a plain `VACUUM` past the freeze
table age) for a red user table, the pinning xmin holder when one blocks it,
and a plan-only `plan_catalog_freeze` escalation for red system catalogs
(a catalog `VACUUM` needs `MAINTAIN` or ownership, which managed providers
rarely grant).

The retired `analyzer.wraparound_freeze_xid_age` still loads, with one
warning, and is ignored.

---

## System Health

### connection_leak

Detects idle connections that may indicate connection pool leaks.

**Severity:** warning

**What it detects:** Connections that have been idle for extended periods, consuming connection slots.

**Example output:**

```
Connection leak: 15 idle connections from app-server-1 (oldest: 4h)
```

**Recommended action:** Fix the application's connection pool configuration or set `idle_in_transaction_session_timeout`.

---

### cache_hit_ratio

Detects a low buffer cache hit ratio over the last collector interval.

**Severity:** warning below `analyzer.cache_hit_ratio_warning` (default 0.95); critical below 0.80.

**What it detects:** The hit ratio of the blocks this database accessed since the
previous snapshot, `blks_hit / (blks_hit + blks_read)` of the interval, is below the
threshold: a working set that outgrew `shared_buffers`, or a new heavy scan. The
lifetime ratio in `pg_stat_database` counts since the last statistics reset, often
months, and barely moves when the cache starts missing now, so it is reported in the
detail (`lifetime_cache_hit_ratio`) but never judged.

The rule is evaluated only when the interval is evidence:

- at least 10,000 blocks were accessed in the interval (a quiet minute of a few hundred
  reads proves nothing either way);
- the interval was measured: not the first snapshot after pg_sage starts, and no
  statistics reset (`pg_stat_reset()`, a single-relation reset, a server restart, or a
  counter that fell) inside it.

Otherwise an open finding is left as it is; it resolves only on an interval that shows
a healthy ratio over enough blocks.

**Example output:**

```
Cache hit ratio 80.00% over the last 60s (threshold 95.00%)
```

The detail carries `cache_hit_ratio` (interval), `lifetime_cache_hit_ratio`,
`blocks_accessed`, `interval_seconds` and `threshold`.

**Recommended action:** Increase `shared_buffers` or investigate workload changes.

The snapshot's `system` category keeps `cache_hit_ratio` with its lifetime meaning (the
forecaster and the dashboard read it) and adds the raw counters and an `interval`
object: `seconds`, `cache_hit_ratio`, `blks_accessed`, `deadlocks`, `xacts`,
`temp_bytes`, `blk_read_time_ms`, `blk_write_time_ms` and `checkpoints` since the
previous snapshot, each `null` when it is unknown. Three more are left out when unknown:
`sessions` (sessions the database opened, closed ones included; PG 14+), and `wal_bytes`
over `wal_seconds` (how far the cluster's WAL insert position advanced between the two
reads of it; unknown on a standby and after the position went back). RCA's
`wal_growth_spike` and connection storm compare these across two consecutive intervals.

---

### sage_footprint

Detects pg_sage itself taking too much of the database it guards.

**Severity:** warning

**What it detects:** The `sage` schema (every table with its TOAST data and indexes)
is larger than `retention.sage_size_warning_pct` percent (default 10) of the database
size collected this cycle, and at least 256 MB (the snapshot cap's minimum): on a small
database a fresh install's own tables are a large share without being a problem. The
finding names the share, the limit and the five largest sage tables. `0` disables the
check; while the database size is unknown the open
finding is kept, not resolved.

**Example output:**

```
pg_sage data uses 23.4% of database lifeos (limit 10%)
```

**Recommended action:** Shorten `retention.snapshots_days` (or the retention of the
largest table listed), or raise `retention.sage_size_warning_pct` if the share is
expected. Snapshot rows written before compact storage age out with
`retention.snapshots_days`.

---

### sage_self_cost

Detects pg_sage itself using too much of the database's time.

**Severity:** warning

**What it detects:** pg_sage's own statements used more database time per collector
cycle than `analyzer.self_cost_budget_ms` (default 3000 ms, 5% of one core at the
default 60 s collector interval). pg_sage does not hide from `pg_stat_statements`: every
statement it sends carries the `/* pg_sage */` tag after its first keyword (after the
opening parenthesis of a parenthesized query; every statement of a multi-statement query
gets its own), where `pg_stat_statements` keeps it on PostgreSQL 14 to 18, and each
analyzer cycle sums those entries' execution and planning time for this database since the previous cycle.
The finding also gives statements, shared blocks, sage-table rows read and written per
cycle and the sage schema's size. Without `pg_stat_statements`, after a statistics reset,
and on the first cycle after a start, the open finding is kept, not resolved; `0`
disables it. The same figures are exported as `pg_sage_self_*` Prometheus metrics.

The tag names the component that sent the statement (`/* pg_sage collector */`,
`/* pg_sage sre:probe v1 */`): the collector, analyzer, executor, facts, tuner and
optimizer name their package, Sage SRE names its probe. When the window's cost has a
component breakdown, the finding's `top_component` and
`top_component_db_time_ms_per_hour` detail and its advice name the costliest one. An
entry whose text `pg_stat_statements` first saw under a bare `/* pg_sage */` tag (before
an upgrade that named its component) counts as `unattributed` until the entry is evicted
or the statistics are reset; that share is shown, not hidden.

**Example output:**

```
pg_sage monitoring used 4200 ms of database time per collector cycle on lifeos (budget 3000 ms)
```

**Recommended action:** Open **Settings > General > Footprint** (or
`GET /api/v1/self-cost?database=<name>`, see [Footprint API](#footprint-api)) to see
which component costs most; lengthen `collector.interval_seconds` or
`analyzer.interval_seconds`, or raise `analyzer.self_cost_budget_ms` if the cost is
expected.

#### Footprint API

`GET /api/v1/self-cost` (every signed-in role; `?database=<name>` for one database,
404 for an unknown one) answers what pg_sage costs each database over the analyzer's
latest window. Per database it returns `db_time_ms_per_hour`, `calls_per_hour`,
`blocks_per_hour`, `rows_read_per_hour` and `rows_written_per_hour` (sage tables),
`schema_bytes`, `window_seconds` and `cycle_seconds` (the database's own collector
period); the budgets `db_time_budget_ms_per_hour` (with `db_time_budget_source`:
`self_budget.db_time_ms_per_hour`, `analyzer.self_cost_budget_ms` at that period, or
`none`), `blocks_budget_per_hour` and `storage_budget_bytes`; `top_statements` (text
without the tag, `component`, `db_time_ms`, `calls`, `blocks`); `by_component` (per
hour, with each component's `share` of the database time) and `unattributed_share`; and
`known` flags (`window`, `statements`, `storage`). `process` gives the sidecar's CPU per
collector cycle against `self_budget.cpu_ms_per_cycle` and its busiest loops of local work
(`component:database`, LLM waits excluded), once for all databases, as the process watch
measured them. A figure not measured yet (first cycle after a start, no
`pg_stat_statements`) is `null`, never `0`; a budget that is not set is `null`. The
database time per component is also exported as
`pg_sage_self_component_db_time_ms_per_hour{database,component}`, with the component
folded into `analyzer`, `collector`, `executor`, `facts`, `optimizer`, `sre`, `tuner`,
`unattributed` or `other` (a component counts under the part before `:`).

---

### sage_self_budget

Detects pg_sage exceeding the budget it declares for itself.

**Severity:** warning

**What it detects:** pg_sage used more than its `self_budget` of one or more resources.
On a database (object type `database`): its own statements' database time per hour
(`self_budget.db_time_ms_per_hour`, off by default because
`analyzer.self_cost_budget_ms` already budgets it per cycle), the shared blocks they
touch per hour (`self_budget.blocks_per_hour`, default 18,000,000) and the size of the
`sage` schema (`self_budget.storage_mb`, default 10240). One finding lists every
resource over budget with what it used, and names the top consumers: pg_sage's five
costliest statements of the window (database time, calls, blocks, the component the tag
names; text shown without the tag) and that database's busiest loops. For the process
(object type and identifier `sidecar`): the sidecar's CPU per global collector cycle
(`self_budget.cpu_ms_per_cycle`, default 600 ms, 1% of one core at the default 60 s
interval). CPU is a process figure, so one process watch checks it once for every
database the sidecar monitors and raises one finding, kept on the lowest-named connected
database (the next one when that database refuses the write; a copy left elsewhere is
resolved); its `top_loops` are the busiest loops across the process. A loop is named
`component:database` (`collector:app`), or the component alone for a process loop; its
busy time is wall time minus the time it waited on the LLM provider and minus the loops
it ran inside it, and that wait is recorded as `llm_wait:<database>`, which never ranks
in `top_loops`. A resource that could not be measured (first cycle after a start, no
`pg_stat_statements`) keeps the open finding open; `0` disables a resource. The same
figures are exported as `pg_sage_self_cpu_ms_per_cycle`,
`pg_sage_self_db_time_ms_per_hour`, `pg_sage_self_blocks_per_hour`,
`pg_sage_self_budget{resource}`, `pg_sage_self_budget_exceeded{database,resource}` (for
CPU `pg_sage_self_budget_exceeded{resource="cpu"}`, without a database),
`pg_sage_self_loop_busy_seconds_total{loop,database}` and
`pg_sage_self_loop_runs_total{loop,database}` (`loop` is the component, `database` is
absent on a process loop, so `sum by (loop)` adds every database), with each loop's
failed runs and last success under the same labels (see deployment.md, Loop health).
A database's per-cycle figures are per cycle of its own collector (its period in fleet
mode, which self-configuration may lengthen).
When the database-time budget is inherited from `analyzer.self_cost_budget_ms`, each database
inherits it at its own collector period, exported as
`pg_sage_self_budget_database{database,resource="db_time"}`, and that database's `db_time`
exceeded flag compares with it.

**Example output:**

```
pg_sage monitoring exceeded its own storage budget on lifeos
pg_sage process exceeded its own cpu budget (all monitored databases together)
```

**Recommended action:** Start with the top statements and loops in the finding.
Lengthen `collector.interval_seconds` or `analyzer.interval_seconds`, turn off
components you do not use, shorten retention for storage, or raise the `self_budget`
value if the cost is expected. To see where the sidecar's CPU goes, enable
`debug.pprof_enabled` and take a CPU profile (see
[configuration](configuration.md#profiling-the-sidecar)).

---

### checkpoint_pressure

Detects high checkpoint frequency.

**Severity:** warning

**What it detects:** Checkpoints occurring more frequently than expected, indicating heavy write load or insufficient `max_wal_size`.

**Example output:**

```
Checkpoint pressure: 45 checkpoints in last hour (expected < 6)
```

**Recommended action:** Increase `max_wal_size` and `checkpoint_completion_target`.

---

## Replication

### replication_lag

Detects replication lag exceeding acceptable thresholds.

**Severity:** warning (replay lag of 30 seconds or more) or critical (5 minutes or more)

**What it detects:** Standby servers falling behind the primary, measured via `pg_stat_replication`.

**Identity:** one finding per `replica:<application_name>@<client_addr>` (`<unnamed>` and `<unknown>` stand in for an empty name or a Unix-socket connection). `pg_stat_replication` row order is not stable, so the row position is not part of the name. Replicas that share a name merge into one finding at the worst lag; `detail.merged_replicas` counts them. Findings opened by earlier releases (named `replica:<row>:<addr>`) resolve once after the upgrade and reopen under the new name.

**Example output:**

```
Replication lag: 45 seconds behind primary
```

**Recommended action:** Investigate network issues, standby I/O capacity, or heavy write load on primary.

---

### inactive_slot

Detects inactive replication slots consuming WAL.

**Severity:** warning, or critical when `wal_status` is `unreserved` or `lost`

**What it detects:** Replication slots without an active consumer, causing WAL to accumulate and disk usage to grow. A slot is reported once it has been inactive for `analyzer.inactive_slot_min_minutes` (default 5), so a CDC connector's reconnect or a nightly consumer does not raise a warning. The inactive time comes from `pg_replication_slots.inactive_since` on PostgreSQL 17 and later; on older servers pg_sage starts its own clock the first time it sees the slot inactive (kept in memory, so a restart only delays a finding, never fires one early; an already open finding stays open while that clock catches up). A slot whose `wal_status` is `extended` (warning) or `unreserved`/`lost` (critical) is reported at once. Temporary slots are never reported, and neither are synchronized failover slots (`synced = true`) on a standby, which are always inactive there and cannot be dropped.

**Detail:** `slot_type`, `retained_bytes`, `wal_status`, `safe_wal_size`, `inactive_since`, `inactive_seconds` and `invalidation_reason` (each when the server has it), `manual_sql` and `approval_required`.

**Example output:**

```
Inactive replication slot old_subscriber (retaining 12.0 GB)
```

**Recommended action:** Find the slot's consumer (CDC connector, subscriber or standby) and reconnect it. If it is gone for good, drop the slot by hand with `detail.manual_sql`. The finding is `high_risk` and carries no executable SQL: dropping a slot is irreversible and the consumer must resync, so pg_sage never runs it.

```sql
SELECT pg_drop_replication_slot('old_subscriber');
```

---

### subscription_errors

Detects a logical replication subscriber whose apply or table synchronization workers keep failing.

**Severity:** warning (the error counters rose since the analyzer last read them), or critical (the subscription is disabled and has errors, as `disable_on_error` leaves it)

**What it detects:** On a subscriber, an apply worker that fails on a unique violation or a missing row exits and is restarted every few seconds, so changes stop arriving while the subscription still looks enabled. pg_sage reads `apply_error_count` and `sync_error_count` from `pg_stat_subscription_stats` (PostgreSQL 15 and later; PostgreSQL 14 has no error counters, so this category never fires there) for this database's subscriptions (`pg_subscription.subdbid`, so fleet mode reads one set per database). The comparison is with the counters the previous analyzer cycle read, kept in memory: after a restart the first cycle only records them, and an open finding stays open. A stats reset counts the errors since the reset. On PostgreSQL 18 the conflict counters (`confl_*`) are added to the detail.

**Identity:** `subscription:<name>`.

**Detail:** `enabled`, `worker_running`, `apply_error_count`, `sync_error_count`, `error_count`, `error_count_delta`, `conflicts` (PostgreSQL 18), `latest_end_lsn`, `latest_end_time`, `manual_sql` and `approval_required`.

**Recommended action:** Find the failing transaction in the subscriber's log and fix the conflicting row on the subscriber. The finding is `high_risk` and carries no executable SQL: `detail.manual_sql` holds `ALTER SUBSCRIPTION ... SKIP (lsn = '...')` with a placeholder for the finish LSN from the log (skipping loses that transaction's changes on the subscriber), or `ALTER SUBSCRIPTION ... ENABLE` for a disabled subscription. pg_sage never runs subscription DDL.

---

### subscription_lag

Detects an enabled subscription that stopped reporting to its publisher.

**Severity:** warning

**What it detects:** An enabled subscription whose apply worker last reported to the publisher (`pg_stat_subscription.latest_end_time`, refreshed by keepalives even when the publisher is idle) at least `analyzer.subscription_lag_minutes` (default 5) ago, or that had no apply worker in two consecutive snapshots (the launcher restarts a failed worker within seconds, so one snapshot without one is not evidence). A disabled subscription is left to `subscription_errors`.

**Identity:** `subscription:<name>`.

**Detail:** `enabled`, `worker_running`, `lag_seconds` (when a worker reports), `latest_end_lsn`, `latest_end_time`.

**Recommended action:** Check the network path to the publisher, the publisher's walsender, `max_logical_replication_workers` and `max_worker_processes`, and the subscriber's log. Advisory only.

---

### cdc_consumer_stalled

Detects a CDC consumer that holds its logical slot but no longer confirms progress.

**Severity:** warning

**What it detects:** A Debezium (or other CDC) slot can stay `active` while its `confirmed_flush_lsn` stands still, so it retains WAL and holds the xmin horizon without ever looking inactive. pg_sage reads each logical slot's `confirmed_flush_lsn` and how far the server's WAL is past it, and keeps an in-memory clock per active, permanent logical slot: the finding fires when WAL advanced (the lag grew) but `confirmed_flush_lsn` did not move for `analyzer.cdc_stall_minutes` (default 10). It resolves as soon as the position moves. An idle server whose WAL does not move raises nothing. Inactive slots are `inactive_slot`'s. After a restart the clock starts again, and an open finding stays open while it catches up. pg_replication_slots lists the whole cluster, so in fleet mode every database on a cluster reports the slot, as for `inactive_slot`.

**Identity:** `slot:<name>`.

**Detail:** `confirmed_flush_lsn`, `confirmed_flush_lag_bytes`, `retained_bytes`, `stalled_seconds`.

**Recommended action:** Check the connector's health and committed offsets. A connector that only captures tables with little traffic needs a heartbeat (Debezium `heartbeat.interval.ms`) to advance its slot. Advisory only.

---

### cdc_heartbeat_stale

Detects a CDC heartbeat table that stopped being written.

**Severity:** warning

**What it detects:** For each entry of `replication.cdc_heartbeats` whose slot is a logical slot of the monitored database, pg_sage runs one `SELECT max(column)` on the heartbeat table under a 2-second statement timeout and raises this finding when the newest value is at least `max_age_minutes` old, or when the table has no row. With nothing configured, nothing runs. A heartbeat that could not be read (a missing table, a permission, a timeout) raises nothing and keeps an open finding open.

**Identity:** `cdc_heartbeat:<slot>`.

**Detail:** `slot_name`, `table`, `column`, `max_age_seconds`, `age_seconds` and `latest`, or `no_rows`.

**Recommended action:** The connector on that slot is down, stuck or disconnected: check it and its heartbeat setting before the slot's WAL retention grows. Advisory only.

---

## Extension Health (pg_cron and pg_partman)

pg_cron and pg_partman keep state nothing else watches: a job that fails every
night, a run history that pg_cron never purges, a partitioned table that ran out of
future partitions, rows landing in a default partition. Each cycle one query reads
`pg_extension` for the monitored database; each rule below runs only when its
extension is installed there, and reads the schema the extension's tables live in
(the schema of pg_cron's `job` table, of pg_partman's `part_config`), quoted, never
an assumed `cron` or `partman`. With neither extension installed nothing runs
beyond that query, and these categories are not evaluated, so open findings stay
open. Every read is read-only under `safety.query_timeout_ms`. In fleet mode each
database reports its own extensions: pg_cron's jobs appear on the database where
pg_cron is installed (`cron.database_name`). None of these findings carries SQL to
run; the recommendation names the operator's step. The rules live in
`rules_ext_gate.go`, `rules_ext_pgcron.go` and `rules_ext_partman.go`, and each can
be switched off by a [rule pack](#rule-packs-and-owner-checks) under its category
name. They are tested against stub schemas with pg_cron's and pg_partman's shapes on
PostgreSQL 14 and 17; a run against real pg_cron 1.6 and pg_partman 5 is pending.

### pg_cron_job_failed

**Severity:** warning (info for the visibility finding below)

**What it detects:** a job whose latest finished run in the last 24 hours failed
(`cron.job_run_details.status = 'failed'`, timed by `end_time`, else `start_time`;
a run exactly 24 hours old is included). `failures` counts its failed runs in those
24 hours. A job whose latest run succeeded is clear, so its finding resolves on the
next cycle. At most 50 jobs are reported, newest failure first; past 50 the category
is not evaluated that cycle, so the jobs not reported keep their open findings. It
needs `job_run_details` (pg_cron 1.4 and later); without it, or when the role cannot
read it, the category is not evaluated and one warning per streak names the table.

pg_cron's row security shows a role only the jobs it owns. When it applies to the
monitoring role and that role sees no job, pg_sage cannot tell healthy from hidden:
it raises one info finding, `pg_cron:visibility` with `visibility: own_jobs_only`,
instead of reporting health, and leaves the category not evaluated. Failures of the
jobs it does see carry `visibility: own_jobs_only` too.

**Identity:** `pg_cron:job:<jobid>`.

**Detail:** `jobid`, `jobname` (when `cron.job` has the column), `failures`,
`last_failed_at`, `last_message` (literals redacted, cut to 300 characters),
`window_hours`, `visibility` (`all_jobs` or `own_jobs_only`) and `schema`.

**Recommended action:** read the job's runs in `job_run_details`, fix the command or
what it depends on, and remove an obsolete job with `cron.unschedule(jobid)`.

### pg_cron_history_unbounded

**Severity:** info

**What it detects:** `job_run_details` holding more than 1,000,000 rows by the
planner's estimate (`pg_class.reltuples`; it never counts the table). A table never
analyzed (`reltuples = -1`) is not judged. pg_cron never deletes run history.

**Identity:** the quoted `job_run_details` table, for example `cron.job_run_details`.

**Detail:** `estimated_rows`, `threshold_rows` and `schema`.

**Recommended action:** schedule a cleanup job, for example
`SELECT cron.schedule('purge-cron-history', '0 3 * * *', $$DELETE FROM cron.job_run_details WHERE end_time < now() - interval '7 days'$$)`,
or set `cron.log_run = off` when run history is not needed.

### partman_premake_gap

**Severity:** warning

**What it detects:** a `part_config` parent (at most 100 per cycle; past that the
category is not evaluated) with fewer future child partitions than its `premake`.
pg_sage lists the newest `premake + 1` children with `show_partitions(parent, 'DESC')`
and reads each child's bounds with `show_partition_info`. A time-controlled child is
future when it starts after now; an id-controlled child when it starts above the
highest control value. That value is read with `SELECT max(control)` on the listed
children, newest first, stopping at the first that holds a row; the parent and older
children are never scanned, and when no listed child holds a row every listed child
is future. A parent whose children could not be read keeps its open finding.
`premake = 0` is not judged.

**Identity:** the quoted parent table.

**Detail:** `parent_table`, `premake`, `future_partitions`, `control`, `control_kind`
(`time` or `id`) and `schema`.

**Recommended action:** `CALL partman.run_maintenance_proc()` creates the missing
children; then find why maintenance stopped (the background worker's
`pg_partman_bgw.interval`, the job that calls it, or `automatic_maintenance` off).

### partman_default_rows

**Severity:** warning

**What it detects:** the default partition of a `part_config` parent holds at least
one row. pg_sage finds the default partition in the catalog and checks it with
`SELECT EXISTS (SELECT 1 FROM <default>)`, which reads at most one row. It does not
judge by `reltuples`, which stays above zero after the rows are moved out until the
next analyze, and reads -1 or 0 on a new default partition. A default partition that
could not be read keeps its open finding.

**Identity:** the quoted default partition.

**Detail:** `parent_table`, `default_partition`, `estimated_rows` (`reltuples`) and
`schema`.

**Recommended action:** `CALL partman.partition_data_proc('<parent>')` moves the rows
into proper children (pg_partman 5; `partition_data_time()` or
`partition_data_id()` on 4.x). Rows in the default partition also block pg_partman
from creating a child whose range they fall in.

## Extension Health (TimescaleDB)

TimescaleDB runs compression, retention and continuous-aggregate refresh as
background jobs, and a failing policy keeps failing quietly: chunks stop
compressing, retention stops dropping, aggregates go stale. These two rules sit
behind the same `pg_extension` query as the
[pg_cron and pg_partman checks](#extension-health-pg_cron-and-pg_partman): they run
only where `timescaledb` is installed, and read the `timescaledb_information` views
of TimescaleDB 2.x in the schema the extension's `job_stats` view lives in, quoted.
Without TimescaleDB nothing runs beyond that query and both categories are not
evaluated, so open findings stay open. Without the 2.x views (TimescaleDB 1.x) both
are not evaluated and one warning per streak says so. Every read is read-only under
`safety.query_timeout_ms`. In fleet mode each database reports its own jobs and
aggregates. Neither finding carries SQL; the recommendation names `run_job()` and
the policy to check. Each can be switched off by a
[rule pack](#rule-packs-and-owner-checks) under its category name. The rules live in
`rules_ext_timescale.go` and `rules_ext_timescale_rules.go`. Timescale Cloud and
self-managed servers carry TimescaleDB 2.x with policies; RDS and Cloud SQL do not.
They are tested against stub relations with the 2.x view shapes on PostgreSQL 14
and 17; a run against real TimescaleDB 2.11 and 2.17 is pending.

### timescale_job_failed

**Severity:** warning

**What it detects:** a scheduled job (`timescaledb_information.jobs.scheduled`) whose
last run failed (`timescaledb_information.job_stats.last_run_status = 'Failed'`). A
job that never ran (a NULL status) is clear, and so is a paused job: pausing it with
`alter_job(..., scheduled => false)` is the operator's decision, and a paused job does
not fail again. TimescaleDB's own telemetry job (`policy_telemetry`) is not reported:
it fails wherever the server cannot reach the internet, and no policy depends on it. A job whose next run succeeds resolves on the next cycle. At most 50
jobs are reported, newest run first; past 50 the category is not evaluated that
cycle, so the jobs not reported keep their open findings. When the role cannot read
the views the category is not evaluated and one warning per streak names
`job_stats`.

**Identity:** `timescaledb:job:<job_id>`.

**Detail:** `job_id`, `proc_schema`, `proc_name` (for example `policy_compression`,
`policy_retention`, `policy_refresh_continuous_aggregate`), `hypertable_schema` and
`hypertable_name` (when the job has a hypertable), `total_failures`, `next_start`,
`last_run_started_at` and `schema`.

**Recommended action:** run the job by hand with `CALL run_job(<job_id>)` to see its
error (TimescaleDB 2.15 and later also keep it in
`timescaledb_information.job_errors`), fix the cause, and check the policy's
settings in `timescaledb_information.jobs` (`alter_job` changes them).

### timescale_cagg_stale

**Severity:** warning

**What it detects:** a continuous aggregate whose watermark lags the clock by more
than 3 x (`schedule_interval` + `end_offset`) of its scheduled refresh policy
(`policy_refresh_continuous_aggregate`), while that policy has not succeeded within the
same time (`job_stats.last_successful_finish`); a lag of exactly that much is clear. A NULL
or negative `end_offset` counts as zero; with more than one refresh policy the
largest sum is used. The watermark comes from `cagg_watermark()`, which TimescaleDB
2.12 moved from `_timescaledb_internal` to `_timescaledb_functions`; pg_sage reads
whichever exists in `pg_proc`, the newer first, and when neither does the category
is not evaluated (open findings stay open) and one warning per streak says so, while
`timescale_job_failed` still runs. Not judged: an aggregate without a scheduled
refresh policy, one over an integer time column (its watermark is not a time), and
one that never materialized anything (its watermark is the time type's minimum; a
failing first refresh shows up as `timescale_job_failed`). At most 100 aggregates
per cycle; past that the category is not evaluated.

The watermark is the end of the newest materialized bucket, so it follows the data,
not the clock: it stops when the source hypertable stops receiving rows, it advances by
whole buckets, and over a `timestamp` column holding local time it is off by the UTC
offset. A refresh policy that keeps succeeding has materialized all there is, so a
recent success clears the aggregate however far its watermark lags; the finding
means the policy is failing, stuck, or not being run (for example the job scheduler is
stopped, or `timescaledb_post_restore()` was not run after a restore). When the role
cannot read `job_stats` (`timescale_job_failed` is then not evaluated and warns), the
aggregate is judged by its watermark alone, and those cases can trip it.

**Identity:** the quoted continuous aggregate view, for example
`public.metrics_hourly`.

**Detail:** `view_schema`, `view_name`, `hypertable_schema`, `hypertable_name`,
`materialized_only`, `job_id`, `schedule_interval`, `end_offset`, `watermark`,
`lag_seconds`, `threshold_seconds`, `watermark_function`, `schema`, and
`last_successful_finish` when the policy has succeeded before.

**Recommended action:** check the refresh job in `timescaledb_information.job_stats`
and run it by hand with `CALL run_job(<job_id>)`; if it never starts, check that
TimescaleDB's job scheduler runs (`timescaledb.max_background_workers`, and
`timescaledb_post_restore()` after a restore).

---

## Backups and WAL Archiving

AP-11 (agent posture) reports backup *configuration*: archiving or point-in-time
recovery off, retention 0. These categories report backup *results*: whether WAL
is archived and whether the provider holds a recent good backup. pg_sage never
treats missing evidence as healthy: a self-managed server's backup tool
(pgBackRest, Barman, WAL-G, `pg_basebackup` cron jobs) leaves nothing in the catalog,
so its backup freshness is **unknown**, never "ok"; only the WAL archiver is
watched there. Archiving is cluster-wide: in fleet mode the database that owns
its instance (the one that also owns the instance's WAL and connection advice)
reports it, once. The archiver rules live in `rules_backup.go`; the provider
reads are in the `cloudtel` package.

### wal_archive_failing

**Severity:** warning, or critical once the archiver has failed for 30 minutes
or more without a successful archive

**What it detects:** `archive_mode` is `on` or `always`, `pg_stat_archiver.failed_count`
rose since the previous snapshot, and `last_failed_time` is after
`last_archived_time`. The streak is measured on the server's clock, from the last
successful archive (or the statistics reset when nothing was archived since) to the
last failure; with neither known the finding is a warning marked
`streak_known: false`. Between the archiver's retries `failed_count` can stay flat
for a cycle: while nothing has been archived since the last failure the open
finding stays open; it resolves after the first successful archive or when
archiving is turned off (AP-11 then reports that).

**Detail:** `archive_mode`, `failed_count`, `archived_count`, `failed_since_last_cycle`,
`last_archived_wal`, `last_archived_time`, `last_failed_time`, `streak_known` and
`streak_minutes`.

**Recommended action:** Read the server log for the `archive_command` or
`archive_library` error (destination full, credentials, network) and fix it.
Point-in-time recovery stops at the last archived WAL, and `pg_wal` grows until
archiving recovers. Advisory only: pg_sage runs nothing.

### wal_archive_stale

**Severity:** warning, or critical at four times the gap

**What it detects:** archiving is on, at least one completed WAL segment waits
after `last_archived_wal` (from the current WAL file and `wal_segment_size`), and
the last successful archive is older than `backup.max_archive_gap_minutes`
(default 15). An idle server, whose only unarchived WAL is the segment being
written, is never stale however old its last archive. A failing archiver is
`wal_archive_failing`'s, and a standby (no current WAL position) is not judged.
This catches an `archive_command` that hangs or falls behind without reporting a
failure. It needs one successful archive to measure from: with nothing archived
since the statistics reset (no `last_archived_wal`, for example `archive_mode = on`
with an empty `archive_command`, which keeps WAL without archiving it) the pending
count is unknown and the rule does not judge.

**Detail:** the archiver fields above, `pending_segments`, `archived_age_minutes`
and `max_archive_gap_minutes`.

### backup_stale

**Severity:** warning, or critical at twice the age

**What it detects:** the provider's last good backup is older than
`backup.max_age_hours` (default 26; exactly the limit does not fire). On RDS it is
the instance's `LatestRestorableTime`, on Aurora the cluster's (both in the
descriptions pg_sage already reads); on Cloud SQL the end of the latest successful
run among the 5 newest `backupRuns.list` entries, read at most every 15 minutes.
Five newest runs with failures and no success also fire (`source: backup_runs`,
no age). No reading (a response without the field, a denied `backupRuns.list`, a
time in the future) leaves the category unevaluated, so an open finding stays
open; a denied read is logged once per reason.

**Detail:** `source` (`latest_restorable_time`, `last_successful_backup` or
`backup_runs`), `age_hours`, `max_age_hours`, `last_good` and `resource`.

### backup_retention_below_policy

**Severity:** warning

**What it detects:** the provider keeps backups for fewer days than
`backup.min_retention_days` (default 7; 0 turns the check off). On RDS and Aurora it
is the backup retention period, on Cloud SQL the transaction log retention days.
Retention 0 is AP-11's "backups off" case and is left to it.

**Detail:** `retention_days`, `min_retention_days` and `resource`.

### backup_shares_delete_path

**Severity:** warning, or critical on a production database with the agent gate on

**What it detects:** no backup the provider shows outlives a delete of the instance, so
one delete call would remove the database and its backups together. RDS: no manual
snapshot within `backup.max_age_hours`, no AWS Backup recovery point and no cross-region
backup replication (automated backups are deleted with the instance unless the delete
retains them). Aurora and RDS Multi-AZ DB clusters: no manual cluster snapshot within that
age. More than 20 manual snapshots with none of the first 20 recent enough is unknown, not
a finding. Cloud SQL:
`settings.retainBackupsOnDelete` is false. Snapshots and automated backups are read once
per 6 hours (15 minutes after a failed read); a snapshot without a create time or not yet
`available` does not count, and one exactly `backup.max_age_hours` old does. A denied
read, a missing field or a provider without an adapter (Azure, AlloyDB, Neon, Lakebase)
leaves the category unevaluated, so an open finding stays open. Production is the agent
gate's environment binding (a database the gate cannot classify counts as production);
pg_sage cannot see which agent holds a cloud credential able to delete the instance, so
with the gate on a production database is the critical case. Naming a backup in
`backup.external_source` resolves it.

**Detail:** `isolation_evidence` (what decided it), `survives_delete`, `production` and
`resource`.

### deletion_protection_off

**Severity:** warning

**What it detects:** a production database (by the agent gate's environment binding, as
above) whose instance, or Aurora cluster, has deletion protection off. Without the agent
gate the environment is unknown and the category is not evaluated; AP-11 still reports
deletion protection off on every managed instance.

**Detail:** `deletion_protection` and `resource`.

### pitr_window_below_policy

**Severity:** warning

**What it detects:** point-in-time recovery reaches back less than `backup.min_pitr_hours`
(default 24; 24 hours passes, 23 h 59 m fails; 0 turns the check off). RDS: the active
automated backup's restore window; Aurora: the cluster's latest minus earliest restorable
time; Cloud SQL: the transaction log retention while point-in-time recovery is on. With
point-in-time recovery off there is no window and AP-11 reports it.

**Detail:** `pitr_window_minutes`, `min_pitr_hours` and `resource`.

### restore_drill_failed

**Severity:** critical

**What it detects:** a restore drill of the database failed or could not run. A
drill asks `clone.provider` for a copy at the latest restorable point, connects only
after the identity guard proves the copy is not the source, and checks, read-only:
the source's 20 largest tables exist with the same columns; each table's row
estimate is within 5% of the source's (5.0% passes, 5.1% fails) and the 3 smallest
match an exact `count(*)` within 5%; a checksum of the first 1,000 rows by primary
key created before the recovery point matches the source (only for tables with a
`created_at`-like column, else `unknown`); `SELECT 1` reads each table; and the
newest committed marker (`sage.snapshots.collected_at`, or the policy document's
`drill.marker_query`) is read to measure RPO. A provider error, a clone that is the
source (`target_is_production`) or a failed invariant opens the finding, and it
pages through the finding alerts (`finding_opened` at critical). The next passed
drill resolves it. A failed drill after a pass does not reset the pass that D7
reads ([agent governance](agent-governance.md)).

**Detail:** `drill_id`, `provider`, `kind`, `failure` (the step and provider, or the
failed invariants), `receipt_id`, `cost_label`, `rto_seconds`, and `rpo_seconds` or
`rpo_unknown`. Every drill, passed or not, is a row of `sage.restore_drills`.

### recoverability_unknown

**Severity:** info

**What it detects:** no clone provider is configured (`clone.provider: none`), so no
drill can prove the database's backups restore ("SQL only: recoverability
unknown"). The recommendation names what to configure: a branch or copy-on-write
provider (`lakebase_branch`, `neon_branch`, `aurora_clone`) or `dle` drills by
default; a full copy (`rds_restore`, `cloudsql_clone`, `alloydb_restore`,
`azure_restore`) needs `clone.allow_full_copy: true` and `full_copy` in the policy
document's `drill.kinds`. Until a drill passes, D7 keeps production agent writes at
L2. A passed drill resolves it.

**Detail:** `reason`.

---

## More Tier 1 Categories

These deterministic rules run alongside the categories above.

| Category | What it detects |
|----------|-----------------|
| <a id="high_total_time"></a>`high_total_time` | A query whose total execution time exceeds 10% of the analysis interval, even when each call is fast. |
| <a id="sort_without_index"></a>`sort_without_index` | An EXPLAIN plan where a Sort feeding a Limit sorts at least 10x the rows the Limit returns; an index on the sort key avoids the sort. |
| <a id="plan_regression"></a>`plan_regression` | A query whose plan changed and whose estimated cost rose at least 2x, or 1.5x with node changes or new disk spills. |
| <a id="stale_statistics"></a>`stale_statistics` | A table with at least 10,000 live rows (`analyzer.analyze_stale_min_rows`) that was never analyzed and has changed since, or whose rows changed since its last analyze (`n_mod_since_analyze`) exceed the autoanalyze threshold and whose last analyze is older than 7 days (`analyzer.analyze_stale_days`). The threshold is autoanalyze's own: `autovacuum_analyze_threshold + autovacuum_analyze_scale_factor x reltuples`, each from the table's storage parameters when set there, else from the server settings. A table with no change since its last analyze is never flagged, however old the analyze. Recommends `ANALYZE`; the detail carries `mod_since_analyze` and `analyze_threshold`. A partitioned parent, which autoanalyze never processes, gets an info finding (`partitioned_parent: true`) when it was never analyzed or not within the cutoff and its partitions' changes since their last analyze exceed the parent's threshold; its `ANALYZE` samples every partition (and analyzes each one before PostgreSQL 18's `ANALYZE ONLY`), so it needs an operator's approval. It is skipped when the partition list could not be read. |
| <a id="slow_replication_slot"></a>`slow_replication_slot` | An active replication slot retaining at least 1 GB of WAL (critical at 5 GB): a slow consumer. |
| <a id="lock_chain"></a>`lock_chain` | A root blocker and the tree of sessions waiting on it in the monitored database. pg_sage's own sessions and configured safe processes get a finding but never cancel or terminate SQL. |
| <a id="stat_statements_pressure"></a>`stat_statements_pressure` | `pg_stat_statements` above 80% of `pg_stat_statements.max` (critical above 95%), where entries start being evicted. |
| <a id="work_mem_promotion"></a>`work_mem_promotion` | A role with `work_mem` hints on five or more queries (`analyzer.work_mem_promotion_threshold`). Recommends one `ALTER ROLE ... SET work_mem`; advisory only. |
| <a id="extension_drift"></a>`extension_drift` | An installed extension whose version lags its default version until `ALTER EXTENSION ... UPDATE` runs. Advisory only. |

---

## Rule packs and owner checks

**Disabling rules.** `analyzer.disabled_rules` names analyzer rules (`unused_indexes`,
`inactive_slots`) or categories (`unused_index`, `query_regression`) to switch off, globally
or per database ([configuration](configuration.md#rule-packs-and-owner-checks)). A disabled
snapshot rule does not run; findings of a disabled category are dropped. Its open findings
are resolved, never left open or deleted: `status = 'resolved'` with
`detail.resolution = "rule_disabled"` and `detail.disabled_by` (the API user's email, `user <id>`
when a stored override is replayed after a restart, or the config path that disabled it).
Derived categories (`clone_schemas`, `app_managed_index`) can be disabled too: they are
dropped after they are derived, so disabling `clone_schemas` hides the family finding but
does not undo the collapse of the per-schema findings into it.
Re-enabling the rule opens a new finding on the next cycle that still sees the problem. `wraparound_freeze`, `xid_wraparound`, `sequence_exhaustion` and the
`sage_*` self rules cannot be disabled: the config load and the API refuse them with the
reason. The suppression API and `schema_lint.disabled_rules` are unchanged.

<a id="custom"></a>**Owner checks (`custom:<id>`).** Each row an [owner check](custom-checks.md)
returns becomes a `custom:<id>` finding (source filter `custom`) keyed by the check's
`object_column`, with the check's severity, title and message. `detail` carries `check_id`,
`fingerprint` (the parsed statement's fingerprint), `row` (every column as text) and
`truncated`. A custom finding never carries SQL: `recommended_sql` is always empty, so it
projects into a case with no action. A check that fails, times out, is refused or returns more
than `max_rows` rows resolves nothing: its open findings stay open until a complete run no
longer returns them.

---

## LLM Advisors, Query Tuner and Executor

| Category | Source | What it reports |
|----------|--------|-----------------|
| <a id="wal_tuning"></a>`wal_tuning` | LLM advisor | WAL and checkpoint settings, such as `max_wal_size` when checkpoints are forced. |
| <a id="connection_tuning"></a>`connection_tuning` | LLM advisor | Connection capacity and churn: `max_connections` (never below the window's peak of total backends + reserved slots + 10), `idle_in_transaction_session_timeout` for this database only (`ALTER DATABASE`, approval required), and pooler advice or observations as `info` rows without SQL. |
| <a id="io_settings"></a>`io_settings` | Advisor (deterministic, PostgreSQL 18+) | `info`, no SQL: `effective_io_concurrency` or `maintenance_io_concurrency` set (source other than `default`) at or below its pre-18 default (1 and 10), which overrides PostgreSQL 18's new default of 16, or `io_method = sync` from a non-default source, which switches asynchronous I/O off. The detail carries `setting`, `value`, `source`, `scope` (`instance`, or `database` for a value set for this database alone), `pg_major`, the documented `doc_note` and, for the concurrency settings, `pre18_default` and `pg18_default`. Makes no model call; never before PostgreSQL 18 or on an unknown version. In fleet mode instance-wide values are reported by the database that owns the instance settings, per-database values by their database. |
| <a id="query_rewrite"></a>`query_rewrite` | LLM advisor | Rewrites for the slowest queries. Text only, always advisory. |
| <a id="bloat_remediation"></a>`bloat_remediation` | LLM advisor | Options for removing table and index bloat. |
| <a id="memory_tuning"></a><a id="vacuum_tuning"></a>`memory_tuning`, `vacuum_tuning` | Tuning agent | Memory and autovacuum configuration changes. A recommendation without SQL is dropped. |
| <a id="query_tuning"></a>`query_tuning` | Query tuner | Per-query hints (for example `Set(work_mem ...)` or an index scan) stored in `sage.query_hints`. Plan anti-patterns (`index_filter_inefficient`, `or_join_filter` in `symptoms`) are advice only: no hint and no SQL, with the plan facts in the `plan_antipatterns` detail (see below). |
| <a id="query_parameterization"></a>`query_parameterization` | Query tuner | A statement shape sent with many IN-list lengths (at least `tuner.param_shape_min_variants` queryids, before PostgreSQL 18), or a statement whose planning time dominates. Advice only, no SQL and no hint: use prepared statements or one array parameter (`= ANY($1)`). Its case offers a `prepare_parameterized_query` script (see [hint verification](hint-verification.md#when-the-tuner-reports-query_parameterization)). |
| <a id="runaway_query"></a>`runaway_query` | Executor | A running query past the configured runaway limits, such as its maximum duration. |
| <a id="workload_budget_violation"></a>`workload_budget_violation` | Workload budgets | `info`: a [workload budget](workload-budgets.md) would have cancelled statements (strictly past its timeout, each counted once a day) or rejected connections today. Warn mode changes nothing; the counts are in `sage.budget_shadow`. |
| <a id="workload_budget_proposal"></a>`workload_budget_proposal` | Workload budgets | `info`: a `statement_timeout` budget proposed for a role without one, 3x its 14-day p99 statement time rounded up to a whole second and never below its longest statement (`advisor.budget_proposals`). Text only, with a `budgets:` snippet in warn mode. |
| <a id="workload_budget_enforce"></a>`workload_budget_enforce` | Workload budgets | An enforce-mode role budget past 7 days of warn-mode data with no protected application cancelled: `set_role_timeout` (`ALTER ROLE ... IN DATABASE ... SET`, moderate) or `set_role_connection_limit` (`ALTER ROLE ... CONNECTION LIMIT`, HIGH risk, always approval). An enforced `statement_timeout` reverts automatically when its cancels counted from the server log exceed twice the shadow prediction within 24 hours; without a readable log it needs approval ("auto-revert unavailable: no statement log"). Resolves once the role has the setting. |

### Plan anti-pattern detail (`query_tuning`)

A `query_tuning` finding whose plan shows an anti-pattern carries
`plan_antipatterns`, one entry per occurrence. Every entry has `symptom` and
`rows_removed` (over all loops) and `loops`.

| Symptom | Other keys |
|---------|------------|
| `index_filter_inefficient` | `relation` (schema.table), `index_name`, `index_cond`, `filter`, `filter_columns` (columns the filter compares; empty for an OR or an expression), `rows_kept`, `removed_share` (0.9 to 1) |
| `or_join_filter` | `join_filter`, `arms` (the top-level OR arms), `aliases` (the join aliases the arms reference) |

The recommendation text names the composite index or the UNION rewrite; pg_sage
does not create the index or rewrite the statement from these findings. See
section 13 of [Hint verification](hint-verification.md).

---

## Tier 2 -- LLM Index Optimizer

<a id="missing_index"></a><a id="covering_index"></a><a id="partial_index"></a><a id="composite_index"></a>
When LLM is enabled, the optimizer generates additional findings of category `missing_index`; the index kind (missing, covering, partial or composite) is in the `index_category` detail. Releases before v1.8.0 stored the kind as the category (`covering_index`, `partial_index`, `composite_index`). These are consolidated index recommendations that have passed through 8 validators and confidence scoring. Each recommendation includes:

- The CREATE INDEX statement (always uses CONCURRENTLY)
- Confidence score (0.0-1.0)
- Action level (autonomous / advisory / informational)
- HypoPG cost reduction (when available)
- Write impact assessment

Missing-index advice also works without an LLM. When the tuning agent is inactive (no endpoint, breaker open, daily budget spent), the deterministic index floor proposes `missing_index` findings for tables flagged `seq_scan_heavy`, with `detail.source = "deterministic_floor"`. They pass the same validator, rejection memory and HypoPG what-if; HypoPG measures their impact, and without HypoPG they are advisory with no executable SQL ("HypoPG not installed: impact unmeasured"). See [Missing-index advice without an LLM](configuration.md#missing-index-advice-without-an-llm).

---

## Tier 2 -- Configuration Advisor

With an LLM configured, the advisor proposes `wal_tuning`, `connection_tuning`,
`query_rewrite` (advisory) and `bloat_remediation` (advisory) findings. The model
never resolves one by going quiet: each cycle pg_sage re-checks every open advisor
finding against the latest snapshot, deterministically.

| Category | Resolves when |
|----------|---------------|
| `wal_tuning`, `connection_tuning` | the live `pg_settings` value equals the recommended value (from the `ALTER SYSTEM`, the managed `ALTER DATABASE`, or the managed-change intent; units normalized). A setting pending a restart is not live yet |
| `query_rewrite` | the query has been absent from `pg_stat_statements` for 3 consecutive cycles (a stats reset or an unreadable query list does not count) |
| `bloat_remediation` | the table has been under 10% dead tuples (or under 1,000 rows) for 2 consecutive cycles |
| `io_settings` | the next cycle no longer sees the carried-over value (it re-runs every cycle on the latest `pg_settings`; skipped, and kept open, while the server version is unknown or the snapshot has no settings) |
| <a id="advisor_degraded"></a>`advisor_degraded` | the LLM is enabled again, or the advisor is disabled on purpose |

A `connection_tuning` `info` row without SQL (pooler advice or an observation) has no
setting to re-check and stays open until it is dismissed.

A finding that still holds is re-emitted unchanged, and only its own setting,
query or table is left out of the next model review. Dismissing a finding keeps
working as before (it is suppressed). The first cycle after upgrading resolves
WAL and connection findings a DBA already applied by hand.

`wal_tuning` is grounded in the advisor window's checkpoint and WAL history (see
[WAL and bloat advisors](configuration.md#wal-and-bloat-advisors)). It is not proposed
while under 20% of the window's checkpoints are requested. A `max_wal_size` below the
current value or below the measured floor (1.5 x WAL per `checkpoint_timeout`) is never
emitted; the floor finding carries `max_wal_size_floor_bytes`,
`wal_per_checkpoint_timeout_bytes`, `wal_bytes_per_hour`, `checkpoint_timeout_seconds`,
`window_seconds`, `requested_share`, `current_max_wal_size_bytes` and `arithmetic` in its
detail. Without the window's history every `wal_tuning` finding is advice without SQL.
An executed change is verified by the requested share of checkpoints.

`bloat_remediation` lists at most 50 tables per review, those with the most estimated
dead bytes, and its dead-tuple trend spans the advisor window ("unknown" without a
window-start snapshot).

A global setting finding whose GUC some tables override (a per-table finding, or
an explicit storage parameter) is kept, with `detail.overridden_by_tables` naming
those tables: they keep their own setting and the global change does not affect
them.

---

## Tuning, Forecast, Cloud, Governance and SRE Categories

These producers write findings outside the analyzer rules. Their subsystems, severities
and config keys are in the [generated catalog](generated/finding-catalog.md).

| Category | Source | What it reports |
|----------|--------|-----------------|
| <a id="index_health"></a>`index_health` | Analyzer | Index health roll-up kept for rows older releases wrote; nothing emits it today. |
| <a id="temp_spill"></a>`temp_spill` | Analyzer | A statement writing temporary files over the interval: `work_mem` is too small for it. |
| <a id="clone_schemas"></a>`clone_schemas` | Analyzer | A family of schemas cloned from one template, reported once instead of per schema. |
| <a id="app_managed_index"></a>`app_managed_index` | Analyzer | An index an application or migration tool manages: pg_sage will not drop it. |
| <a id="query_hint_retirement"></a>`query_hint_retirement` | Query tuner | A plan hint that no longer helps, proposed for removal (`tuner.hint_retirement_days`). |
| <a id="tuning_index_drop"></a>`tuning_index_drop` | Tuning agent | An index the tuning agent proposes to drop. |
| <a id="tuning_index_replace"></a>`tuning_index_replace` | Tuning agent | An index the tuning agent proposes to replace with a better one. |
| <a id="query_create_statistics"></a>`query_create_statistics` | Tuning agent | Extended statistics the tuning agent proposes. |
| <a id="table_tuning"></a>`table_tuning` | Tuning agent | A per-table storage parameter the tuning agent proposes. |
| <a id="forecast"></a>`forecast` | Forecaster | Any category starting with `forecast`: a trend projected past a limit. The `forecaster` filter matches the whole family. |
| <a id="forecast_disk_growth"></a>`forecast_disk_growth` | Forecaster | Database size growing faster than `forecaster.disk_warn_growth_gb_day`. |
| <a id="forecast_connection_saturation"></a>`forecast_connection_saturation` | Forecaster | Connections trending toward `max_connections`. |
| <a id="forecast_query_volume"></a>`forecast_query_volume` | Forecaster | Query volume growing sharply over the lookback. |
| <a id="forecast_cache_pressure"></a>`forecast_cache_pressure` | Forecaster | The cache hit ratio trending below `forecaster.cache_warn_threshold`. |
| <a id="forecast_checkpoint_pressure"></a>`forecast_checkpoint_pressure` | Forecaster | Requested checkpoints trending above `forecaster.checkpoint_requested_per_hour`. |
| <a id="forecast_wraparound_runway"></a>`forecast_wraparound_runway` | Runway | Time left before a table or the database reaches transaction ID wraparound. |
| <a id="forecast_wal_runway"></a>`forecast_wal_runway` | Runway | Time left before retained WAL fills the disk (`forecaster.disk_capacity_bytes`) or a slot's limit. |
| <a id="forecast_sequence_runway"></a>`forecast_sequence_runway` | Runway | Time left before a sequence runs out of values at its current rate. |
| <a id="forecast_multixact_member_runway"></a>`forecast_multixact_member_runway` | Runway | Time left before multixact member space reaches the aggressive-vacuum threshold (half of the 2^32 member space), from a sampled estimate and its error ([Multixact member space](configuration.md#multixact-member-space)). |
| <a id="managed_parameter_drift"></a>`managed_parameter_drift` | Cloud telemetry | A managed provider parameter that drifts from its resolved target. |
| <a id="managed_storage_runway"></a>`managed_storage_runway` | Cloud telemetry | Time left before a managed instance's provisioned storage fills. |
| <a id="instance_rightsize"></a>`instance_rightsize` | Cost | An instance that is over or under its size on 14 days of host usage. Advisory only: info severity, no SQL, and no executor path acts on it ([below](#instance-right-sizing)). |
| <a id="cnpg_target_unconfigured"></a>`cnpg_target_unconfigured` | CloudNativePG | A self-managed database that looks like a CloudNativePG Cluster (the instance manager's `archive_command`, or `allow_alter_system = off`) but no `cnpg` block names it; see [Managed clouds](managed-clouds.md). Warning, no SQL. |
| <a id="incident"></a><a id="incident_open"></a><a id="incident_resolved"></a>`incident` | RCA | Incident-class rows; `incident_open` and `incident_resolved` mark an incident's state. |
| <a id="migration_safety"></a>`migration_safety` | Migration advisor | A migration safety rule's finding on a DDL statement. |
| <a id="upgrade_readiness"></a>`upgrade_readiness` | Upgrade readiness | A major-version upgrade check (U1-U7) that does not pass; see [Upgrade Readiness](#upgrade-readiness). |
| <a id="post_upgrade"></a>`post_upgrade` | Upgrade readiness | A step of the plan pg_sage proposes after a major version upgrade: an `ANALYZE` of a table, or a `REINDEX INDEX CONCURRENTLY` of an index whose collation version changed; see [After the upgrade](#after-the-upgrade). |
| <a id="test_fixture_cleanup"></a>`test_fixture_cleanup` | Facts | A confirmed test-fixture fact's cleanup batch for an operator to review. pg_sage never drops a schema itself. |
| <a id="schema_lint"></a>`schema_lint` | Schema lint | `schema_lint:<rule id>`: a schema lint rule's finding. |
| <a id="agent_posture"></a>`agent_posture` | Agent posture | `agent_posture:<detector>`: an agent posture detector's finding (AP-01 to AP-17; AP-17 is an agent-like session connected as an owner or service role). The exposure detectors also run in a sweep between cycles; see [Agent governance](agent-governance.md#where-you-see-it). |
| <a id="agent_environment_binding"></a>`agent_environment_binding` | Agent guard | An agent connected to a database outside its bound environment. |
| <a id="agent_role_drift"></a>`agent_role_drift` | Agent guard | An agent role whose access differs from what governance recorded. |
| <a id="agent_backend_violation"></a>`agent_backend_violation` | Agent guard | A connected agent role that breaks the agent role rules. |
| <a id="agent_role_upkeep"></a>`agent_role_upkeep` | Agent guard | A scheduled agent role job that cannot run. |
| <a id="autonomy_handoff"></a>`autonomy_handoff` | Executor | A remediation pg_sage earned but holds for an operator's approval. |
| <a id="audit_chain_broken"></a>`audit_chain_broken` | Audit job | The audit log's hash chain does not verify. |
| <a id="audit_chain_regressed"></a>`audit_chain_regressed` | Audit job | An audit trail that went back behind its SIEM export. |
| <a id="audit_chain_verification_failed"></a>`audit_chain_verification_failed` | Audit job | The audit chain verification job could not run. |
| <a id="siem_event_undeliverable"></a>`siem_event_undeliverable` | Audit job | An audit event a SIEM sink refused. |
| <a id="auth_email_case_duplicates"></a>`auth_email_case_duplicates` | Schema migration | User accounts whose emails differ only in case, which block case-insensitive sign-in until an admin merges or renames them. |
| <a id="sre_action"></a>`sre_action` | SRE agent | A backend cancel the SRE agent proposes; it waits for an operator to approve or deny it. |
| <a id="sre_maintenance"></a>`sre_maintenance` | SRE agent | Operator-requested extended statistics, queued for approval. |

### Instance right-sizing

`instance_rightsize` is a deterministic rule over `sage.host_usage_hourly`, the hourly roll-up
of cloud telemetry (RDS, Aurora provisioned and Cloud SQL). It judges only the current size:
a resize starts a new window. It needs at least 14 days of hourly rows covering 90% or more of
the hours, with the newest hour under two hours old; otherwise the size is `unknown` and any
open finding is left as it is.

- **Upsize** one size in the family when the CPU p95 reached 85% on 3 of the last 7 days, or
  the DB load p95 exceeds the vCPU count.
- **Downsize** one size in the family only when the CPU p95 projected onto the smaller size
  (scaled by vCPUs) is at most 60%, the projected peak at most 85%, and the smaller size's RAM
  holds `shared_buffers` plus the used-memory p95. It never downsizes onto a burstable class
  (`db.t3`, `db.t4g`) or to a single-vCPU Cloud SQL tier, and with memory use unknown it keeps
  the size.
- Serverless instances (Aurora Serverless, Lakebase autoscaling) and classes without a
  documented vCPU count are `unknown: capacity telemetry not collected` or `unknown` and never
  produce a finding.

With `cost.rates` set for the instance, the finding's detail carries the monthly compute delta
(the configured compute price scaled linearly by vCPUs), its currency, `rate_source` and
`as_of`; `estimated_cost_usd` holds the delta only when the rates are in USD. Without rates
there is no dollar figure. The finding is info severity with no recommended SQL: pg_sage never
resizes an instance, and an LLM may only narrate a finding this rule produced. A later `keep`
verdict resolves it.

## Upgrade Readiness

`upgrade_readiness` findings are the major-version upgrade case. Once a day,
on a database whose major is past or within 90 days of its community end of
life (where AP-10 reports it), pg_sage checks an upgrade to the newest major
it knows via `pg_upgrade`. Run any other target on demand with
`GET /api/v1/upgrade-readiness` or the MCP tool `upgrade_readiness`.

Each check is `pass`, `warn`, `block` or `unknown`. A check that does not pass
is one finding and one case: `block` is a warning, `warn` and `unknown` are
info. The finding's detail holds the check's reason, up to 50 evidence rows
and the total. The case is advisory: it never has an action candidate, and
pg_sage runs no DDL for it. When the next daily run finds a check passing, its
finding resolves.

| Check | What it reads | Verdict |
|-------|---------------|---------|
| U1 | User tables (`relkind` r/p, permanent) with REPLICA IDENTITY NOTHING, or with the default replica identity and no primary key | `block` for `blue_green` and `logical` (logical replication stops at them), `warn` for `pg_upgrade`. REPLICA IDENTITY FULL or USING INDEX passes. |
| U2 | Installed extensions (not plpgsql) against `pg_available_extension_versions` on `target_database` | `block` when the target lacks one, `warn` when it lacks the installed version, `unknown` with the installed list when no `target_database` (a fleet member already at the target major) is named. |
| U3 | This database's logical slots and subscriptions (`subconninfo` is never read) | `block` for `pg_upgrade` from below 17 with slots (they disappear); `warn` for subscriptions from below 17 and for `blue_green`/`logical`; `pass` for `pg_upgrade` from 17+. |
| U4 | `data_checksums` | `block` when off and the target is 18+ via `pg_upgrade`: initialise with `initdb --no-data-checksums`, or run `pg_checksums --enable` offline first. |
| U5 | `datcollversion` against `pg_database_collation_actual_version` (PG15+), ICU `collversion` against `pg_collation_actual_version`, and for an 18+ target on a non-libc database the GIN/GiST indexes on tsvector expressions or with `gin_trgm_ops`/`gist_trgm_ops` | `warn`: reindex them after the upgrade. Arms that do not apply are listed in `skipped` with the reason. |
| U6 | Non-default settings (`pg_settings.source`, which managed providers expose) removed or renamed between the source and target: `stats_temp_directory` (15), `force_parallel_mode`, `vacuum_defer_cleanup_age`, `promote_trigger_file` (16), `old_snapshot_threshold`, `db_user_namespace` (17) | `warn`. |
| U7 | The provider's dated support table: the community end of life for `self-managed`, the RDS and Aurora standard-support calendars for `rds` and `aurora`; each row names its source URL and read date | `block` when the target is already out of support, `warn` when it leaves support within a year, `unknown` for a provider or major without a dated row, never `pass`. |

A check that cannot read (for example permission denied) is `unknown` with
its SQLSTATE; the other checks still run. All reads run in one read-only
REPEATABLE READ transaction under a statement timeout.

### Upgrade brief

`GET /api/v1/databases/{db}/upgrade-brief?target_major=&method=&target_database=`
(or the MCP tool `upgrade_readiness` with `brief: true`) turns one readiness run
into a dated plan for the database. `target_major` defaults to the newest
community major pg_sage knows. The brief has:

| Section | What it says |
|---------|--------------|
| `blockers` | Every check that blocks, plus `hypopg` on Azure for an in-place upgrade (Azure's precheck refuses it). `summary` is `no blockers found` when there are none; `unknown_checks` names checks that could not tell. |
| `extension_path` | Each installed extension with the versions `target_database` offers and its path (carried, `ALTER EXTENSION ... UPDATE` after, or not offered), or `unknown` without a member at the target major. |
| `slot_plan` | Each logical slot (with its consumer from a confirmed `slot_consumer` fact) and subscription, with its step: from below 17 via `pg_upgrade`, "drop and recreate after, consumer re-snapshots" or use blue/green. |
| `collation_reindex` | Indexes on collations whose recorded version changed, and the U5 indexes an 18+ target folds case differently for, each a future `reindex_concurrently` step. |
| `query_time_share` | The 50 busiest tables by their share of this database's `pg_stat_statements` total time (a statement counts toward each table it names), each with up to 5 of its statements. `unknown` without `pg_stat_statements`. It orders the ANALYZE after the upgrade. |
| `cost_of_waiting` | On RDS, Aurora and Azure: months in extended support x vCPUs x 730 h x your `cost.rates[].extended_support_per_vcpu_hour` ([Configuration](configuration.md)), else `unpriced`; `not_applicable` elsewhere. `dba_hours` is always an estimate range with its formula. |
| `checklist` | Ordered steps: resolve blockers, extension path, reindex plan, slot plan, rehearsal, the operator's cutover, post-upgrade steps. |

A section whose read fails is `unknown` with its SQLSTATE. The brief only reads
the monitored database; pg_sage keeps it in `sage.upgrade_briefs` only when it
changed (`stored`, `brief_id`, and `stored_at`, the brief's age), so a resolved
blocker adds a row and keeps the old one. `query_time_share` is not part of that
comparison (it moves with every statement): a known share is refreshed on the
latest row instead. The upgrade itself stays with the operator.

#### Upgrade rehearsal

Plans flip after a major upgrade because, before PG18, `pg_upgrade` does not
carry the planner statistics. An upgrade rehearsal measures that on a clone,
never on the monitored database:

1. Read the database's top 50 statements by total time from production's
   `pg_stat_statements` (queries only; `BEGIN`, `SET` and other utility
   statements are left out) in a bounded read-only transaction.
2. Create a clone under a receipt of purpose `rehearsal` (so the reaper
   destroys it if pg_sage dies mid-rehearsal). A full-copy provider needs
   `clone.allow_full_copy`, and the receipt carries the cost label, as for a
   restore drill.
3. Capture each statement's plan and shared blocks per call on the clone.
   Only a statement `explain.ReadOnlyStatement` admits is sent to the clone, and
   it is executed with `EXPLAIN (ANALYZE, BUFFERS)` only when the analyze guard
   proves it read-only, in a READ ONLY rolled-back transaction under a 5 s
   timeout. Any other statement is listed as `skipped` and never executed. A
   statement with `$n` parameters is planned with `EXPLAIN (GENERIC_PLAN)` when
   both majors are PG16 or later, else it is `not_comparable`.
4. Upgrade the clone. Aurora and RDS upgrade the clone cluster or instance
   (`ModifyDBCluster` / `ModifyDBInstance`, the newest engine version AWS offers
   at the target major), and only when a live `rehearsal` receipt names it and it
   is not the source. Neon cannot change a project's major, so a Neon rehearsal
   is refused. A self-managed restore command upgrades it through the
   operator's `upgrade_argv` (given `SAGE_CLONE_ID` and `SAGE_TARGET_MAJOR`).
   The clone must then answer at the target major.
5. Capture again with no `ANALYZE` ("as cut over"), run `ANALYZE` on the clone
   (bounded at 30 minutes), capture a third time, and destroy the clone.

The report (the brief's `rehearsal` section, carried onto later briefs of the
same target) lists every statement whose plan fingerprint changed (`changed`,
with `blocks_per_call_delta_as_cut_over` and `..._after_analyze`), the counts
`plans_changed` and `recovered_after_analyze`, the statements not compared and
why, each recorded state (`requested`, `cloned`, `upgraded`, `analyzed`,
`destroyed`, or `failed` with the provider's message), and when the upgrade and
`ANALYZE` ran and how long they took. One rehearsal runs per database at a time.
Supabase branches hold no data, so they cannot rehearse; a database with no
clone provider gets the brief only.

The rehearsal is built into the sidecar but not yet exposed: no API route, MCP
tool or `config.yaml` key starts one, and the restore command's `upgrade_argv`
cannot be set from the configuration yet. Until that wiring lands, the brief
carries a `rehearsal` section only when the sidecar's code stores one.

#### After the upgrade

When the change feed records a major version upgrade (the `server_version`
source, `version_change` with `version:major:...`), pg_sage proposes one plan per
database and new version (`origin_ref` `post_upgrade:<database>:<server_version_num>`;
see [Plans](architecture.md)). It reads the change feed of the last 7 days on each
executor cycle, on the fleet leader of a writable primary. The plan is proposed,
never approved: a person approves it, and every step runs through the executor.

1. `analyze_table`: one `ANALYZE` of every table pg_sage's role may analyze
   (from PG17 the `MAINTAIN` privilege; before it ownership, the database owner or
   a superuser), ordered by the stored brief's `query_time_share` (the latest brief
   of the database, its target first), then the tables not in it by size, largest
   first. `pg_stat_statements` starts empty after `pg_upgrade`, so the live view
   cannot order it. Tables above `tuner.analyze_max_table_mb` (a partitioned
   table counts its partitions) are left out, as the executor leaves its own
   ANALYZE findings, and the statement stops at 10,000 characters; the tables left
   out are operator text. On PostgreSQL 18 and later there is no ANALYZE step, with the reason
   "statistics carried over" (extended statistics are not: ANALYZE the tables
   that have them).
2. `reindex_concurrently`: one `REINDEX INDEX CONCURRENTLY` per index in the
   stored brief's `collation_reindex` whose collation version still differs on
   the upgraded server, each after step 1.

Each step cites its own open `post_upgrade` finding, targets the brief's
statements on its tables (else the live top statements), and measures time per
call. The executor judges an ANALYZE by the rows modified since the last one,
which `pg_upgrade` resets, so an inconclusive or unverifiable verdict keeps the
ANALYZE step and the plan moves on; a regression halts the plan and asks.
What pg_sage cannot run is logged as an operator step instead: `vacuumdb
--analyze-in-stages` when the role may not ANALYZE some tables or no target
statement is known, `REINDEX INDEX CONCURRENTLY ...` for an index the role does
not own or past the plan's 50 steps. If the plan cannot be stored, the failure is
logged and the next cycle tries again. This needs the SRE change feed (the
provider-event sources run even with `sre.change_events.feed_enabled: false`, but
only where the SRE read policy lets the feed read the database).
