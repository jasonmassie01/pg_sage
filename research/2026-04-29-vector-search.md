# Postgres Vector Search Ecosystem and HNSW Tuning Opportunities

Research date: 2026-04-30  
Target project: pg_sage  
Scope: current Postgres vector search ecosystem, pgvector HNSW/IVFFlat tuning,
filtered search, recall/latency benchmarking, and safe autonomous DBA actions.

## Executive Summary

pgvector remains the baseline Postgres vector extension. As of the current upstream
README, it installs as `v0.8.2`, supports HNSW and IVFFlat, and adds a broad
surface area beyond float vectors: `halfvec`, `bit`, `sparsevec`, L2, inner
product, cosine, L1, Hamming, and Jaccard operators. HNSW is the default
recommendation for many production workloads because it has a better speed/recall
tradeoff than IVFFlat, but it uses more memory and takes longer to build.

The biggest safe tuning opportunity for pg_sage is query-time control:
`SET LOCAL hnsw.ef_search`, `hnsw.iterative_scan`, `hnsw.max_scan_tuples`,
`hnsw.scan_mem_multiplier`, `ivfflat.probes`, and `ivfflat.max_probes` can be
benchmarked and applied per transaction without rebuilding indexes. Build-time
options such as HNSW `m`, HNSW `ef_construction`, IVFFlat `lists`, and
StreamingDiskANN construction parameters should be treated as recommendations
that require evidence, maintenance windows, and usually `CREATE INDEX CONCURRENTLY`.

Filtering is the dominant hard case. In pgvector, filters are generally applied
as candidates are returned by the approximate index, which can underfill `LIMIT`
or lose recall when predicates are selective. pgvector 0.8.0 introduced
iterative scans to continue scanning until enough rows are found or a bound is
hit. Ecosystem alternatives are moving toward prefiltering or integrated filters:
pgvectorscale has label-aware StreamingDiskANN, VectorChord has prefiltering, and
recent research such as ACORN and FANNS papers is centered on predicate-aware
ANN search.

## Source Map

Primary and official sources:

- pgvector upstream README: https://github.com/pgvector/pgvector
- pgvector 0.8.2 release note and CVE notice:
  https://www.postgresql.org/about/news/pgvector-082-released-3245/
- pgvector 0.8.0 iterative scan release note:
  https://www.postgresql.org/about/news/pgvector-080-released-2952/
- pgvectorscale upstream README:
  https://github.com/timescale/pgvectorscale
- Timescale SQL interface for pgvector and pgvectorscale:
  https://docs.timescale.com/ai/latest/sql-interface-for-pgvector-and-timescale-vector/
- Supabase HNSW docs:
  https://supabase.com/docs/guides/ai/vector-indexes/hnsw-indexes
- Supabase IVFFlat docs:
  https://supabase.com/docs/guides/ai/vector-indexes/ivf-indexes
- Supabase production guide:
  https://supabase.com/docs/guides/ai/going-to-prod
- Neon pgvector optimization guide:
  https://neon.com/docs/ai/ai-vector-search-optimization
- Neon HNSW parallel build article:
  https://neon.com/blog/pgvector-30x-faster-index-build-for-your-vector-embeddings
- Lantern upstream repo:
  https://github.com/lanterndata/lantern
- Lantern HNSW autotune docs:
  https://lantern.dev/docs/lantern-cli/autotune
- Lantern HNSW memory analysis:
  https://lantern.dev/blog/calculator
- VectorChord upstream repo:
  https://github.com/tensorchord/VectorChord
- VectorChord prefiltering article:
  https://blog.vectorchord.ai/vectorchord-04-faster-postgresql-vector-search-with-advanced-io-and-prefiltering

Papers and high-signal community sources:

- HNSW original paper:
  https://arxiv.org/abs/1603.09320
- ACORN predicate-aware HNSW paper:
  https://arxiv.org/abs/2403.04871
- Filtered ANN systems paper with pgvector findings:
  https://arxiv.org/abs/2602.11443
- PostgreSQL-V decoupled vector search paper:
  https://www.cidrdb.org/cidr2026/papers/p2-liu.pdf
- QueryPlane pgvector HNSW tuning guide:
  https://queryplane.com/docs/blog/pgvector-hnsw-tuning-guide
- Production community discussion on filtered vector queries:
  https://www.reddit.com/r/PostgreSQL/comments/1ooeduv/

## pgvector Baseline

### Current version and safety posture

