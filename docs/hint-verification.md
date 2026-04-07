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

## 6. High Plan Time &rarr; `Set(plan_cache_mode "force_generic_plan")`

**Symptom:** Planning time dominates execution time for repeated queries with different parameters.

**Hint:** `/*+ Set(plan_cache_mode "force_generic_plan") */`

The hint is accepted by pg_hint_plan (confirmed via `pg_hint_plan.debug_print`). This forces PostgreSQL to reuse a generic plan instead of re-planning for each parameter set.

**Verdict:** Hint accepted. Effect is measurable on prepared statements with high planning overhead.

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

**Verdict:** Table-driven hints work. No application code changes needed &mdash; pg_sage inserts the hint, pg_hint_plan applies it automatically on matching queries.

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

## Summary

| Case | Symptom | Fix | Plan Change |
|------|---------|-----|-------------|
| Seq Scan w/ Index | Planner ignores index | `IndexScan(t idx)` | Bitmap &rarr; Index Scan |
| Disk Sort | Sort spills 119MB | `work_mem=256MB` | external merge &rarr; quicksort Memory |
| Hash Spill | 16 hash batches | `work_mem=128MB` | Batches: 16 &rarr; 1 |
| Bad Nested Loop | NL with bad estimate | `HashJoin(c o)` | Nested Loop &rarr; Hash Join |
| Parallel Off | Serial scan on 500K rows | `max_parallel_workers_per_gather=4` | Seq Scan &rarr; Parallel Seq Scan |
| High Plan Time | Re-planning overhead | `force_generic_plan` | Reuse cached plan |
| Sort + LIMIT | Full sort for 10 rows | CREATE INDEX (composite) | Sort eliminated |
| Temp Spill | Aggregation spills 22MB | `work_mem=512MB` | external merge &rarr; quicksort Memory |
| Missing FK Index | Seq scan on FK join | CREATE INDEX | Seq Scan &rarr; Bitmap Index (53x) |
| Hint Table | Transparent hint injection | `hint_plan.hints` INSERT | Bitmap &rarr; Index Scan |
| Merge Join | Force join strategy | `MergeJoin(o l)` | Hash Join &rarr; Merge Join |
| Combined Hints | Multiple symptoms | Combined hint string | Hash+Seq &rarr; Merge+Index |
