# Hint Verification: EXPLAIN Before & After

pg_sage's tuner detects query plan symptoms and prescribes [pg_hint_plan](https://github.com/ossc-db/pg_hint_plan) directives or DDL fixes. This document shows the **actual EXPLAIN output** before and after each optimization, proving the hints produce the intended plan changes.

All tests run against PostgreSQL 17 with pg_hint_plan 1.7.1 on a local Docker container. Source: [`sidecar/test/hint_verify/hint_verify_test.go`](https://github.com/jasonmassie01/pg_sage/blob/main/sidecar/test/hint_verify/hint_verify_test.go)

---

## Test Environment

| Component | Version |
|-----------|---------|
| PostgreSQL | 17 |
| pg_hint_plan | 1.7.1 |
| pg_stat_statements | 1.11 |
| Container defaults | `work_mem=4MB`, `max_parallel_workers_per_gather=0` |

**Schema:** 10K customers, 100K orders, 5K products, 200K line items, 500K events.

---

## 1. Seq Scan With Index &rarr; `IndexScan(table index)`

**Symptom:** Planner chooses sequential or bitmap scan on a table that has a usable index.

**Hint:** `/*+ IndexScan(orders idx_orders_status) */`

??? example "EXPLAIN output"

    **BEFORE** &mdash; Bitmap Heap Scan (planner's default choice):

    ```
    Bitmap Heap Scan on orders  (cost=282.74..1384.37 rows=25090 width=30)
      Recheck Cond: (status = 'pending'::text)
      ->  Bitmap Index Scan on idx_orders_status  (cost=0.00..276.47 rows=25090 width=0)
            Index Cond: (status = 'pending'::text)
    ```

    **AFTER** &mdash; Index Scan forced by hint:

    ```
    Index Scan using idx_orders_status on orders  (cost=0.29..3501.87 rows=25090 width=30)
      Index Cond: (status = 'pending'::text)
    ```

**Verdict:** Hint overrides the planner's bitmap strategy and forces a direct index scan.

---

## 2. Disk Sort &rarr; `Set(work_mem "NMB")`

**Symptom:** Sort spills to disk because `work_mem` is too low.

**Fix:** Raise `work_mem` so the sort fits in memory.

??? example "EXPLAIN ANALYZE output"

    **BEFORE** &mdash; `work_mem=256kB`, sort spills 119 MB to disk:

    ```
    Sort  (cost=287326.92..288576.92 rows=500000 width=226)
          (actual time=5581.664..5631.182 rows=500000 loops=1)
      Sort Key: payload
      Sort Method: external merge  Disk: 119464kB
      Buffers: shared hit=12252 read=3996, temp read=59709 written=60371
      ->  Seq Scan on big_events  (cost=0.00..21248.00 rows=500000 width=226)
            (actual time=0.004..40.692 rows=500000 loops=1)
    Execution Time: 5653.086 ms
    ```

    **AFTER** &mdash; `work_mem=256MB`, sort completes in memory:

    ```
    Sort  (cost=68576.92..69826.92 rows=500000 width=226)
          (actual time=4437.704..4500.274 rows=500000 loops=1)
      Sort Key: payload
      Sort Method: quicksort  Memory: 134388kB
      Buffers: shared hit=12284 read=3964
      ->  Seq Scan on big_events  (cost=0.00..21248.00 rows=500000 width=226)
    Execution Time: 4515.701 ms
    ```

**Verdict:** `external merge Disk: 119MB` &rarr; `quicksort Memory: 131MB`. Execution time drops ~20%.

---

## 3. Hash Spill &rarr; `Set(work_mem "NMB")`

**Symptom:** Hash join uses multiple batches, spilling intermediate results to disk.

**Fix:** Raise `work_mem` so the hash table fits in a single batch.

??? example "EXPLAIN ANALYZE output"

    **BEFORE** &mdash; `work_mem=64kB`, hash uses 16 batches:

    ```
    Hash Join  (cost=1787.57..7248.60 rows=48975 width=8)
          (actual time=4.773..43.504 rows=50000 loops=1)
      Hash Cond: (l.order_id = o.id)
      Buffers: shared hit=2062 read=23, temp read=725 written=725
      ->  Seq Scan on lineitems l
      ->  Hash  (cost=1379.16..1379.16 rows=24833 width=4)
            (actual time=4.653..4.654 rows=25000 loops=1)
            Buckets: 4096  Batches: 16  Memory Usage: 89kB
    Execution Time: 44.778 ms
    ```

    **AFTER** &mdash; `work_mem=128MB`, single batch:

    ```
    Hash Join  (cost=1689.57..5488.60 rows=48975 width=8)
          (actual time=3.311..25.876 rows=50000 loops=1)
      Hash Cond: (l.order_id = o.id)
      Buffers: shared hit=2085
      ->  Seq Scan on lineitems l
      ->  Hash  (cost=1379.16..1379.16 rows=24833 width=4)
            (actual time=3.293..3.295 rows=25000 loops=1)
            Buckets: 32768  Batches: 1  Memory Usage: 1135kB
    Execution Time: 27.199 ms
    ```

**Verdict:** `Batches: 16` &rarr; `Batches: 1`. No more temp I/O. Execution time drops 40%.

---

## 4. Bad Nested Loop &rarr; `HashJoin(alias)`

**Symptom:** Planner picks Nested Loop with inaccurate row estimate, causing excessive iterations.
The tuner flags it only when the row estimate is off by more than 10x (on the join or its outer
side) **and** the inner side read at least `tuner.nested_loop_row_threshold` rows in total
(`Actual Rows` x `Actual Loops`, default 10,000). A small nested loop with a bad estimate is cheap
either way and gets no hint (or, on a stale table, no ANALYZE). The rule needs a plan with actual
row counts (auto_explain with `log_analyze`); a plain EXPLAIN plan never triggers it. The hint
is offered only when the join has an equality between the two inputs (here
`Index Cond: (id = o.customer_id)`); a join that is only an OR cannot be hash-joined and gets no
hint (see `or_join_filter` below).

**Hint:** `/*+ HashJoin(c o) */`

??? example "EXPLAIN output"

    **BEFORE** &mdash; Nested Loop with Memoize (hash join disabled for baseline):

    ```
    Nested Loop  (cost=0.30..7380.60 rows=25000 width=15)
      ->  Seq Scan on orders o  (cost=0.00..1788.00 rows=100000 width=10)
      ->  Memoize  (cost=0.30..0.32 rows=1 width=13)
            Cache Key: o.customer_id
            ->  Index Scan using customers_pkey on customers c
                  Index Cond: (id = o.customer_id)
                  Filter: (region = 'east'::text)
    ```

    **AFTER** &mdash; Hash Join forced by hint:

    ```
    Hash Join  (cost=240.25..2290.86 rows=25000 width=15)
      Hash Cond: (o.customer_id = c.id)
      ->  Seq Scan on orders o  (cost=0.00..1788.00 rows=100000 width=10)
      ->  Hash  (cost=209.00..209.00 rows=2500 width=13)
            ->  Seq Scan on customers c
                  Filter: (region = 'east'::text)
    ```

**Verdict:** Nested Loop &rarr; Hash Join. Estimated cost drops from 7380 to 2290.

---

## 5. Parallel Disabled &rarr; `Set(max_parallel_workers_per_gather "4")`

**Symptom:** Large sequential scan runs single-threaded because `max_parallel_workers_per_gather=0`.

**Fix:** Enable parallel workers.

??? example "EXPLAIN output"

    **BEFORE** &mdash; Serial scan:

    ```
    Aggregate  (cost=24164.67..24164.68 rows=1 width=8)
      ->  Seq Scan on big_events  (cost=0.00..23748.00 rows=166667 width=0)
            Filter: (length(payload) > 200)
    ```

    **AFTER** &mdash; Parallel with 3 workers:

    ```
    Finalize Aggregate  (cost=19802.08..19802.09 rows=1 width=8)
      ->  Gather  (cost=19801.76..19802.07 rows=3 width=8)
            Workers Planned: 3
            ->  Partial Aggregate  (cost=18801.76..18801.77 rows=1 width=8)
                  ->  Parallel Seq Scan on big_events
                        Filter: (length(payload) > 200)
    ```

**Verdict:** Serial scan &rarr; Gather with 3 parallel workers. Estimated cost drops 19%.

---

## 6. High Plan Time &rarr; parameterization advice, no hint

**Symptom:** Planning time dominates execution time for a statement called at
least `tuner.min_query_calls` times (`mean_plan_time >
mean_exec_time x tuner.plan_time_ratio`; needs
`pg_stat_statements.track_planning`), or one statement shape reaches
pg_stat_statements as many queryids because each call sends a different
IN-list length.

**Hint:** none. pg_sage used to prescribe
`Set(plan_cache_mode "force_generic_plan")`. pg_hint_plan accepts it, but it
only has an effect on prepared statements: a simple-protocol statement is
planned afresh whatever the setting, and on skewed data a forced generic plan
can be worse than the custom plans it replaces. The tuner no longer installs
it. A `plan_cache_mode` hint may come back only when a hinted EXPLAIN shows
that it changes the plan.

**Finding:** `query_parameterization` (severity `warning`, object type
`query_shape`, identifier `shape:<16 hex digits of the SHA-256 of the
collapsed text>`), advice only, with no SQL. Its detail has
`normalized_query` (the statement with every `IN ($1, ...)` and
`= ANY (ARRAY[$1, ...])` list of bind parameters collapsed to `(...)`),
`reason` (`in_list_variants` or `plan_time_share`), `variant_count`,
`queryids` (at most 20), `calls`, `total_exec_ms`, `plan_time_share` and
`pg_major`. The case it opens offers a `prepare_parameterized_query` script
for review in the application's repository: use prepared statements or a
single array parameter (`= ANY($1)`).

### When the tuner reports query_parameterization

Two triggers:

- **IN-list variants** (`reason=in_list_variants`): the collector's latest
  snapshot holds `tuner.param_shape_min_variants` (default 20, 2-1000) or more
  queryids of one collapsed shape. A subquery (`IN (SELECT ...)`) or a list of
  literals in an unnormalized utility statement is not a parameter list. The
  trigger is off on PostgreSQL 18, whose pg_stat_statements squashes lists into
  one entry.
- **Plan-time share** (`reason=plan_time_share`): the statement's planning
  dominates as above. A shape the IN-list trigger already reported in the
  cycle is not reported twice.

**Upgrade note:** `plan_cache_mode` hints pg_sage installed before this
change stay `active` until hint revalidation or a rollback removes them; no
new ones are installed.

---

## 7. Sort + LIMIT &rarr; CREATE INDEX on sort columns

**Symptom:** `ORDER BY ... LIMIT N` forces a full-table sort before returning N rows.

**Fix:** Create a composite index matching the sort order so PostgreSQL can use an ordered index scan.

??? example "EXPLAIN output"

    **BEFORE** &mdash; Incremental Sort on 500K rows for 10-row limit:

    ```
    Limit  (cost=0.67..2.56 rows=10 width=226)
      ->  Incremental Sort  (cost=0.67..94514.97 rows=500000 width=226)
            Sort Key: score DESC, created_at
            Presorted Key: score
            ->  Index Scan Backward using idx_events_score on big_events
    ```

    **AFTER** &mdash; Direct index scan, no sort:

    ```
    Limit  (cost=0.42..2.03 rows=10 width=227)
      ->  Index Scan using idx_events_score_created on big_events
    ```

    ```sql
    CREATE INDEX idx_events_score_created
      ON big_events (score DESC, created_at);
    ```

**Verdict:** Incremental Sort eliminated. Cost drops from 94K to 2. The composite index serves both the ordering and the limit directly.

---

## 8. Temp Spill (Aggregation) &rarr; `Set(work_mem "NMB")`

**Symptom:** `GROUP BY` with ordered aggregation spills sort data to disk.

**Fix:** Raise `work_mem` to keep the sort in memory.

??? example "EXPLAIN ANALYZE output"

    **BEFORE** &mdash; `work_mem=256kB`, external merge during aggregation:

    ```
    GroupAggregate  (cost=53489.11..276194.03 rows=5 width=38)
          (actual time=316.825..762.689 rows=5 loops=1)
      Group Key: event_type
      Buffers: temp read=41981 written=42609
      ->  Incremental Sort
            Sort Method: external merge  Average Disk: 22564kB  Peak Disk: 22712kB
    Execution Time: 779.333 ms
    ```

    **AFTER** &mdash; `work_mem=512MB`, sort in memory:

    ```
    GroupAggregate  (cost=68576.92..72326.98 rows=5 width=38)
          (actual time=891.582..1073.095 rows=5 loops=1)
      Group Key: event_type
      Buffers: shared hit=16134 read=114
      ->  Sort
            Sort Method: quicksort  Memory: 128568kB
    Execution Time: 1079.225 ms
    ```

**Verdict:** `external merge Disk: 22MB` &rarr; `quicksort Memory: 125MB`. Temp I/O eliminated entirely (0 temp reads/writes).

### How the tuner sizes a stat-based spill hint

When no captured plan shows the sort or hash that spills, the tuner sizes the hint from
`pg_stat_statements`. Its counters are cumulative, so the tuner sizes from the interval
between two tuner cycles, never from the totals:

1. Each cycle it keeps a baseline of `calls` and `temp_blks_written` for each statement and
   role (`queryid`, `userid`). The first sighting of a statement only records the baseline,
   so no `stat_temp_spill` finding appears on the first cycle after startup. A fall in either
   counter (`pg_stat_statements_reset()`) starts a new baseline and gives no finding.
2. The per-call spill is `Δtemp_blks_written × block_size / Δcalls`. `block_size` is read from
   the server each cycle.
3. The baseline `work_mem` is the one the statement's role runs with: its role-in-database
   setting, then its role setting, then the database setting, then the `ALTER ROLE ALL`
   setting (`pg_db_role_setting`), then the server default (`pg_settings.reset_val`).
   pg_sage's own session value is not used. When pg_sage's own login role has a `work_mem`
   setting, or pg_sage passes one as a connection option, the server default is hidden
   from it: a role with no setting of its own then has no known baseline and gets no
   stat-spill hint. A per-call spill at or below the baseline gives no hint.
4. The hint doubles the baseline until it is at least twice the per-call spill. There is no
   64 MB floor. Example: 1,000 calls that wrote 2,048 blocks each spill 16 MB per call; at a
   4 MB baseline the hint is `Set(work_mem "32MB")`. 100,000 calls of 1 MB each give no
   hint at 4 MB.
5. No hint is emitted when the result, after the cap below, is not above the baseline.
6. A hint applies to every role that runs the statement, so no hint is emitted when another
   role seen running the same `queryid` in that cycle has a higher `work_mem`, or one the
   tuner cannot read: the hint would lower that role's `work_mem`.

The finding's `stat_spill_sizing` detail records each number in this arithmetic.

### The per-query work_mem cap

Every work_mem hint, from the deterministic rules or from the tuning agent, is capped at
**min(`tuner.work_mem_max_mb`, 5% of host memory)**. This is the same 5% rule the config
advisor applies to `ALTER SYSTEM SET work_mem`. One hint applies to every sort or hash node
of every concurrent execution, multiplied by `hash_mem_multiplier`.

- Deterministic hints above the cap are clamped to it. The finding's `work_mem_budget` detail
  records the requested size, the cap and the host memory used.
- The tuning agent's hints above 5% of host memory are refused ("above 5% of host memory").
  A request within the cap that the tuner's 64 MB minimum for hint work_mem would lift above
  it (on a host under about 1.3 GB) is set to the cap instead.
  Its prompt cites the computed cap.
- When host memory is unknown (no managed-cloud memory telemetry, for example on a
  self-managed host), only `tuner.work_mem_max_mb` applies. Every work_mem hint is then
  marked `approval_required`, so it waits for an operator.

---

## 9. Missing FK Index &rarr; CREATE INDEX

**Symptom:** Join on a foreign key column triggers a sequential scan because there is no index.

**Fix:** `CREATE INDEX idx_orders_customer ON orders(customer_id)`

??? example "EXPLAIN ANALYZE output"

    **BEFORE** &mdash; Seq Scan on orders for FK lookup:

    ```
    Nested Loop  (actual time=0.014..2.756 rows=10 loops=1)
      ->  Index Scan using customers_pkey on customers c
            Index Cond: (id = 42)
      ->  Seq Scan on orders o  (actual time=0.008..2.741 rows=10 loops=1)
            Filter: (customer_id = 42)
            Rows Removed by Filter: 99990
    Execution Time: 2.777 ms
    ```

    **AFTER** &mdash; Bitmap Index Scan on new index:

    ```
    Nested Loop  (actual time=0.020..0.032 rows=10 loops=1)
      ->  Index Scan using customers_pkey on customers c
            Index Cond: (id = 42)
      ->  Bitmap Heap Scan on orders o  (actual time=0.013..0.023 rows=10 loops=1)
            Recheck Cond: (customer_id = 42)
            ->  Bitmap Index Scan on idx_orders_customer
                  Index Cond: (customer_id = 42)
    Execution Time: 0.052 ms
    ```

**Verdict:** Seq Scan (filtering 99,990 rows) &rarr; Bitmap Index Scan (10 rows directly). **53x faster** (2.78ms &rarr; 0.05ms).

---

## 10. hint_plan.hints Table Integration

pg_hint_plan 1.7.1 supports table-driven hints via `hint_plan.hints`, keyed by `query_id` from `pg_stat_statements`. This is how pg_sage persists hints without modifying application SQL.

??? example "EXPLAIN output"

    **BEFORE** &mdash; No hint in table, planner picks Bitmap:

    ```
    Bitmap Heap Scan on orders
      Recheck Cond: (status = 'shipped'::text)
      ->  Bitmap Index Scan on idx_orders_status
            Index Cond: (status = 'shipped'::text)
    ```

    **Hint inserted:**

    ```sql
    INSERT INTO hint_plan.hints (query_id, application_name, hints)
    VALUES (-8629227083328336691, '', 'IndexScan(orders idx_orders_status)');
    ```

    **AFTER** &mdash; Hint applied transparently:

    ```
    Index Scan using idx_orders_status on orders
      Index Cond: (status = 'shipped'::text)
    ```

**Verdict:** Table-driven hints work without application code changes, but only in sessions
where pg_hint_plan reads its table. `pg_hint_plan.enable_hint_table` defaults to `off`, and
with it off pg_hint_plan ignores `hint_plan.hints` entirely. The test above turns it on for its
own session; application sessions need it set for the database or their role.

### Prerequisites pg_sage checks before it offers an executable hint

A hint in `hint_plan.hints` changes an application's plan only when all four hold for the
sessions that run the statement. pg_sage reads each from the catalog (`pg_settings`,
`pg_db_role_setting`, `pg_class`/`pg_index`, `has_table_privilege`), never with `SET`, `ALTER`
or `LOAD`, at startup and again at the start of every tuner cycle, so a change takes effect
at the next cycle without a restart.

| Fact | Holds when | `hint_readiness` when missing | Fix |
|---|---|---|---|
| Preload | `shared_preload_libraries`, or `session_preload_libraries` for the server, the database or a login role, lists `pg_hint_plan` | `not preloaded` (`shared_preload_libraries unreadable` when pg_sage's role may not see it: grant `pg_read_all_settings`, part of `pg_monitor`) | add `pg_hint_plan` to `shared_preload_libraries` and restart |
| Hint table on | `pg_hint_plan.enable_hint_table`, and the master switch `pg_hint_plan.enable_hint` (on by default), are on for the same sessions: server setting, `ALTER DATABASE ... SET` or `ALTER ROLE ... [IN DATABASE ...] SET`, with PostgreSQL's precedence | `enable_hint_table off`; `enable_hint off` when the table setting is on but the master switch is off; or `preload and enable_hint_table differ by role` when one role preloads and another has the table on | `ALTER DATABASE mydb SET pg_hint_plan.enable_hint_table = on;` (new sessions pick it up); `ALTER DATABASE mydb RESET pg_hint_plan.enable_hint;` when it was turned off |
| Table layout | `hint_plan.hints` has the pg_hint_plan 1.7 layout (`query_id`, `application_name`, `hints`) with its unique index, and pg_sage can read it | `hint table layout` | `CREATE EXTENSION pg_hint_plan;` at 1.7 or later (PG17+) |
| Privilege | pg_sage's role holds `INSERT` and `DELETE` on `hint_plan.hints` (and `USAGE` on schema `hint_plan`, `SELECT` on the table) | `privilege` | `GRANT USAGE ON SCHEMA hint_plan TO sage_agent; GRANT SELECT, INSERT, DELETE ON hint_plan.hints TO sage_agent;` |

When any is missing, `query_tuning` findings stay advisory: they keep the proposed
`hint_directive` but carry no `recommended_sql`, and their `hint_readiness` detail names what
is missing (several joined with `; `). The tuning agent proposes no hints. A failed catalog
read counts as not ready (`probe failed`). The fleet capability report (`hint_plan`) shows the
same facts, with a limitation line while hints have no effect.

On RDS, Aurora and Cloud SQL, `shared_preload_libraries` is set through the parameter group or
database flag; `ALTER DATABASE ... SET pg_hint_plan.enable_hint_table = on` works as the
database owner. pg_sage only reads these settings.

### Every hint is planned before it is proposed

Syntax alone does not prove a hint works: pg_hint_plan silently ignores an
unknown alias or index, and a join or scan hint can make the plan worse. So
before the tuner or the tuning agent proposes a hint, pg_sage plans the
statement twice in one bounded `READ ONLY` transaction (the tuner's catalog
`statement_timeout` and `lock_timeout`): once plain, once with `/*+ hint */`
at the head of the statement. It never runs `ANALYZE`, never writes, and only
turns comment hints on for that transaction; `hint_plan.hints` is ignored
there, so a hint already installed for the statement never plans the plain
side. A parameterized statement is planned with `GENERIC_PLAN` on PostgreSQL
16 and later. Multi-statement text is refused before any `EXPLAIN`.

The two plans are compared by shape (the plan hash) and root total cost:

| Verdict | When | Result |
|---|---|---|
| `changed` | The hint changes the plan, and a join or scan hint keeps the estimated cost within `plain x (1 + tuner.hint_cost_tolerance_pct / 100)` (default 10). A `Set(work_mem)` hint whose plan keeps its shape is `changed` (basis `spill`) when the statement's cached `EXPLAIN ANALYZE` plan spills (a disk sort or more than one hash batch). | Executable hint |
| `no_effect` | The hinted plan is the plain plan, or (pg_hint_plan 1.4-1.6) it rests on a disabled path because the alias or index is unknown | Refused |
| `cost_regression` | A join or scan hint raises the estimated cost past the tolerance | Refused |
| `explain_timeout`, `explain_failed` | An `EXPLAIN` hit its statement or lock timeout, or failed | Refused |
| `unverifiable` | pg_hint_plan is not loaded in pg_sage's session (not preloaded for its role and `LOAD` not available), the statement is parameterized on PostgreSQL 14 or 15, or a `Set(work_mem)` hint has no spill evidence | The finding stays advisory: no `recommended_sql`, and nothing is recorded as `proposed` |

Every compared finding carries `detail.hint_evidence`: `verdict`, `basis`,
`reason`, `plain_cost`, `hinted_cost`, `cost_delta_pct`, `plain_plan_hash`,
`hinted_plan_hash`, `pg_major`, `generic_plan`, `tolerance_pct`, and both plans
(`plain_plan`, `hinted_plan`, each up to 64 KB) for the approval card. The
tuning agent can run the same comparison itself with its `hinted_explain` tool
(at most three per case); a hint it proposes with a `changed` verdict has the
prediction note "plan cost comparison".

Costs are the planner's estimates. The executor's post-apply verification of
the query's latency stays the authority on whether a hint helped. On managed
services pg_hint_plan must be preloaded through the parameter group or flags;
otherwise hints stay advisory. The check runs per database in fleet mode.


---

## 11. Merge Join Hint &rarr; `MergeJoin(alias)`

**Hint:** `/*+ MergeJoin(o l) */`

??? example "EXPLAIN output"

    **BEFORE** &mdash; Hash Join:

    ```
    Hash Join  (cost=52.03..3851.05 rows=1897 width=8)
      Hash Cond: (l.order_id = o.id)
      ->  Seq Scan on lineitems l
      ->  Hash
            ->  Index Only Scan using orders_pkey on orders o
                  Index Cond: (id < 1000)
    ```

    **AFTER** &mdash; Merge Join:

    ```
    Merge Join  (cost=1.87..9707.16 rows=1897 width=8)
      Merge Cond: (o.id = l.order_id)
      ->  Index Only Scan using orders_pkey on orders o
            Index Cond: (id < 1000)
      ->  Index Scan using idx_li_order on lineitems l
    ```

**Verdict:** Hash Join &rarr; Merge Join. Both sides now use ordered index scans.

---

## 12. Combined Hints &rarr; Multiple directives in one comment

pg_sage's `CombineHints()` merges multiple prescriptions into a single hint string. When both a join hint and a work_mem hint apply, they are combined.

**Hint:** `/*+ Set(work_mem "128MB") MergeJoin(c o) IndexScan(o) */`

??? example "EXPLAIN output"

    **BEFORE** &mdash; Hash Join with Seq Scans:

    ```
    HashAggregate  (cost=2859.61..2959.61 rows=10000 width=17)
      ->  Hash Join
            Hash Cond: (o.customer_id = c.id)
            ->  Seq Scan on orders o
            ->  Hash
                  ->  Seq Scan on customers c
    ```

    **AFTER** &mdash; Merge Join with Index Scans:

    ```
    HashAggregate  (cost=7230.03..7330.03 rows=10000 width=17)
      ->  Merge Join
            Merge Cond: (c.id = o.customer_id)
            ->  Index Scan using customers_pkey on customers c
            ->  Index Scan using idx_orders_customer on orders o
    ```

**Verdict:** All three hints applied simultaneously: Merge Join forced, both tables use Index Scan, work_mem raised.

---

## 13. Plan anti-patterns &rarr; advice only, no hint

Two slow-query causes that no hint fixes. The tuner reports them as informational
`query_tuning` findings: no `hint_directive`, no `recommended_sql`, and no row in
`sage.query_hints`. Both need an executed plan (EXPLAIN ANALYZE, for example from
auto_explain); a plain EXPLAIN never yields them. PostgreSQL 14 to 18.

**`index_filter_inefficient`:** an Index Scan, Index Only Scan or Bitmap Heap Scan
whose `Filter` discards at least 90% of the rows the index read, and at least
10,000 rows over all loops (`Rows Removed by Filter` and `Actual Rows` are
per-loop averages, so both are multiplied by `Actual Loops`; a missing loop count
is one loop). The index serves only part of the predicate. The advice names a
composite index: the `Index Cond` columns, then the filter's equality columns.

    Index Scan using ip_a_idx on ip  (actual rows=129 loops=1)
      Index Cond: (a = 5)
      Filter: (b = 7)
      Rows Removed by Filter: 12371

> index ip_a_idx reads 12500 rows per execution and its filter discards 98%; an
> index on (a, b) would read only the kept rows

**`or_join_filter`:** a Nested Loop whose `Join Filter` is a top-level OR with at
least two arms that between them reference both join inputs, discarding at least
10,000 rows over all loops (`Rows Removed by Join Filter` x `Actual Loops`). An OR
that references only one input is not reported.

    Nested Loop  (actual rows=680 loops=1)
      Join Filter: ((a.x = b.x) OR (a.y = b.y))
      Rows Removed by Join Filter: 39320

> the nested loop's join filter discards 39320 rows: the OR across a and b is
> applied after the join; rewriting as UNION (or UNION ALL with an exclusion
> predicate) lets each arm use its own index. pg_sage does not rewrite statements

Neither adds a hint of its own. A misestimated nested loop (`bad_nested_loop`) gets
`HashJoin(...)` only when its join is hashable: an equality between a column of the
outer input and a column of the inner input, as a top-level AND conjunct of the
`Join Filter` or of a parameterized inner scan's `Index Cond`, `Recheck Cond` or
`Filter` (through `Memoize`, `Materialize`, `Append` and a bitmap scan). A join that
is a pure OR, has no join condition, or cannot be parsed has nothing to hash on, so
PostgreSQL cannot build a hash join and pg_hint_plan cannot force one: the symptom
records `hashable_join: false`, no join hint is offered, and the finding's
`join_hint_note` (also appended to its recommendation) says why. The OR advice above
still applies. The 10,000-row thresholds are built in (no configuration key).

---

## Summary

| Case | Symptom | Fix | Plan Change |
|------|---------|-----|-------------|
| Seq Scan w/ Index | Planner ignores index | `IndexScan(t idx)` | Bitmap &rarr; Index Scan |
| Disk Sort | Sort spills 119MB | `work_mem=256MB` | external merge &rarr; quicksort Memory |
| Hash Spill | 16 hash batches | `work_mem=128MB` | Batches: 16 &rarr; 1 |
| Bad Nested Loop | NL with bad estimate | `HashJoin(c o)` | Nested Loop &rarr; Hash Join |
| Parallel Off | Serial scan on 500K rows | `max_parallel_workers_per_gather=4` | Seq Scan &rarr; Parallel Seq Scan |
| High Plan Time / IN-list variants | Re-planning overhead | Advice: prepared statements or `= ANY($1)` | None (advisory) |
| Sort + LIMIT | Full sort for 10 rows | CREATE INDEX (composite) | Sort eliminated |
| Temp Spill | Aggregation spills 22MB | `work_mem=512MB` | external merge &rarr; quicksort Memory |
| Missing FK Index | Seq scan on FK join | CREATE INDEX | Seq Scan &rarr; Bitmap Index (53x) |
| Hint Table | Transparent hint injection | `hint_plan.hints` INSERT | Bitmap &rarr; Index Scan |
| Merge Join | Force join strategy | `MergeJoin(o l)` | Hash Join &rarr; Merge Join |
| Combined Hints | Multiple symptoms | Combined hint string | Hash+Seq &rarr; Merge+Index |
| Index filter discards most rows | Index reads rows its filter drops | Advice: composite index | None (advisory) |
| OR join filter | OR applied after the join | Advice: UNION rewrite | None (advisory) |

---

## Which plan a hint is built from

The tuner and the tuning agent read a statement's plan from
`sage.explain_cache` (never Lakebase plan history), and use a cached plan
only while it still describes the statement. A plan is stale when:

- it is `tuner.plan_max_age_minutes` old or older (default 1440, 24 hours;
  0 or less means the default);
- it was captured before the last `ANALYZE` or autoanalyze of a relation it
  reads (`pg_stat_user_tables`, read for those relations only);
- it was captured before an executed pg_sage action (`sage.action_log`,
  outcome not `failed`) whose finding or `sage.decision.target_objects` names
  one of its relations or indexes, such as a `CREATE INDEX` on its table.

A capture at the same instant as one of these events counts as stale, and so
does a plan whose relation history cannot be read. Of the statement's three
newest captures, the newest fresh executed plan (an `auto_explain_log` plan,
or any plan with actual rows) is used, else the newest fresh estimate
(`explain_estimate` or `generic_plan`): an estimate taken after a logged plan
never hides the plan's actual rows. When none is fresh:

- the tuning agent runs one plain `EXPLAIN` in a bounded read-only
  transaction (`GENERIC_PLAN` on PostgreSQL 16 and later; on 14 and 15 a
  parameterized statement has no plan). A failing `EXPLAIN` is reported as no
  plan; the stale plan is never used in its place;
- the query tuner does the same for a statement that had a cached plan; a
  statement that never had one keeps the stat-based rules only. When the
  tuner cannot plan the statement afresh, it has no plan.

A stale plan never drives a symptom or a hint, but the tuner still reads the
tables it names to defer a statement whose table has a pending index
recommendation: a statement reads the same tables after an `ANALYZE`, so
dropping the stale plan does not let the statement past that gate.

A plan made by plain `EXPLAIN` carries no actuals, so the disk sort, hash
spill and bad nested loop rules, which need `EXPLAIN ANALYZE` output, never
run on it. pg_sage never runs `EXPLAIN ANALYZE` on this path. The tuner reads
each candidate's plan once per cycle. The agent's `explain` tool result and
each `query_tuning` finding (`detail.plan`) carry the plan's `source`,
`capture` (the cache row's own source: `auto_explain_log`, `explain_estimate`
or `generic_plan`), `captured_at` and `age_seconds`, and, when the cached plan
was skipped, `stale_reason`.

## Hint lifecycle

Every hint pg_sage records lives in `sage.query_hints`. Its `status` moves
through the states below. The revalidation loop (`tuner.verify_after_apply`,
every `tuner.revalidation_interval_hours`) reads `active` and `dormant` hints.
It never removes a hint for working or for being old. A hint due for
re-verification (`reverify_due`, after `tuner.hint_retirement_days`) is planned
with and without the hint as above, from its `pg_stat_statements` text; the
costs go to `before_cost`, `after_cost` and `verified_at`, and a `no_effect`
verdict retires it (logged with reason `no_effect`). Any other verdict,
`unverifiable` included, leaves it as it was.

| Status | Meaning | Installed in `hint_plan.hints` |
|--------|---------|--------------------------------|
| `proposed` | The tuner recommended the hint; it waits for approval. | no |
| `active` | The hint is installed and its query ran since the last revalidation. | yes |
| `dormant` | The hint is installed, but its query has not run since the last revalidation, or its queryid is absent from `pg_stat_statements`. | yes |
| `retired` | The query case's retire action retired the hint, or re-verification found it no longer changes the plan (`no_effect`). | removed through the `retire_query_hint` action |
| `broken` | An index the hint names no longer exists. | removed through the `retire_query_hint` action |
| `rolled_back` | The installed hint of an `active` or `dormant` row was removed (rollback or operator). | no |
| `rejected` | An operator rejected the approval of the proposal's `query_tuning` finding. | no |
| `expired` | The proposal waited 30 days without an answer. | no |

Transitions:

| From | To | When |
|------|----|------|
| `proposed` | `active` | The executor installed exactly this hint text for the query (reconciled at the start of each tuner cycle). |
| `active` | `dormant` | A revalidation pass sees no new calls since the last pass, or the queryid is absent from `pg_stat_statements` (an eviction, or a query that stopped running). |
| `dormant` | `active` | A revalidation pass sees the summed calls advance again. |
| `active`, `dormant` | `retired` | A revalidation pass of a hint due for re-verification plans the statement the same with and without it (`no_effect`). |
| `active`, `dormant` | `broken` | A revalidation pass finds an index named in an `IndexScan` / `BitmapScan` directive missing from `pg_class`. |
| `active`, `dormant` | `rolled_back` | The row for the query no longer holds this hint's text: it was removed, or an operator replaced it with their own hint (reconciled at the start of each tuner cycle). |
| `proposed` | `rejected` | An operator (`decided_by` set) rejected an approval of the statement's `query_tuning` finding after the proposal was made. A system expiry of the queue item rejects nothing. Reconciled at the start of each tuner cycle, with or without a hint table. |
| `proposed` | `expired` | The proposal is older than 30 days (the same window the tuning agent keeps operator rejections). Reconciled at the start of each tuner cycle. |

`rejected` and `expired` free the statement: it may take a new proposal
(one per statement still applies). The tuner's deterministic rules do not
propose again, within 30 days of the operator's decision, the exact hint
text an operator rejected for the statement. No `proposed` row is written while hints cannot reach
application sessions (see `hint_readiness`): such a proposal could only wait
until it expired. `sage.query_hints.status` carries a `NOT VALID` CHECK on
these eight values; rows earlier releases wrote are not rescanned.

### When a query_tuning finding resolves

The tuning agent keeps every open finding it did not judge, so a
`query_tuning` finding (the tuner's or the agent's) resolves only on
deterministic evidence, checked once per analyzer cycle:

- for a finding that carries installable hint SQL, its statement's latest
  `sage.query_hints` row left `proposed` (it is `active`, `dormant`,
  `broken`, `retired`, `rolled_back`, `rejected` or `expired`): at once. An
  operator-hint or advisory finding recorded no row of its own, so an older
  row does not resolve it;
- its queryid has been absent from `pg_stat_statements` in this database
  for `tuner.hint_absent_cycles` consecutive cycles (default 3);
- it has stayed below the tuner's admission for `tuner.hint_absent_cycles`
  consecutive cycles: the cumulative counters no longer pass
  `tuner.min_query_calls` with a mean above 100 ms, temp blocks written or
  planning above `tuner.plan_time_ratio`, and the interval between two
  snapshots shows no calls, or a mean of at most 100 ms with no temp blocks
  written.

The counts are stored on the finding (`absent_cycles`, `quiet_cycles` in its
detail), so a restart keeps them; a statement seen again, or seen slow,
resets them. N cycles is N analyzer intervals: with the default 3 and a
5-minute interval a finding resolves 15 minutes after its statement left
`pg_stat_statements`. Nothing unknown resolves a finding: if
`pg_stat_statements` cannot be read, or the snapshots cannot measure the
statement's interval, the finding and its counts stay as they were. An open
hint finding no longer keeps the agent from asking about the rest of its
case (index and statistics answers); a second hint for the statement is
still refused.

Two revalidation outcomes keep the status:

- **Stats reset (`stats_reset`).** Each pass reads
  `pg_stat_statements_info.stats_reset` (PostgreSQL 14+) and stores it in
  `sage.query_hints.pgss_reset_at`. When the stored value is older, or the
  summed calls went down (a `pg_stat_statements_reset()`, a crash, or an
  entry eviction), the pass re-baselines `calls_at_last_check` and keeps the
  hint as it was. A reset never marks a hint `broken` or `dormant`.
- **Re-verification due (`age_reverify`).** Past
  `tuner.hint_retirement_days` (default 14; `0` disables the rule) the pass
  stamps `reverify_due` once. The hint stays installed; the counterfactual
  check that will act on it is planned separately.

Calls are summed over every `pg_stat_statements` row of the queryid in the
current database (one row per role and toplevel flag), so two roles running
the same statement cannot make the hint look idle. If `pg_stat_statements`
or `pg_stat_statements_info` cannot be read, the pass counts an error and
leaves the hint untouched (fail closed). Dormant hints count as covered:
the tuner does not propose a hint again for their query (see
[One hint per statement](#one-hint-per-statement)).

## One hint per statement

pg_sage proposes at most one hint per statement, and it checks this against
the database on every proposal, not only against its in-memory cooldown
(which now only spaces out repeat proposals for `trust.cascade_cooldown_cycles`
cycles). Before the tuner or the tuning agent proposes a hint for a queryid
it reads `sage.query_hints` and `hint_plan.hints` for it:

| What it finds | Deterministic tuner pass | Tuning agent hint |
|---------------|--------------------------|-------------------|
| Nothing | proposes the hint | proposes the hint |
| An open `proposed` row, nothing installed | refreshes that proposal in place (never a second row) | refused as a duplicate |
| pg_sage's own hint installed, or an `active` / `dormant` row | proposes nothing | refused as a duplicate |
| A `hint_plan.hints` row whose text no pg_sage record carries (an operator's own hint, for any `application_name`) | an informational finding, "operator hint present", with no SQL | refused (`ErrForeignHint`), rejected as a duplicate |

If that read fails, nothing is proposed (fail closed): the tuner pass reports
the error and the agent's hint is refused.

The apply SQL is a plain insert:

```sql
INSERT INTO hint_plan.hints (query_id, application_name, hints)
VALUES (4242, '', $sageqh$HashJoin(o l)$sageqh$)
```

If a row for the query appeared between the proposal and its approval,
pg_hint_plan 1.7's unique index on `(query_id, application_name)` rejects the
insert (SQLSTATE 23505). The action ends `failed`, no rollback is monitored,
and the existing row is untouched. pg_sage treats the hint table as ready
only when it has that unique index (valid, without a predicate, on
`query_id` and `application_name`); without it, as with pg_hint_plan below
1.7 (the PG14-16 builds), hint findings stay advisory and carry no SQL. In
fleet mode the check runs per database. Replacing an installed hint is not
something pg_sage does; change or remove an operator hint by hand.

## Rollback and removal touch only pg_sage's own hint

Releases before the one-hint check used an apply INSERT that was a silent
no-op when a `hint_plan.hints` row already existed for the query (for example
an operator's own hint), and actions they recorded may still be rolled back.
So every DELETE pg_sage builds for `hint_plan.hints` (an action's rollback,
and the `query_hint_retirement` removal of a `retired` or `broken` hint)
matches the query id, `application_name = ''` and the exact hint text pg_sage
recorded:

```sql
DELETE FROM hint_plan.hints
 WHERE query_id = 4242 AND application_name = ''
   AND hints = $sageqh$HashJoin(o l)$sageqh$
```

Rolling back an older action whose INSERT changed nothing deletes nothing, and the
operator's row stays. The removal finding is emitted only while the installed
row still holds the retired hint's text.

Actions recorded by earlier releases keep the SQL they were recorded with.
Their apply INSERT may carry the old `INSERT ... SELECT ... WHERE NOT EXISTS`
form; pg_sage still reads the hint text from it. Their rollback DELETE may
lack the hint-text predicate, so rolling such an action back now deletes only
a row whose text matches pg_sage's own. At rollback time (manual or automatic) the executor
rewrites it to the scoped form, using the text in `sage.query_hints` for that
query: the text the action's own INSERT installed, or, when the INSERT cannot
be read, the query's only recorded text. If no single recorded text matches,
the rollback is refused with reason `hint_text_unknown` (manual: the API
returns the error; automatic: the action ends `rollback_skipped`). Nothing is
deleted; remove the row from `hint_plan.hints` by hand if it is pg_sage's.