pgvector `v0.8.2` is current in the upstream install examples. The PostgreSQL
release note for 0.8.2 says it fixes a buffer overflow in parallel HNSW index
builds, CVE-2026-3172. pg_sage should inventory installed `vector` extension
versions and flag versions below 0.8.2 for upgrade planning, especially on
systems that build HNSW indexes in parallel.

Minimum inventory query:

```sql
SELECT extname, extversion
FROM pg_extension
WHERE extname = 'vector';
```

### HNSW parameters

Build-time options:

- `m`: max connections per layer. Default is 16. Higher values tend to improve
  recall, but increase index size, build time, insert cost, and memory pressure.
- `ef_construction`: dynamic candidate list used while building the graph.
  Default is 64. Higher values can improve index quality, but increase build
  time and insert speed cost. Supabase and Neon both repeat the practical rule
  that it should be at least `2 * m`.

Query-time options:

- `hnsw.ef_search`: dynamic candidate list for search. Default is 40. Higher
  values improve recall and usually increase latency. Use `SET LOCAL` inside a
  transaction for one query rather than session-level `SET` on pooled
  connections.
- `hnsw.iterative_scan`: available since pgvector 0.8.0. Values are `off`,
  `strict_order`, and `relaxed_order`.
- `hnsw.max_scan_tuples`: approximate cap for tuples visited by iterative scans.
  Default is 20,000.
- `hnsw.scan_mem_multiplier`: memory cap multiplier over `work_mem` for HNSW
  scan state. Default is 1. pgvector docs suggest increasing this if increasing
  `hnsw.max_scan_tuples` no longer improves recall.

Build/resource options:

- `maintenance_work_mem`: HNSW builds are much faster when the graph fits in
  memory. Do not set it so high that concurrent memory usage can exhaust the
  server.
- `max_parallel_maintenance_workers`: parallel index builds can speed HNSW
  creation. pgvector docs show setting it to 7 plus the leader, but this should
  be bounded by instance CPU, memory, and the installed pgvector version.
- `pg_stat_progress_create_index`: useful for progress telemetry.

Operational caveats:

- HNSW supports incremental inserts and can be built on an empty table.
- Indexes should still usually be created after initial bulk load for speed.
- In production, use `CREATE INDEX CONCURRENTLY` to avoid blocking writes.
- HNSW vacuum can be slow. pgvector suggests `REINDEX INDEX CONCURRENTLY`
  before vacuuming an HNSW-heavy table when appropriate.

### IVFFlat parameters

Build-time option:

- `lists`: number of inverted lists or clusters. pgvector suggests starting
  with `rows / 1000` for up to 1M rows and `sqrt(rows)` above 1M rows.

Query-time options:

- `ivfflat.probes`: lists to probe during search. Default is 1. Higher values
  improve recall and increase latency. A good starting point is `sqrt(lists)`.
  If `probes` equals `lists`, the search becomes exact and the planner may not
  use the index.
- `ivfflat.iterative_scan`: available since pgvector 0.8.0.
- `ivfflat.max_probes`: cap for iterative scans. If lower than
  `ivfflat.probes`, `ivfflat.probes` wins.

Operational caveats:

- IVFFlat should be created only after the table has enough data for meaningful
  clustering.
- If the data distribution changes significantly, IVFFlat may need rebuilding.
- IVFFlat has faster build time and lower memory use than HNSW, but generally a
  weaker speed/recall tradeoff. The filtered-ANN paper reports cases where
  partition-based indexes such as IVFFlat outperform graph indexes for some
  filtered workloads, so pg_sage should not treat HNSW as universally optimal.

### Type and index-design surface

pgvector now offers several space/latency levers:

- Use `halfvec` to reduce storage and cache footprint; HNSW can index up to
  4,000 dimensions with `halfvec` operator classes.
- Use binary quantization expression indexes and re-rank on original vectors for
  better recall.
- Use subvector expression indexes and re-rank on full vectors when models or
  Matryoshka embeddings make that valid.
- Use the operator class matching the query operator. Mismatches silently remove
  the expected ANN speedup.
- For normalized embeddings, inner product can be faster than cosine while
  preserving ranking equivalence in many workloads. Supabase recommends this as
  a production performance tip.

## Filtering and Iterative Scans

The key pgvector behavior: approximate indexes return candidates by vector
distance, then SQL predicates can filter those candidates. If a predicate matches
10 percent of rows and `hnsw.ef_search = 40`, only about four rows are expected
to survive before iterative scans or larger candidate lists are considered.

pg_sage should detect vector queries with both `WHERE` predicates and
`ORDER BY embedding <op> query LIMIT k`, then classify them:

- Low-selectivity or small filtered subset: a B-tree, BRIN, GIN, or exact scan
  on the filter path may beat ANN and preserve perfect recall.
