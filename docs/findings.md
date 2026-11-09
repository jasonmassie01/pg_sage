# Findings Catalog

pg_sage detects issues across 16+ categories using deterministic rules (Tier 1) plus LLM-powered index optimization (Tier 2). Each finding includes a severity level, human-readable title, detailed description, recommendation, and (where applicable) remediation SQL with rollback SQL.

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
| `config_data` | the advisor's config features and the RCA idle-in-transaction signal |

The next cycle that reads the category resumes them.

!!! tip "See also: [Hint Verification](hint-verification.md)"
    EXPLAIN before-and-after output for every optimization pg_sage applies, proving each hint and index change produces the intended plan improvement.

---

## Source filter and risk fields

`GET /api/v1/findings?source=<subsystem>` filters by the producer that emitted a finding:
`rules`, `advisor`, `optimizer`, `query_tuning`, `forecaster`, `incident` or `schema_lint`
(see the category table in [ui-redesign-v2.md §16](ui-redesign-v2.md)). Each row reports
the same value as `subsystem`. An unknown `source` value applies no filter, and
`migration_advisor` is reserved and returns no rows.

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

## Query Performance

### slow_query

Detects queries with mean execution time above the threshold.

**Severity:** warning (critical if extremely slow)

**What it detects:** Queries from `pg_stat_statements` where `mean_exec_time` exceeds the configured threshold (default 1000ms).

**Example output:**

```
Slow query (mean 3500ms, 15000 calls): SELECT * FROM orders WHERE ...
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

Detects queries whose performance has degraded compared to previous snapshots.

**Severity:** warning

**What it detects:** Queries where `mean_exec_time` has increased significantly between consecutive snapshots, indicating a performance regression.

**Example output:**

```
Query regression: mean_exec_time increased 340% (120ms -> 528ms) for queryid 1234567890
```

**Recommended action:** Investigate recent changes -- schema modifications, data volume changes, or configuration updates.

---

### seq_scan_heavy

Detects sequential scans on large tables.

**Severity:** info (warning if frequent)

**What it detects:** Tables exceeding the configured row threshold that are accessed primarily via sequential scans rather than index scans.

**Example output:**

```
Sequential scan heavy: public.events (2.5M rows, 95% seq_scan ratio)
```

**Recommended action:** Analyze query patterns and add appropriate indexes.

---

## Sequences

### sequence_exhaustion

Detects sequences approaching their maximum value.

**Severity:** critical

**What it detects:** Sequences where the current value is a high percentage of the maximum for their data type. Integer sequences (max ~2.1 billion) are flagged earlier than bigint sequences.

**Example output:**

```
Sequence public.orders_seq at 93.1% capacity (integer)
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

**Severity:** warning (critical for XID wraparound risk)

**What it detects:** Tables with high dead tuple ratios indicating VACUUM is not keeping up.

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

**Severity:** warning (SAFE-tier action)

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

Detects tables approaching XID wraparound threshold.

**Severity:** critical

**What it detects:** Tables where the transaction ID age is approaching the wraparound limit, risking database shutdown.

**Example output:**

```
XID age for public.orders approaching wraparound (1.8 billion)
```

**Recommended action:** Run VACUUM FREEZE immediately.

```sql
VACUUM FREEZE public.orders;
```

---

### wraparound_freeze

Detects tables whose own transaction age is past
`analyzer.wraparound_freeze_xid_age` (default 150,000,000).

**Severity:** warning from the threshold, critical from 200,000,000.

**What it detects:** The older of `age(relfrozenxid)` of the table and of
its TOAST table, so a TOAST relation near wraparound is not hidden behind a
young heap. The detail also carries `mxid_age` (multixact age).

Partitioned parents are never flagged: they have no storage of their own
(`relfrozenxid` is 0, which PostgreSQL reports as age 2,147,483,647), and a
VACUUM of a parent recurses into every partition. pg_sage records their age
as unknown (`xid_age: -1`) and assesses each partition on its own.

**Example output:**

```
Table public.events_2026_10 transaction age is 210000000 — freeze to avoid wraparound
```

**Recommended action:** Run VACUUM (FREEZE) on the table (safe tier; it can
run unattended once the class has earned trust).

```sql
VACUUM (FREEZE) "public"."events_2026_10";
```

Whatever produced it, a VACUUM whose target is a partitioned table, names no
single table, or whose target cannot be checked is queued for operator
approval instead of running unattended. A finding whose table was dropped is
skipped as stale.

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
previous snapshot, each `null` when it is unknown.

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

**Example output:**

```
pg_sage monitoring used 4200 ms of database time per collector cycle on lifeos (budget 3000 ms)
```

**Recommended action:** Look at the `pg_stat_statements` entries that contain
`/* pg_sage */`, sorted by `total_exec_time`, to see which component costs most; lengthen
`collector.interval_seconds` or `analyzer.interval_seconds`, or raise
`analyzer.self_cost_budget_ms` if the cost is expected.

---

### sage_self_budget

Detects pg_sage exceeding the budget it declares for itself.

**Severity:** warning

**What it detects:** pg_sage used more than its `self_budget` of one or more resources
on this database: the sidecar process's CPU per collector cycle
(`self_budget.cpu_ms_per_cycle`, default 600 ms, 1% of one core at the default 60 s
interval), its own statements' database time per hour
(`self_budget.db_time_ms_per_hour`, off by default because
`analyzer.self_cost_budget_ms` already budgets it per cycle), the shared blocks they
touch per hour (`self_budget.blocks_per_hour`, default 18,000,000) and the size of the
`sage` schema (`self_budget.storage_mb`, default 10240). One finding lists every
resource over budget with what it used, and names the top consumers: pg_sage's five
costliest statements of the window (database time, calls, blocks; text shown without
the tag) and its busiest loops (collector and analyzer busy time). CPU is a process
figure: when one sidecar monitors several databases, each database's finding reports
the same process CPU. A resource that could not be measured (first cycle after a
start, no `pg_stat_statements`) keeps the open finding open; `0` disables a resource.
The same figures are exported as `pg_sage_self_cpu_ms_per_cycle`,
`pg_sage_self_db_time_ms_per_hour`, `pg_sage_self_blocks_per_hour`,
`pg_sage_self_budget{resource}`, `pg_sage_self_budget_exceeded{database,resource}` and
`pg_sage_self_loop_busy_seconds_total{loop}`. Per-cycle figures are per cycle of the
database's own collector (its period in fleet mode, which self-configuration may lengthen).
When the database-time budget is inherited from `analyzer.self_cost_budget_ms`, each database
inherits it at its own collector period, exported as
`pg_sage_self_budget_database{database,resource="db_time"}`, and that database's `db_time`
exceeded flag compares with it.

**Example output:**

```
pg_sage monitoring exceeded its own cpu, storage budget on lifeos
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

## Tier 2 -- LLM Index Optimizer

When LLM is enabled, the optimizer generates additional findings of category `index_recommendation`. These are consolidated index recommendations that have passed through 8 validators and confidence scoring. Each recommendation includes:

- The CREATE INDEX statement (always uses CONCURRENTLY)
- Confidence score (0.0-1.0)
- Action level (autonomous / advisory / informational)
- HypoPG cost reduction (when available)
- Write impact assessment

Optimizer findings do not appear without an LLM endpoint configured.
