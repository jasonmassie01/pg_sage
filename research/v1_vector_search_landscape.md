# pg_sage v1: Vector Search Landscape and Opportunity Map

Research date: 2026-04-29
Scope: Postgres vector search ecosystem, operator pain points, AI-native stack, and concrete opportunity areas for an autonomous DBA sidecar to add vector-aware features.

This file is a complement to `2026-04-29-vector-search.md` (deep technical reference on pgvector knobs, iterative scans, and pgvectorscale parameters). It does not re-derive that material; it focuses on (a) ecosystem positioning, (b) what operators actually break in production, (c) the AI-native stack around the database, (d) prioritized pg_sage features, and (e) competitive gap analysis.

---

## 1. Ecosystem State (April 2026)

The Postgres vector search ecosystem has consolidated around four serious projects, plus one full-text/hybrid contender. The market is no longer "is pgvector enough" — it is "which extension stack" and "how do you keep it healthy."

### 1.1 pgvector — the de facto baseline

pgvector is the default vector extension on every major managed Postgres in 2026: Supabase, Neon, AWS RDS/Aurora, Azure, Google AlloyDB, Crunchy Bridge, Heroku, Tembo. Current upstream is `0.8.2` ([CHANGELOG](https://github.com/pgvector/pgvector/blob/master/CHANGELOG.md)) which patched a parallel-build buffer overflow (CVE-2026-3172). 0.8.0 introduced the now-essential `iterative_scan` mode (`strict_order` and `relaxed_order`) for filtered ANN ([release note](https://www.postgresql.org/about/news/pgvector-080-released-2952/)). 0.7 brought `halfvec`, `bit`, and `sparsevec` types and changed the storage and quantization story ([Aurora 0.7 announcement](https://aws.amazon.com/about-aws/whats-new/2024/08/pgvector-0-7-0-aurora-postgresql/)).

The most thoughtful 2025-2026 critique is Alex Jacobs's ["The Case Against pgvector"](https://alex-jacobs.com/posts/the-case-against-pgvector/) (and the [HN discussion](https://news.ycombinator.com/item?id=45798479)). The thesis is not "pgvector is broken." The thesis is "pgvector demos beautifully and operationalizes terribly." Concrete failure modes called out: index management is memory-heavy and disruptive, the planner cost model was never built for ANN, recall drops as IVFFlat data drifts post-build, multi-tenant tuning is "nearly impossible," and the gap between a 10K-row demo and a 50M-row production deployment is enormous. Simon Willison [endorsed](https://simonwillison.net/2025/Nov/3/the-case-against-pgvector/) the post. Counter-evidence: pgvector 0.8 closes most of the planner-and-filter gap when you actually use the new knobs.

### 1.2 pgvectorscale — Tiger Data (formerly Timescale)

[pgvectorscale](https://github.com/timescale/pgvectorscale) is a Rust pgrx extension that *augments* pgvector with a `diskann` access method (StreamingDiskANN, based on Microsoft's DiskANN paper) and Statistical Binary Quantization (SBQ). It is the answer to "my HNSW index doesn't fit in RAM anymore." Tiger's own benchmarks show 28x lower p95 latency and 16x higher throughput vs Pinecone s1 at 99% recall on 50M Cohere 768-dim vectors at 75% lower cost ([Tiger blog](https://www.tigerdata.com/blog/pgvector-is-now-as-fast-as-pinecone-at-75-less-cost)). It also added label-aware filtered search based on Microsoft's Filtered DiskANN paper, which sidesteps pgvector's "filter after candidates" problem at the storage-layer level.

Production status: GA, used in Tiger's managed offering, available as OSS PGRX extension. Real concern: it depends on pgvector and ships its own binary. Many managed Postgres providers do not yet enable it (RDS does not; AlloyDB does not; Supabase does not by default, requires `pgvectorscale` in the allowlist).

### 1.3 VectorChord (vchord) — successor to pgvecto.rs

[VectorChord](https://github.com/tensorchord/VectorChord) is the Tensorchord team's rewrite of `pgvecto.rs`. The original pgvecto.rs is now [explicitly deprecated](https://docs.vectorchord.ai/admin/migration.html); Immich migrated off it in v1.133.0 ([discussion](https://github.com/immich-app/immich/discussions/16335)). VectorChord depends on pgvector data types so migration is column-level, not full-database. It claims 2x QPS over pgvector at equivalent recall, 16x faster index builds via offboarded KMeans, 1565 inserts/sec vs pgvector's 246, and 26x cheaper per-vector storage than pgvector through aggressive RaBitQ-style quantization ([VectorChord 1.0 launch](https://blog.vectorchord.ai/vectorchord-10-developer-first-vector-search-on-postgres-100x-faster-indexing-than-pgvector)).

VectorChord's real selling point for pg_sage's audience: it ships [prefiltering](https://blog.vectorchord.ai/vectorchord-04-faster-postgresql-vector-search-with-advanced-io-and-prefiltering) at the index level (not just iterative scan after the fact), which is the right answer to selective metadata filters.

### 1.4 Lantern — pgvector-compatible HNSW with autotune

[Lantern](https://github.com/lanterndata/lantern) is interesting for pg_sage specifically because it already ships a tuner: [`lantern-cli autotune-index`](https://lantern.dev/docs/lantern-cli/autotune) sweeps `m`, `ef`, `ef_construction` against a recall target and reports latency at each combination. Lantern also offers [external indexing](https://lantern.dev/blog/pgvector-external-indexing) — offload index build to a separate machine and stream the file back, avoiding the production-OOM pattern. It supports pgvector's data type, so apps can drop in. Lantern is real but lower momentum than pgvector / pgvectorscale / VectorChord; treat it primarily as a reference design for what an autotuner UI looks like.

### 1.5 ParadeDB pg_search — hybrid search via BM25

[pg_search](https://github.com/paradedb/paradedb) is the BM25-in-Postgres extension, built on Tantivy (Rust Lucene). ParadeDB pitches it for hybrid: BM25 plus pgvector with Reciprocal Rank Fusion ([ParadeDB hybrid manual](https://www.paradedb.com/blog/hybrid-search-in-postgresql-the-missing-manual)). Hybrid retrieval is now the default RAG architecture pattern in 2026 because pure semantic gets ~62% precision while BM25+RRF+vector gets ~84%. Note: Neon [removed pg_search availability](https://neon.com/docs/extensions/pg_search) for new projects on 2026-03-19, which is a significant shift in managed-Postgres support. Tiger countered with [pg_textsearch](https://www.tigerdata.com/blog/introducing-pg_textsearch-true-bm25-ranking-hybrid-retrieval-postgres) as a BM25 alternative, narrowing ParadeDB's moat on hosted Postgres.

### 1.6 pg_embedding — deprecated

[pg_embedding](https://github.com/neondatabase/pg_embedding) (Neon's HNSW extension) was the early competitor to pgvector. Neon [paused development](https://neon.com/docs/extensions/pgvector) in late 2023 once pgvector got HNSW. Existing users still get support; new projects should not pick it. Mention only as a cautionary tale: "we forked pgvector for performance" rarely wins long-term.

### 1.7 Quick reference table

| Extension | Status 2026 | Best at | Sharp edge |
|---|---|---|---|
| pgvector 0.8.2 | de facto standard | broad ecosystem, halfvec, iterative scan | recall@scale, vacuum on HNSW, planner |
| pgvectorscale | GA, Tiger | DiskANN + SBQ for >10M vectors | not on RDS/AlloyDB by default |
| VectorChord | GA, Tensorchord | prefiltering, build speed, quantization | newest API surface, fewer prod refs |
| Lantern | active, lower mindshare | autotune CLI, external builds | smaller community |
| pg_search (ParadeDB) | active | BM25 / hybrid | dropped from new Neon projects |
| pgvecto.rs | DEPRECATED | — | migrate to VectorChord |
| pg_embedding | DEPRECATED | — | migrate to pgvector |

---

## 2. The Pain Operators Actually Report

This section sticks to specifics with citations. These are the things on-call engineers reported in the last 12 months.

### 2.1 HNSW build OOMs and multi-hour stalls

The single most-reported failure mode. [pgvector issue #822](https://github.com/pgvector/pgvector/issues/822): HNSW build on ~40M rows stuck at 29.2% for 8+ hours, total build 19h, the moment maintenance_work_mem is exceeded the build flips to a [disk-based fallback that runs 10–50x slower](https://tech-champion.com/database/the-vector-hangover-hnsw-index-memory-bloat-in-production-rag/). Real production reports: 200M 75-dim vectors took 13h and crashed the host at 100GB/124GB RAM ([anup.io](https://www.anup.io/pgvector-doesnt-scale/)). Mitigations: (1) raise maintenance_work_mem, (2) use parallel build (0.6+ added ~30x speedup, [Neon](https://neon.com/blog/pgvector-30x-faster-index-build-for-your-vector-embeddings)), (3) use halfvec to halve memory, (4) external index build (Lantern), (5) move to pgvectorscale or VectorChord for memory-efficient builds.

### 2.2 Vacuum on HNSW is brutal

pgvector docs and Crunchy guidance say to `REINDEX INDEX CONCURRENTLY` *before* `VACUUM` on HNSW-heavy tables ([deepwiki](https://deepwiki.com/pgvector/pgvector/6.2-updating-vectors-and-vacuum)). The HNSW graph fragments under updates and deletes; autovacuum cannot keep up under heavy write workload. A [Layers engineering post](https://medium.com/engineering-layers/signal-driven-health-monitoring-for-hnsw-indices-w-pgvector-ba35d9a6e575) proposes a three-metric health score for HNSW indices (dead tuple ratio, cache hit ratio, bytes-per-vector). Slow inserts when an HNSW index is present are commonly ~5x ([pgvector #877](https://github.com/pgvector/pgvector/issues/877)).

### 2.3 Filtered ANN: underfill and recall collapse

The classic shape: `WHERE tenant_id = $1 ORDER BY embedding <-> $2 LIMIT 10`. Pre-0.8 pgvector did post-filter: HNSW returns `ef_search` candidates, predicate filters them, you may end up with 2 rows when you asked for 10. With selective filters you may end up with 0. Iterative scan in 0.8.0 (`hnsw.iterative_scan = strict_order|relaxed_order`, bounded by `hnsw.max_scan_tuples` and `hnsw.scan_mem_multiplier`) [solves the underfill problem](https://aws.amazon.com/blogs/database/supercharging-vector-search-performance-and-relevance-with-pgvector-0-8-0-on-amazon-aurora-postgresql/). It does not solve the *plan-shape* problem: with very selective filters, brute-force on a B-tree+seq distance can dominate ANN, and the planner's vector cost estimate is still naive ([pgvector #862](https://github.com/pgvector/pgvector/issues/862) — planner assumes strict ordering even under relaxed_order).

### 2.4 Recall drift

IVFFlat clustering is built once at index time. As data distribution shifts (new content topics, new tenants, seasonal embeddings), centroids stale, recall drops with no warning ([dbi-services](https://www.dbi-services.com/blog/pgvector-a-guide-for-dba-part-2-indexes-update-march-2026/)). HNSW is more forgiving but still degrades after ~30% row turnover. The signal: recall drops without query-pattern change, or insert volume since last build > 30%. There is no built-in monitoring for this; operators discover it through user complaints.

### 2.5 work_mem and JIT interaction

Two recurring issues. First, JIT-ON for vector workloads frequently *hurts* because cold compile time exceeds query latency on small ANN scans. Several Aurora and Crunchy guides recommend `jit = off` for ANN-heavy schemas. Second, `hnsw.scan_mem_multiplier` is gated on `work_mem`, so iterative scans on selective filters need work_mem raised — but Postgres allocates work_mem per node, so a permissive global work_mem on a high-concurrency app can OOM the host independently of vector workload. The right pattern is `SET LOCAL` per transaction, but most apps and ORMs don't do this.

### 2.6 Index size dominates table size

HNSW typically lands at 1.5–3x raw vector data; for 1536-dim float vectors that means index > heap. A 10M-row 1536-dim table at 60GB heap can carry an 80–120GB HNSW ([Lantern storage analysis](https://lantern.dev/blog/pgvector-storage)). pgvectorscale's SBQ and VectorChord's RaBitQ change this dramatically (sub-1x in some cases) but only if you migrate. pg_sage's existing cost dashboard has no vector-aware bytes-per-vector accounting, and "index 4x heap" is invisible to most operators until billing alerts fire.

### 2.7 Dimension change is brutal

Common scenario: team upgrades from `text-embedding-ada-002` (1536) to `text-embedding-3-large` (3072) or to a Matryoshka model with truncation. `ALTER TABLE ... ALTER COLUMN embedding TYPE vector(3072)` requires dropping all dependent indexes, takes an `ACCESS EXCLUSIVE` lock, and rebuilds the index — full downtime on a hot RAG path. The two-column migration (add new col, backfill, drop old col, rename) is the safer pattern but doubles storage during cutover ([TianPan post](https://tianpan.co/blog/2026-04-09-embedding-models-production-versioning-index-drift)). Worse: silent model upgrades from API providers (OpenAI quietly bumping the underlying model behind a stable endpoint name) cause [index drift](https://www.dbi-services.com/blog/rag-series-embedding-versioning-with-pgvector-why-event-driven-architecture-is-a-precondition-to-ai-data-workflows/) — vectors from v1 and v2 cohabit the same index and recall silently degrades. There is no `model_version` column standard.

### 2.8 Multi-tenant pathologies

["Why did Tenant A's growth slow down Tenant B?"](https://www.thenile.dev/blog/multi-tenant-rag) is a real political problem when one global HNSW serves many tenants. Per-tenant partial indexes (`CREATE INDEX ... WHERE tenant_id = $X`) work for VIP tenants but explode the catalog for thousands of small tenants. The 2026 consensus is partition-by-tenant for fat tenants, B-tree-on-tenant-id + iterative_scan for the long tail. Few teams instrument this; most discover it after p99 spikes.

---

## 3. The AI-Native Postgres Stack in 2026

A modern app using Postgres for vectors typically has three vector workloads layered on the same database:

1. **RAG corpus** — long-lived, mostly read-only, large (millions to hundreds of millions of vectors), high-dimensional, hybrid search expected. Build cost matters more than insert latency.
2. **Agent memory store** — append-heavy, time-decayed, mid-size (tens of thousands per agent, millions across the fleet), often filtered by user_id and recency. Insert latency matters; recall on recent items matters most.
3. **Semantic cache** — high write rate, high read rate, small per-tenant size, TTL-based eviction, cosine similarity + threshold. Latency dominates; recall is binary (hit/miss). Production hit rates are 20–45%, not the 95% vendors claim ([TianPan](https://tianpan.co/blog/2026-04-10-semantic-caching-llm-production)).

The newest layer in 2026: **temporal-aware agent memory**. [MemoriesDB](https://arxiv.org/abs/2511.06179), [Mem0](https://mem0.ai/blog/state-of-ai-agent-memory-2026), and [Constructive's agentic-db](https://www.morningstar.com/news/pr-newswire/20260428sf45149/constructive-open-sources-agentic-db-the-postgres-memory-layer-for-ai-agents) all sit on Postgres + pgvector and add: time-decay scoring (semantic_similarity * exp(-lambda * age)), TTL per memory category, a graph layer for entity relationships, and BM25 alongside vectors. None of these systems are turnkey; each one re-implements the same ops problems pg_sage already solves for the OLTP case.

What an autonomous DBA needs to monitor that doesn't exist for OLTP:

- **Recall over time** as a first-class metric, not just latency. A query at 5ms that returns the wrong answer is worse than one at 50ms that's right.
- **Index-to-heap ratio** — vector indexes are the only common case where indexes routinely exceed the table size.
- **Build pressure** — current `pg_stat_progress_create_index` plus historical build-completion-time-vs-row-count to predict next rebuild's blast radius.
- **Embedding model versioning** — rows tagged with model and dimension; alerts on mixed-version writes.
- **Filter selectivity drift** — when a partial index's WHERE clause's row population shrinks toward zero, the index becomes unmaintained ballast.
- **Cache locality of HNSW** — `pg_buffercache` overlap with vector index relfilenode. When eviction climbs, ANN goes from 2ms to 200ms with no other signal.

These are all in pg_sage's wheelhouse: collector + rules engine + LLM advisor + executor.

---

## 4. pg_sage Opportunity Areas (Prioritized)

Each item below: complexity (Tier-1 deterministic / Tier-2 LLM / Tier-3 executor), competitive differentiation, and dependencies. Numbered for cross-reference, ordered by opportunity-cost ratio.

### 4.1 Vector index inventory + version & sharp-edge audit (T1, low complexity)

What: scan `pg_extension` for `vector`, `vchord`, `vectorscale`, `lantern`, `pg_search`; enumerate all vector indexes with their access method, opclass, parameters (m, ef_construction, lists, num_neighbors), build duration, and current size; flag known-bad versions (pgvector < 0.8.2 due to CVE-2026-3172, pgvecto.rs in any version because deprecated, pg_embedding because deprecated).

LLM role: none. Pure rules.

Complexity: 1 week. This is the foundation for everything else and instantly differentiates from pg_sage's current OLTP-only inventory.

Comp: nobody does cross-extension vector inventory. Tiger's tools assume Tiger; Lantern's CLI assumes Lantern.

### 4.2 Build-cost & build-risk forecaster (T1+T2, medium)

What: when a vector index is requested or scheduled (or when row count crosses a threshold), forecast (a) build duration and (b) peak maintenance_work_mem requirement. Compare to free RAM and `maintenance_work_mem` config. Refuse Tier-3 auto-execution if predicted memory exceeds (host_ram - shared_buffers - 20% safety). Recommend halfvec, parallel workers, or external build (Lantern model) when over budget.

LLM role: explain the forecast and recommend mitigations in plain English. The forecast itself is a regression on pgvector's published rules + observed history.

Complexity: 2-3 weeks. This is the single feature that prevents the most common production-down event in vector workloads.

Comp: no Postgres tool does this. Pinecone & Milvus AutoIndex sidestep it because their build is opaque.

### 4.3 Recall measurement framework (T2, medium-high)

What: a "recall sampler" that, on a configurable schedule, picks N representative ANN queries from `pg_stat_statements`, runs each with current ANN settings, then runs the same query with `enable_indexscan = off` (or explicit `LIMIT k OFFSET 0` exact mode) inside a `BEGIN; SET LOCAL ... ; ROLLBACK;` envelope, computes recall@k and latency, and stores time-series in `sage.vector_recall_history`. Trigger Tier-2 advisor when recall drops > 5% week-over-week.

LLM role: synthesize "your recall dropped on the 'support_tickets' index because you've inserted 1.2M rows since last rebuild and your IVFFlat centroids are stale." Recommend `REINDEX CONCURRENTLY`, larger `lists`, or migration to HNSW.

Complexity: 3-4 weeks. The ground-truth pass is expensive; needs sampling, throttling, and a budget per database.

Comp: AlloyDB has [a manual `pg_similarity_search_recall`](https://cloud.google.com/alloydb/docs/ai/measure-vector-query-recall) but no scheduling, no time-series, no advisor. Lantern autotune-CLI does it once at index-design time, not continuously. **This is pg_sage's strongest moat.** Nothing else closes the loop on recall.

### 4.4 HNSW/IVFFlat query-time autotuner (T1+T2, medium)

What: per-query-shape (hash of normalized text), search a small grid of `hnsw.ef_search` (default 40, try 40/80/160/320), `hnsw.iterative_scan` (off/strict/relaxed), `ivfflat.probes` (1, sqrt(lists), 4*sqrt(lists)). Optimize for recall@k >= target and minimize p95 latency. Apply via `SET LOCAL` in transaction or surface as a query hint / `SET` recommendation. **Build-time** params (m, ef_construction, lists) are explicitly Tier-3 advisory only with manual approval; they require `REINDEX CONCURRENTLY`.

LLM role: explain when to choose strict vs relaxed ordering for a given query (e.g., "this query has `LIMIT 10` and is shown in a search UI ranked list; use `strict_order` to preserve display ordering") and when to recommend a rebuild instead of more ef_search.

Complexity: 4-6 weeks. The hard part is benchmark harness reliability, not the search.

Comp: Lantern's `autotune-index` is the closest parallel but only build-time, only Lantern, and one-shot. Qdrant's [collection optimizer](https://qdrant.tech/documentation/concepts/optimizer/) does similar but inside Qdrant. Pinecone hides it. Nobody has continuous query-time autotuning on Postgres.

### 4.5 Hybrid plan-shape diagnostic (T2, medium)

What: for queries with both `WHERE` and `ORDER BY embedding <op> $vec LIMIT k`, run `EXPLAIN (ANALYZE, BUFFERS)` periodically, classify the chosen shape (vector-index-then-filter, B-tree-then-seq-distance, parallel-bitmap, brute force), measure rows-removed-by-filter, and detect pathological combos. Common pathology: planner picks vector index but the `WHERE` is so selective that 99% of candidates are filtered out — fixable by either iterative_scan with bounded max_scan_tuples or by a partial vector index.

LLM role: read the plan, explain the pathology, propose: (a) `SET LOCAL hnsw.iterative_scan = relaxed_order`, (b) a partial index, (c) a per-tenant partition with vector index per partition. Treat (b) and (c) as Tier-3 advisory with HypoPG-style virtualization for confidence.

Complexity: 4 weeks. Reuses pg_sage's existing optimizer plumbing.

Comp: none on Postgres. ParadeDB's [hybrid manual](https://www.paradedb.com/blog/hybrid-search-in-postgresql-the-missing-manual) is the prose version of this analysis; nobody automates it.

### 4.6 Embedding drift / model-version monitor (T1+T2, medium)

What: detect tables containing vector columns and check for a sibling `model_version` or `embedding_model` column. If absent, flag as a Tier-1 finding ("write embeddings without model tag — silent drift risk"). If present, audit `SELECT model_version, count(*) FROM ...` for mixed versions in the same index and alert. Optionally probe vector norm and direction stats over time to detect an upstream model swap on a stable API endpoint.

LLM role: when mixed versions detected, generate a remediation plan (blue-green re-embedding, partial-index split by model_version).

Complexity: 2-3 weeks. The norm-drift heuristic is novel; even mature tools miss this.

Comp: nobody does this. Mem0 mentions versioning but doesn't enforce it.

### 4.7 Vacuum & reindex scheduler for vector indexes (T1+T3, medium)

What: extend pg_sage's existing vacuum advisor with HNSW-specific rules: HNSW dead-tuple ratio, cache hit ratio on the index relfilenode, bytes-per-vector trend. When degraded, recommend `REINDEX INDEX CONCURRENTLY` *before* `VACUUM` (per pgvector docs). Schedule during low-traffic windows. Tier-3 auto-execute under existing trust ramp once user passes ADVISORY phase.

LLM role: minimal. Existing advisor format extended.

Complexity: 2 weeks. Extends existing infrastructure cleanly.

Comp: Tiger's tooling assumes their cloud. RDS Performance Insights doesn't surface HNSW health. Layers's blog post is a manual playbook; pg_sage automates it.

### 4.8 Cost & storage panel — vector-aware (T1, low)

What: in the existing cost dashboard, surface index_size / heap_size ratio per table, flag where vector index > heap, recommend `halfvec` migration or pgvectorscale SBQ where applicable. Quantify savings: a 1536-dim float index converted to halfvec saves ~50% storage with <1% recall loss on normalized embeddings ([Neon](https://neon.com/blog/dont-use-vector-use-halvec-instead-and-save-50-of-your-storage-cost)).

LLM role: build the migration plan.

Complexity: 1 week.

Comp: none.

### 4.9 Migration safety: ALTER vector dimension as a Tier-3 *guarded* operation (T2+T3, high)

What: detect when a user wants to change a vector column's dimension (or change opclass, or rebuild with different `m`/`ef_construction`). Refuse to auto-execute. Generate the safe two-column migration plan: add new column, backfill in batches, swap, drop old. Estimate downtime, peak storage, and rollback path.

LLM role: write the migration. Tier-3 always requires manual approval for HIGH-risk operations.

Complexity: 6+ weeks (high because backfill orchestration touches the executor's most sensitive guardrails).

Comp: none.

### 4.10 Semantic cache & agent-memory health profiles (T1, low-medium)

What: detect schemas matching common shapes (cache table with `query_hash`, `embedding`, `response`, `expires_at`; agent memory table with `agent_id`, `embedding`, `created_at`, `decay_factor`). Apply specialized rule profiles: cache hit-rate audit, TTL/expiry monitoring, decay-function sanity checks, per-agent memory growth.

LLM role: classify the schema; generate the profile.

Complexity: 2 weeks for detection + 2 weeks per profile.

Comp: nobody. This is squarely "an autonomous DBA for AI workloads" and aligns with pg_sage's core thesis.

---

## 5. Competitive Gap: What Pinecone, Qdrant, Weaviate, Milvus, Vespa Autotune

Reading their docs and benchmarks, here's the autotune landscape:

| System | Auto-* features | What they hide / abstract |
|---|---|---|
| Pinecone | auto-scaling, auto-indexing optimization in v2026.03; `s1`/`p1`/`p2` storage tiers ([Pinecone docs](https://www.pinecone.io/blog/pinecone-vs-pgvector/)) | index type, parameters, replica count |
| Milvus | `AUTO_INDEX` opt-in ([Milvus docs](https://milvus.io/)) — picks HNSW/IVF/DiskANN by collection size and hardware | parameter tuning, segment merge policy |
| Qdrant | per-collection optimizer with `default_segment_number=0` for CPU-based default ([Qdrant optimizer](https://qdrant.tech/documentation/concepts/optimizer/)) | segment management, indexing thresholds |
| Weaviate | dynamic indexing thresholds; HNSW autotune is roadmap not GA | nothing as strong as Qdrant or Pinecone |
| Vespa | rank-profile tuning; tensor optimization at compile time | shard balancing |

The honest takeaway: **the dedicated vector DBs autotune *server-side* parameters operators don't see** (segment merging, replica balancing, memory tiering). They do **not** generally autotune the *user-facing* parameters (ef, probes, m, ef_construction). They expose those and let users choose. The reason is operational: changing those changes recall, and changing recall silently is a contract violation.

This is pg_sage's wedge. Postgres exposes all the knobs. An autonomous DBA can:

1. Measure recall continuously (nobody else does this for Postgres).
2. Tune query-time knobs *with measured recall as a constraint* (nobody does this anywhere with this contract).
3. Recommend build-time knobs only with a HypoPG-style confidence test plus operator approval (Lantern is the only comparable, and only one-shot).

**The smallest useful autotuner that beats hand-tuning** is the per-query-shape `hnsw.ef_search` and `hnsw.iterative_scan` chooser (item 4.4 above), gated on a recall measurement loop (item 4.3). That alone replaces a senior engineer afternoon spent in Aurora Workbench. It is shipping-grade in 2-3 sprints. Everything else (build-time tuning, cross-extension migration, cost dashboards) is incremental from there.

---

## Top 5 Concrete pg_sage Features (Prioritized)

1. **Recall Measurement Framework (4.3)** — the moat. Continuous recall@k sampling with time-series, per-index. Nobody does this on Postgres. Foundation for items 2 and 4.
2. **Query-time Autotuner: ef_search × iterative_scan (4.4 query-time portion)** — highest ratio of value to engineering. Per-query-shape tuning via `SET LOCAL`, gated on (1). Beats hand-tuning. 2-3 sprints.
3. **Build-cost & OOM Forecaster (4.2)** — prevents the single most common vector outage. Must-have before turning on Tier-3 auto-exec for vector indexes. Pure regression on row count, dimensions, m, ef_construction; LLM only for the explanation.
4. **Hybrid Plan-Shape Diagnostic (4.5)** — leverages existing pg_sage optimizer plumbing. Detects filter-pathology that managed services and Pinecone literally cannot diagnose because they don't see SQL. Highest LLM leverage.
5. **Vector-aware Inventory + Vacuum/Reindex Scheduler (4.1 + 4.7)** — bundle them. Item 4.1 is week-1 work and instantly differentiates the dashboard; item 4.7 reuses existing vacuum advisor infrastructure and addresses the second-most-reported pain (HNSW vacuum / bloat). Combined: 3 weeks for a visible win.

Items 4.6, 4.8, 4.9, 4.10 are all valuable but second-wave. 4.9 (dimension migration) is the highest-stakes feature and should be HIGH-risk Tier-3 with manual approval forever; do it last.

---

## Open Questions

- **Recall sampling cost vs accuracy.** A ground-truth pass requires either a sequential scan (expensive on 50M+ rows) or a held-out validation set. Should pg_sage maintain a per-table "golden set" of ~1000 representative query vectors with pre-computed exact neighbors, refreshed monthly? How to detect when the golden set itself has gone stale (data distribution shift)?
- **Per-query-shape stability.** Query shapes change frequently in agent workloads (LLMs generate slightly different SQL each call). How aggressive should pg_sage's normalization be before declaring two queries "the same shape" for autotuning purposes?
- **Cross-extension migration recommendations.** When a user is on pgvector and we believe pgvectorscale or VectorChord would help, do we recommend migration? RDS users *cannot* install either. AlloyDB users get ScaNN. The recommendation must be platform-aware. Should pg_sage maintain a managed-Postgres extension matrix and gate recommendations on it?
- **LLM trust for SQL rewrites.** Item 4.5 (hybrid plan analysis) sometimes wants to recommend a query rewrite, not just a knob change. How to safely surface "rewrite your query as a CTE" without crossing into "modify application code"?
- **Embedding model fingerprinting.** Detecting silent model swaps (a stable API endpoint changing the underlying model) ideally uses vector-norm and direction histograms over time. Is that signal robust enough across providers (OpenAI, Cohere, Voyage, local nomic, sentence-transformers)? What's the false-positive rate in normal data drift?
- **Multi-tenant fairness vs aggregate optimization.** When tuning a global HNSW index, tenant A's recall might improve while tenant B's degrades. Should pg_sage's recall measurement be per-tenant? That multiplies sampling cost by the tenant count.
- **Trust ramp for vector operations.** Building or rebuilding a 100GB HNSW index is a several-hour, several-tens-of-GB-of-RAM operation. Does it belong in the existing OBSERVATION → ADVISORY → AUTONOMOUS ramp, or does it need its own "vector-only" trust track with longer dwell times?

---

## Sources

Primary references cited inline above. Additional high-signal sources reviewed but not pulled directly:

- [pgvector benchmarks lie](https://thenewstack.io/why-pgvector-benchmarks-lie/) — The New Stack, on why ANN benchmark numbers don't transfer.
- [VectorDBBench](https://github.com/zilliztech/VectorDBBench) — the standard cross-DB harness; supports pgvector, pgvectorscale, VectorChord, Pinecone, Qdrant, Weaviate, Milvus.
- [Crunchy Data: HNSW Indexes](https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector) — production tuning playbook.
- [Supabase HNSW guide](https://supabase.com/docs/guides/ai/vector-indexes/hnsw-indexes) — managed-Postgres production posture.
- [QueryPlane pgvector tuning](https://queryplane.com/docs/blog/pgvector-hnsw-tuning-guide) — published m/ef_construction grids.
- [Tiger pgvector vs Pinecone](https://www.tigerdata.com/blog/pgvector-is-now-as-fast-as-pinecone-at-75-less-cost) — the canonical pro-Postgres argument.
- [Anup: pgvector doesn't scale](https://www.anup.io/pgvector-doesnt-scale/) — production failure-mode catalog.
- [Mem0 State of AI Agent Memory 2026](https://mem0.ai/blog/state-of-ai-agent-memory-2026) — agent-memory market.
- [TianPan: Semantic Caching](https://tianpan.co/blog/2026-04-10-semantic-caching-llm-production) — production cache hit-rate reality.