- Few distinct filter values: recommend partial vector indexes, for example one
  HNSW index per important tenant, category, or status.
- Many distinct filter values with natural isolation: recommend table
  partitioning by tenant/category/time bucket, then a vector index per partition.
- General selective filters: benchmark `hnsw.iterative_scan = strict_order` and
  `relaxed_order`, then bound with `hnsw.max_scan_tuples` and
  `hnsw.scan_mem_multiplier`.
- Distance thresholds: use a materialized CTE and put the distance predicate
  outside the candidate-producing query, as pgvector recommends.

Strict vs relaxed ordering:

- `strict_order`: preserves exact distance ordering across iterations.
- `relaxed_order`: may improve recall/latency but can produce slightly
  out-of-order rows. Use a materialized CTE and final `ORDER BY distance` when
  strict output order is required.

Autonomous DBA safety rule: pg_sage can recommend or apply `SET LOCAL` query
knobs for the current transaction, but it should not globally alter planner
knobs such as `enable_seqscan` outside a benchmark harness.

## Benchmarking Recall and Latency

A useful benchmark harness for pg_sage:

1. Discover vector query shapes from `pg_stat_statements` and schema metadata.
2. Sample real query vectors and representative filters. Include common tenant,
   category, time-range, and permission predicates.
3. Build exact ground truth by running the same query with ANN index scans
   disabled inside a transaction:

```sql
BEGIN;
SET LOCAL enable_indexscan = off;
-- exact query here
COMMIT;
```

4. Run candidate ANN settings with `EXPLAIN (ANALYZE, BUFFERS)` and capture:
   recall@k, row underfill rate, p50/p95/p99 latency, rows removed by filter,
   buffers hit/read/dirtied, plan shape, and query text hash.
5. Warm caches before comparing steady-state latency. Supabase recommends
   `pg_prewarm` plus 10,000 to 50,000 warm-up queries before benchmarks.
6. Run cold-cache checks separately if the workload has bursty traffic or indexes
   do not fit in memory.
7. Prefer query-time grids first:
   `hnsw.ef_search` = `k`, 40, 80, 100, 200;
   `hnsw.iterative_scan` = off, strict, relaxed;
   `ivfflat.probes` = 1, `sqrt(lists)`, higher plateaus.
8. Only recommend rebuild grids after query-time tuning fails:
   HNSW `m` = 16, 24, 32; `ef_construction` = 64, 128;
   IVFFlat `lists` around pgvector starting rules.

QueryPlane's benchmark on random 128-dimensional vectors shows the expected
shape: raising `ef_search` can materially improve recall while adding latency,
and raising `m` increases index size and build time. Treat its numbers as
directional, not portable; real embeddings and filters must be measured locally.

## pgvectorscale and Timescale

pgvectorscale adds a `diskann` access method called StreamingDiskANN on top of
pgvector data types and syntax. It targets larger, more cost-sensitive indexes
than memory-heavy HNSW and adds Statistical Binary Quantization plus filtered
search support.

Build-time parameters:

- `storage_layout`: `memory_optimized` uses SBQ compression; `plain` stores
  vectors uncompressed. Default is `memory_optimized`.
- `num_neighbors`: max neighbors per node. Default is 50. Higher improves
  accuracy and slows graph traversal.
- `search_list_size`: construction-time greedy search list. Default is 100.
- `max_alpha`: graph quality parameter. Default is 1.2.
- `num_dimensions`: defaults to 0, meaning all dimensions. Can index fewer
  dimensions for Matryoshka-style embeddings.
- `num_bits_per_dimension`: SBQ bit width; defaults vary by dimensionality.

Query-time parameters:

- `diskann.query_search_list_size`: additional candidates considered during
  graph search. Default is 100.
- `diskann.query_rescore`: number of elements rescored. Default is 50.
  Timescale docs suggest tuning this first for accuracy.

Filtering:

- Label-based filtering is indexed by adding a `smallint[]` labels column to the
  `diskann` index and querying with the `&&` overlap operator.
- Arbitrary `WHERE` filtering is supported as streaming post-filtering. It is
  correct, but slower than label-aware filtering.
- DiskANN uses relaxed ordering; use a materialized CTE and final order if exact
  distance order is required.

pg_sage implication: if `vectorscale` is installed, expose a separate advisory
path. Do not translate HNSW advice directly to DiskANN; its safe query-time
knobs are `diskann.query_rescore` and `diskann.query_search_list_size`.

## Lantern

