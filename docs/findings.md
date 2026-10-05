# Findings Catalog

pg_sage detects issues across 16+ categories using deterministic rules (Tier 1) plus LLM-powered index optimization (Tier 2). Each finding includes a severity level, human-readable title, detailed description, recommendation, and (where applicable) remediation SQL with rollback SQL.

Severities: **critical** > **warning** > **info**

!!! tip "See also: [Hint Verification](hint-verification.md)"
    EXPLAIN before-and-after output for every optimization pg_sage applies, proving each hint and index change produces the intended plan improvement.

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
`unused_since` and `stats_epoch`. Before an autonomous `DROP INDEX`, the executor re-checks
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

---

### missing_fk_index

Detects foreign key columns that lack a supporting index on the referencing table.

**Severity:** warning

**What it detects:** Foreign keys where the referencing column(s) have no matching index. This causes sequential scans on JOINs and cascading DELETE operations.

**Example output:**

```
Missing index on public.order_items(order_id) for FK to public.orders
```

**Recommended action:** Create an index on the FK column(s).

```sql
CREATE INDEX CONCURRENTLY idx_order_items_order_id ON public.order_items (order_id);
```

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

Detects low buffer cache hit ratio.

**Severity:** critical (if below threshold)

**What it detects:** Cache hit ratio below the expected level, indicating insufficient `shared_buffers` or a working set that exceeds available memory.

**Example output:**

```
Cache hit ratio 87% (expected > 95%)
```

**Recommended action:** Increase `shared_buffers` or investigate workload changes.

---

### sage_footprint

Detects pg_sage itself taking too much of the database it guards.

**Severity:** warning

**What it detects:** The `sage` schema (every table with its TOAST data and indexes)
is larger than `retention.sage_size_warning_pct` percent (default 10) of the database
size collected this cycle. The finding names the share, the limit and the five largest
sage tables. `0` disables the check; while the database size is unknown the open
finding is kept, not resolved.

**Example output:**

```
Sage data uses 23.4% of database lifeos (limit 10%)
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
Sage monitoring used 4200 ms of database time per collector cycle on lifeos (budget 3000 ms)
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
`pg_sage_self_loop_busy_seconds_total{loop}`.

**Example output:**

```
Sage monitoring exceeded its own cpu, storage budget on lifeos
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

**Severity:** warning or critical

**What it detects:** Standby servers falling behind the primary, measured via `pg_stat_replication`.

**Example output:**

```
Replication lag: 45 seconds behind primary
```

**Recommended action:** Investigate network issues, standby I/O capacity, or heavy write load on primary.

---

### inactive_slot

Detects inactive replication slots consuming WAL.

**Severity:** warning

**What it detects:** Replication slots without active consumers, causing WAL to accumulate and disk usage to grow.

**Example output:**

```
Replication slot 'old_subscriber' is inactive (consuming 12 GB WAL)
```

**Recommended action:** Drop the inactive slot if the consumer is permanently gone.

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