Lantern is an open-source Postgres extension with a `lantern_hnsw` index built
on `usearch`. Its useful ideas for pg_sage are autotuning and offloaded index
builds.

Important Lantern surfaces:

- Index parameters include `M`, `ef_construction`, `ef`, dimension, and metric.
- `lantern-cli autotune-index` can run recall/latency experiments and create the
  best index when requested.
- Lantern's memory analysis emphasizes that `M` controls neighbor-list footprint;
  larger `M` improves recall but increases memory and query time.
- Lantern supports external index creation/import, allowing expensive builds to
  happen away from the primary database workload.

pg_sage implication: copy the workflow pattern, not necessarily the extension.
For pgvector, pg_sage can implement a local autotune report: generate exact
ground truth, sweep query-time settings, then produce DDL recommendations for
build-time changes.

## Supabase and Neon Operational Guidance

Supabase:

- Recommends HNSW as the default vector index because of performance and
  robustness as data changes.
- Calls out the same HNSW tuning axes: `m`, `ef_construction`, and `ef_search`.
- Recommends benchmark warm-up using `pg_prewarm` and 10,000 to 50,000 warm-up
  queries before measuring production behavior.
- Advises benchmarking real queries and tuning `ef_search` or `probes` until
  both accuracy and QPS match requirements.
- Uses `halfvec` in automatic embedding examples to reduce storage, with HNSW
  indexes over `halfvec_cosine_ops`.

Neon:

- Documents exact search as the 100 percent recall baseline and recommends ANN
  indexes when data size or QPS make sequential scans too expensive.
- Recommends starting `m` in the 12 to 48 range for many HNSW workloads.
- Notes `ef_search` should be at least `k`, the query `LIMIT`.
- Emphasizes the HNSW memory/build-time tradeoff and shows scaling compute up
  for index builds, then scaling back down.
- Highlights `maintenance_work_mem` and `max_parallel_maintenance_workers` as
  important for HNSW build throughput.

pg_sage implication: model HNSW build advice as a capacity-planning task, not a
minor knob. It needs RAM headroom, CPU headroom, version checks, and rollback
planning.

## VectorChord and Research Direction

VectorChord is a pgvector-compatible Postgres extension focused on scalable,
disk-friendly search. It uses RaBitQ compression and autonomous reranking, offers
`vchordrq` indexes, and documents prefiltering, prefetching, recall measurement,
prewarming, and external builds. Its 0.4 article claims lower cold-query latency
from better I/O and faster filtered searches when prefiltering applies.

Research direction is aligned with this:

- ACORN extends HNSW with predicate subgraph traversal for predicate-aware
  hybrid search and reports large throughput improvements at fixed recall.
- The 2026 filtered-ANN systems paper finds that engine-level execution strategy
  can dominate raw index performance, and that pgvector's optimizer may choose
  ANN plans when exact scans would have comparable latency with perfect recall.
- PostgreSQL-V explores decoupling vector search from the Postgres executor
  while remaining compatible with pgvector APIs.

pg_sage implication: filtering advice should be first-class. The advisor should
not only say "raise ef_search"; it should compare exact, post-filtered ANN,
iterative ANN, partial indexes, partitioning, and extension-specific filtered
indexes.

## Safe Autonomous DBA Actions for pg_sage

Safe to apply automatically when scoped to one transaction and backed by policy:

- Use `SET LOCAL hnsw.ef_search = n` for known query classes with measured
  recall/latency wins.
- Use `SET LOCAL hnsw.iterative_scan = strict_order` for filtered HNSW queries
  that underfill `LIMIT` and require exact ordering.
- Use `SET LOCAL hnsw.iterative_scan = relaxed_order` only when the application
  can tolerate relaxed candidate order or pg_sage rewrites with a final
  materialized CTE sort.
- Use `SET LOCAL hnsw.max_scan_tuples` and `hnsw.scan_mem_multiplier` within
  bounded presets when filtered recall is poor and memory headroom is known.
- Use `SET LOCAL ivfflat.probes = n` and `ivfflat.max_probes = n` for IVFFlat
  query classes.
- Use `SET LOCAL diskann.query_rescore` or
  `diskann.query_search_list_size` when `vectorscale` is installed and the
  indexed query uses `diskann`.
- Run recall probes using exact search in a transaction with local planner
  changes, then restore automatically at commit.
- Emit warnings for pgvector versions below 0.8.2 and for extension versions
  lacking iterative scan support.

Safe to recommend, but not apply automatically without explicit policy:

- Rebuild HNSW with larger `m` or `ef_construction`.
- Rebuild IVFFlat with different `lists`.
- Add partial HNSW indexes for tenants/categories/statuses.
- Partition large vector tables by tenant, category, or time.
- Create `halfvec`, binary quantization, or subvector expression indexes.
- Add pgvectorscale label columns or migrate selected workloads to DiskANN.
- Reindex HNSW indexes before vacuum.
- Increase server-wide `maintenance_work_mem`, parallel worker settings, or
  shared memory parameters.

Unsafe defaults:

- Do not set vector GUCs globally on pooled app connections without a reset
  strategy.
- Do not force `enable_seqscan = off` or `enable_indexscan = off` in production
  query paths.
- Do not rebuild indexes based only on generic defaults. Require measured
  recall, latency, size, and build-cost evidence.
- Do not assume HNSW is best for every filtered workload.
- Do not build parallel HNSW indexes on old pgvector versions affected by the
  0.8.2 security fix.

## Candidate pg_sage Features

1. Vector extension inventory:
   detect `vector`, `vectorscale`, `lantern`, `vchord`, extension versions,
   operator classes, index sizes, and current GUC settings.

2. Vector query classifier:
   identify `ORDER BY embedding <op> const LIMIT k`, operator/opclass mismatch,
   missing `LIMIT`, vector dimensions, filters, joins, and tenant predicates.

3. Recall harness:
   compare ANN results against exact search for sampled queries and store
   recall@k, underfill rate, and latency percentiles.

4. Filter advisor:
   recommend iterative scans, higher candidate budgets, exact prefilter paths,
   partial indexes, partitioning, or extension-specific label/prefilter indexes.

5. Query-time tuning policy:
   apply `SET LOCAL` knobs only for query fingerprints with a saved benchmark
   and guardrails for max latency, max memory, and minimum recall.

6. Build-time DDL planner:
   generate `CREATE INDEX CONCURRENTLY` alternatives with estimated build memory,
   disk size, lock impact, fallback plan, and validation query suite.

7. Cache and memory advisor:
   compare vector index sizes with RAM, `shared_buffers`, observed buffer reads,
   and cold-query behavior; recommend `pg_prewarm`, halfvec, quantization, or
   partitioning when indexes do not fit.

8. HNSW lifecycle advisor:
   watch delete/update churn, index bloat signals, slow vacuum, and recall drift;
   recommend `REINDEX INDEX CONCURRENTLY` before maintenance where appropriate.

## Practical Tuning Playbook

Default first pass:

1. Confirm pgvector is at least 0.8.2.
2. Confirm the query uses the distance operator matching the index opclass.
3. Confirm `ORDER BY distance_operator ASC LIMIT k`.
4. Capture exact recall baseline and current ANN plan.
5. Sweep `hnsw.ef_search` with `SET LOCAL` before touching the index.
6. For filtered queries, sweep iterative scan modes and bounds.
7. If query-time tuning cannot hit recall/latency targets, recommend HNSW
   rebuild options or alternate physical design.

Filtered HNSW first pass:

```sql
BEGIN;
SET LOCAL hnsw.ef_search = 100;
SET LOCAL hnsw.iterative_scan = strict_order;
SET LOCAL hnsw.max_scan_tuples = 50000;
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, embedding <=> $1 AS distance
FROM items
WHERE tenant_id = $2
ORDER BY embedding <=> $1
LIMIT 10;
COMMIT;
```

Relaxed ordering with final sort:

```sql
BEGIN;
SET LOCAL hnsw.iterative_scan = relaxed_order;
WITH relaxed_results AS MATERIALIZED (
  SELECT id, embedding <=> $1 AS distance
  FROM items
  WHERE tenant_id = $2
  ORDER BY embedding <=> $1
  LIMIT 50
)
SELECT *
FROM relaxed_results
ORDER BY distance
LIMIT 10;
COMMIT;
```

IVFFlat first pass:

```sql
BEGIN;
SET LOCAL ivfflat.probes = 10;
SET LOCAL ivfflat.iterative_scan = relaxed_order;
SET LOCAL ivfflat.max_probes = 100;
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, embedding <=> $1 AS distance
FROM items
ORDER BY embedding <=> $1
LIMIT 10;
COMMIT;
```

## Bottom Line for pg_sage

The highest-value pg_sage feature is not an automatic "best HNSW parameters"
rule. It is a measured advisor that understands vector query fingerprints,
filters, exact recall baselines, ANN recall/latency tradeoffs, and extension
capabilities. Query-time `SET LOCAL` tuning can be made safe and reversible.
Build-time graph shape, clustering, quantization, and partitioning should remain
evidence-backed recommendations until pg_sage has explicit operator approval or
a mature autonomous change policy.
