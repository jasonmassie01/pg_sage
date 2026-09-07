# pg_sage v1 Research: HNSW / IVFFlat Autotuning — Algorithm Design and Prior Art

**Author:** research subagent
**Date:** 2026-04-29
**Scope:** algorithm + prior art for an autoresearch / autotuner that finds optimal HNSW (and IVFFlat) parameters for a given recall/latency target on a real workload. Vector-search landscape covered separately.

---

## 1. HNSW and IVFFlat parameter mechanics

### 1.1 HNSW

HNSW (Hierarchical Navigable Small World) builds a multi-layer proximity graph. pgvector exposes three knobs ([pgvector HNSW configuration parameters](https://deepwiki.com/pgvector/pgvector/5.1.4-hnsw-configuration-parameters), [Crunchy Data HNSW guide](https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector)):

| Param | Set at | Default | Range | Costs | Buys |
|---|---|---|---|---|---|
| `m` | build | 16 | 5–48 | Linear in graph fan-out → linear in index size and memory; superlinear in build time at large `m` | Recall, especially at high dimensions and high recall targets. Bigger `m` is better for high-D / high-recall regimes; smaller `m` works fine at lower recall ([HNSW paper, Malkov & Yashunin 2016](https://arxiv.org/abs/1603.09320)) |
| `ef_construction` | build | 64 | must be ≥ 2·m | Linear-ish in build time; minimal index-size impact | Graph quality (better neighbor selection during insert). Diminishing returns past ~200 |
| `ef_search` (`hnsw.ef_search` GUC) | query | 40 | 1–1000 | Linear in query latency: candidate priority queue size ([Neon optimization guide](https://neon.com/docs/ai/ai-vector-search-optimization)) | Recall@k on each query. The cheap, online lever |

Practical observations:

- A 1M-row, 1536-dim index can take 6+ minutes to build and exceed 8 GB on disk ([Crunchy Data](https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector)).
- Build is gated by `maintenance_work_mem`. If the graph spills to disk the build slows by an order of magnitude ([pgvector#430](https://github.com/pgvector/pgvector/issues/430)).
- Query latency is roughly linear in `ef_search` for a fixed `m`; recall climbs steeply at low `ef_search` and flattens past the recall-target knee ([Aurora pgvector blog](https://aws.amazon.com/blogs/database/accelerate-hnsw-indexing-and-searching-with-pgvector-on-amazon-aurora-postgresql-compatible-edition-and-amazon-rds-for-postgresql/)).
- pgvector 0.8.0 added `iterative_scan` for filtered queries, which interacts with `ef_search` and `hnsw.max_scan_tuples` ([Aurora 0.8.0 blog](https://aws.amazon.com/blogs/database/supercharging-vector-search-performance-and-relevance-with-pgvector-0-8-0-on-amazon-aurora-postgresql/)). Any autotuner must measure with the same scan mode the application uses.

### 1.2 IVFFlat

IVFFlat partitions vectors with k-means into `lists` Voronoi cells; queries scan the closest `probes` cells.

| Param | Set at | Costs | Buys |
|---|---|---|---|
| `lists` | build | More lists → more centroids, more bookkeeping; build cost dominated by k-means | Selectivity. Rule of thumb: `lists = rows / 1000` up to 1M, then `sqrt(rows)` ([pgvector README](https://github.com/pgvector/pgvector)) |
| `probes` (`ivfflat.probes` GUC) | query | Linear in query latency: scan k cells | Recall. Rule of thumb: `probes = sqrt(lists)` |

IVFFlat is cheaper to build and rebuild than HNSW but plateaus at lower recall for the same QPS, especially at high dimension ([Tembo IVF vs HNSW](https://www.tembo.io/blog/vector-indexes-in-pgvector)). pg_sage should keep IVFFlat as a build-budget-constrained fallback; HNSW is the default target.

---

## 2. Existing autotuning approaches

### 2.1 ANN-Benchmarks: the Pareto frontier as a primitive

[ann-benchmarks.com](https://ann-benchmarks.com/) is the canonical methodology. For each algorithm, it runs a parameter sweep, plots every configuration as a point on (recall@k, QPS), and reports the **Pareto frontier** of non-dominated configurations ([Aumüller et al. 2017 PDF](https://itu.dk/~maau/additional/sisap2017-preprint.pdf)). All datasets use a held-out 10k query set with brute-force ground truth. This is the de-facto evaluation contract for any vector-index autotuner.

Key implication for pg_sage: the goal is not "the best config" — it is **the Pareto-optimal config that meets the target**. Multiple acceptable configs exist; the autotuner picks the cheapest (build time / memory) that clears the recall and latency thresholds.

### 2.2 VectorDBBench (Zilliz)

[VectorDBBench](https://github.com/zilliztech/VectorDBBench) is a config-sweep harness. It runs four phases (load, optimize, search, filter-search) and reports QPS, recall, and p99 latency for each predefined config ([Zilliz docs](https://docs.zilliz.com/docs/perf-benchmark-vectordb)). It does not autotune — it benchmarks human-chosen configs against canned datasets (SIFT, GIST, Cohere, OpenAI). Useful as a **calibration suite** for pg_sage: if our recommended config beats VDBBench's "default" by N% on a synthetic dataset that resembles the user's workload, we have evidence the recommendation is real.

### 2.3 Milvus / Weaviate / Qdrant

None ship a full autotuner. Each ships docs and tuning playbooks ([Qdrant resource optimization](https://qdrant.tech/articles/vector-search-resource-optimization/), [Weaviate ANN benchmark](https://docs.weaviate.io/weaviate/benchmarks/ann/)). They expose `ef` at query time so users can tune online without rebuild. Qdrant's tutorials emphasize the same recall-vs-latency Pareto sweep ann-benchmarks codified.

### 2.4 Vespa: adaptive search, not autotuning

Vespa added **ACORN-1** (filter-first traversal) and **adaptive beam search** in 2025 ([Vespa blog](https://blog.vespa.ai/additions-to-hnsw/)). Adaptive beam uses a distance-based termination condition with an `exploration-slack` parameter — it does not auto-pick `ef`, but lets one configured `ef` behave well across a wider query range by terminating early. Implication: a single "good enough" `ef` with adaptive termination may dominate a fragile perfectly-tuned constant.

### 2.5 Pinecone serverless: hide the params

Pinecone serverless exposes only cloud, region, dimension, and metric — no `m`, `ef`, or list count ([Pinecone docs](https://docs.pinecone.io/guides/indexes/understanding-indexes)). Pinecone has not published the algorithm, but published indicators suggest a workload-adaptive sharding/index strategy plus aggressive defaults rather than per-tenant Bayesian search. Lesson: most users want a recommendation, not a slider.

### 2.6 OtterTune-style Bayesian DB tuning

[OtterTune](https://db.cs.cmu.edu/papers/2017/p1009-van-aken.pdf) uses Gaussian Process regression with Expected Improvement (EI) acquisition for single-objective DB knob tuning. Single-objective is the wrong tool here — recall and QPS conflict, and the user wants the **Pareto frontier**, not a single optimum ([Tsinghua tuning survey](https://dbgroup.cs.tsinghua.edu.cn/ligl/papers/tuning-survey.pdf)).

### 2.7 VDTuner (2024) — directly applicable

[VDTuner](https://arxiv.org/pdf/2404.10413) (Yang et al., ICDE 2024) is the closest prior art:

- **Multi-objective Bayesian optimization** with **Expected Hypervolume Improvement (EHVI)** as acquisition. Recall and QPS are both objectives; the optimizer learns the Pareto frontier, not a single point.
- Reports **14% QPS / 186% recall** improvement over defaults, **3.57× faster tuning** vs single-objective baselines.
- Explicitly identifies OtterTune's single-objective limitation for vector DBs.

VDTuner is the algorithm pg_sage should use as the spine of its autotuner.

### 2.8 FastPGT (2026) — efficient evaluation phase

[FastPGT](https://arxiv.org/html/2602.11573v1) extends VDTuner:

- The **evaluation phase** (rebuild + query) is ~98.67% of tuning cost; the BO recommender is cheap.
- Batches candidates per iteration (mmEHVI) and shares distance computations across simultaneous builds (mKANNS, mPrune). **2.37× speedup** vs VDTuner at similar quality.

Implication for pg_sage: we cannot parallel-build against production, but we can on a staging replica or sample, where FastPGT's shared-computation tricks apply.

### 2.9 Earlier work

- [Jääsaari et al. 2018](https://arxiv.org/abs/1812.07484) targets randomized space-partitioning trees, not graphs. Tuning folds into the build with minimal overhead — tuning during build is cheaper than tuning by repeated rebuilds.
- [Original HNSW paper](https://arxiv.org/abs/1603.09320) heuristics: smaller `m` for low recall and low dimension; bigger `m` for high dimension and high recall. These seed the search space.

---

## 3. The pg_sage autoresearch loop — design

### 3.1 Inputs and outputs

**Inputs:**
- Connection string for the target database (advisory mode) or a staging replica (autonomous mode).
- Target table, vector column, distance operator (`<->`, `<=>`, `<#>`), `k`.
- Optional query set (supplied by user) **or** sampled from `pg_stat_statements` filtered to queries against the vector column.
- Targets: `recall@k ≥ R` (default 0.95), `p99_latency ≤ L_ms` (default 50), `build_time_budget ≤ B_min`, `memory_budget ≤ M_gb`.
- Trust level (`monitor` | `advisory` | `autonomous`).

**Outputs:**
- Recommended `(index_type, m, ef_construction, ef_search)` or `(lists, probes)`.
- Predicted recall, p50/p95/p99 latency, build time, index size — with confidence intervals.
- A SQL plan: `CREATE INDEX CONCURRENTLY ...` plus `SET hnsw.ef_search = ...`.
- A rollback plan.

### 3.2 Workload capture

Two paths:
1. **User-supplied query set.** Highest fidelity; the user provides 1k–10k representative query vectors (or text + the embedding model).
2. **pg_stat_statements harvest.** pg_sage already parses pg_stat_statements. Filter for queries containing `<->`, `<=>`, `<#>` against the vector column, sample by frequency-weighted random selection, capture parameter values from `pg_stat_statements.query` (after parameter normalization). Floor of 200 queries; upgrade to 1k+ if available. ([pgvector monitoring guidance](https://github.com/pgvector/pgvector#performance))

The captured query set **is the workload definition**. Any drift in this set (Section 5) invalidates the tune.

### 3.3 Ground-truth: sample-based brute force

Naive brute force on a 10M-row table is infeasible — a single query is O(N·D) and an autotuner needs ground truth for every probe query.

Approach (matches ann-benchmarks practice scaled down):

1. **Sample S rows uniformly** from the target table where S ≤ 100k. The sample is fixed and reused across all autotuner iterations.
2. **Sample Q probe queries** (default Q = 500, max 2000) from the captured workload.
3. **Compute brute-force top-k for each probe query against the S-sample using exact search**: pgvector documents this — `BEGIN; SET LOCAL enable_indexscan = off; SELECT ... ORDER BY embedding <-> $1 LIMIT k;` ([pgvector README, "Improve recall"](https://github.com/pgvector/pgvector#improving-recall)). Run it on a **read replica** or off-hours; throttle with statement_timeout and `pg_sleep` between probes to cap CPU.
4. **Recall is measured against the S-sample**, not the full 10M. This is **sampled recall**, biased upward by the smaller candidate pool, but the bias is **constant across configs** — so it is valid for *ranking* configs against each other, which is exactly what the BO loop needs. For final reporting, we re-validate the winner against a larger sample (say 1M rows or full brute force on a single off-peak window) before recommending.

This sampled-recall approach is standard in ANN evaluation literature ([dice-research benchmark methodology](https://papers.dice-research.org/2025/ESWC_ANN_Benchmark/public.pdf)) and is how pg_sage avoids melting prod.

Rough cost: 500 queries × 100k-row brute force at 1536-dim ≈ 500 × 250 ms = 2 minutes. One-time cost per tuning campaign.

### 3.4 Search strategy

Budget hierarchy:

| Strategy | When to use | Tradeoff |
|---|---|---|
| Smart defaults from heuristics | First 30 seconds: emit a sane recommendation immediately based on row count and dimension (HNSW paper heuristics + ann-benchmarks priors) | Zero tuning cost; mediocre quality |
| Online `ef_search` sweep | Cheap: needs only the existing index. Sweep `ef_search ∈ {10, 20, 40, 80, 160, 320}` and measure recall + p99. | Cannot change `m` or `ef_construction` without rebuild |
| Multi-objective BO over (m, ef_construction, ef_search) | Autonomous mode; user has consented to staging-replica rebuilds | High quality; rebuild cost dominates (per FastPGT, 98% of cost). Use EHVI, ~20–40 evaluations to converge per VDTuner |
| Grid search | Fallback when BO library not available, or for ≤ 12 candidate cells | Wastes evaluations; predictable |
| Evolutionary (CMA-ES) | Not recommended | More evaluations than BO for our small (≤3-D) parameter space |

Default: `ef_search` sweep first (online, no rebuild). If targets unmet, escalate to BO over the build params on a staging replica.

### 3.5 HypoPG and online estimation

[HypoPG](https://hypopg.readthedocs.io/en/rel1_stable/index.html) supports hypothetical B-tree/hash indexes but **does not model HNSW or IVFFlat cost**. We cannot estimate latency or recall of a vector index without building it. Every BO evaluation costs a rebuild.

Mitigations:
1. Build on a **staging replica** (logical-replication slave or logical dump), not prod.
2. Build on a **sample table** (~5% of rows). Pareto ranking of configs is approximately preserved at sample scale ([VDTuner](https://arxiv.org/pdf/2404.10413)). Winner is rebuilt and re-measured on full data before commit.
3. Cache results: never re-evaluate a tested `(m, ef_construction)` pair.

### 3.6 Stop criteria

Stop on **any** of: (1) targets met and EHVI improvement < 1% over last 3 iterations; (2) BO budget exhausted (default 30 iterations); (3) wall-clock exhausted (default 4 h); (4) Pareto-front stagnation — surrogate predicts no unexplored region meets targets.

In case 4, pg_sage emits a *negative* recommendation: "no HNSW config meets recall ≥ 0.95 at p99 ≤ 50 ms with this dimension and dataset; relax targets or downsize embedding." Itself valuable — it stops the user chasing an unattainable target.

### 3.7 Trust-ramp integration

pg_sage's existing trust ramp (`monitor` → `advisory` → `autonomous`) maps cleanly:

- **monitor**: capture workload, compute current recall via the trick from Section 3.3 against the existing index, emit "your current recall@10 is 0.81; target is 0.95" — no actions taken.
- **advisory**: run the autotuner on a staging replica (or with explicit user opt-in, against prod with throttling), produce a recommendation as a SQL plan: predicted recall + latency + build time + memory. User reviews and runs it manually.
- **autonomous**: pg_sage executes the rebuild during a configured maintenance window with the shadow-index + atomic-rename pattern below, validates against held-out queries, and rolls back if regression detected.

### 3.8 Avoiding catastrophic regressions: shadow index + atomic swap

HNSW rebuild is destructive: pgvector supports `REINDEX CONCURRENTLY` on HNSW (PG 12+, pgvector 0.5+), but a parameter change (`m`, `ef_construction`) requires a new index, not a reindex.

The safe pattern, documented in the embedding-drift literature ([TianPan: Embedding Models in Production](https://tianpan.co/blog/2026-04-09-embedding-models-production-versioning-index-drift)):

1. `CREATE INDEX CONCURRENTLY idx_v2 USING hnsw (...) WITH (m = M', ef_construction = E')` — non-blocking, runs alongside reads on the old index.
2. Validate v2 against the held-out probe set: recall, p99, plan stability (`EXPLAIN`).
3. If validation passes, **transactional swap**: `BEGIN; DROP INDEX idx_v1; ALTER INDEX idx_v2 RENAME TO idx_v1; COMMIT;` — atomic from the application's perspective, with `SET hnsw.ef_search = E_new` configured at the role/database level so the new query-time param flips with the index.
4. If validation fails, drop v2 and keep v1. Zero rollback cost.

Caveats:
- `CREATE INDEX CONCURRENTLY` doubles disk usage during the build. pg_sage must check `pg_database_size` headroom before starting.
- Concurrent build is slower than blocking build (no parallel workers in current pgvector for concurrent builds — see [pgvector#822](https://github.com/pgvector/pgvector/issues/822)). Estimate from a sample build before committing to full build.

### 3.9 Pseudocode sketch

```
fn autotune(target_table, vec_col, k, recall_target=0.95, p99_target_ms=50,
            build_budget_min=120, mem_budget_gb=16, trust_level):

    # --- Phase 0: workload capture ---
    queries = capture_queries(pg_stat_statements, vec_col, n=1000)
    if len(queries) < 200:
        return error("not enough vector queries in pg_stat_statements; provide explicit set")

    # --- Phase 1: ground-truth on a sample ---
    sample = sample_rows(target_table, n=min(100_000, table_rows))
    probes = sample(queries, n=500)
    truth = {}
    for q in probes:                      # off-peak, throttled, on replica if available
        truth[q] = brute_force_topk(sample, q, k, exact=True)

    # --- Phase 2: cheap online sweep on existing index (if any) ---
    if existing_index_is_hnsw():
        for ef in [10,20,40,80,160,320]:
            r, p99 = measure(existing_index, probes, truth, ef_search=ef)
            record(ef, r, p99)
        if any config meets (recall_target, p99_target):
            return cheapest_meeting_config()

    # --- Phase 3: multi-objective BO over build params ---
    space = {
        m:               IntUniform(8, 48),
        ef_construction: IntUniform(64, 400),
        ef_search:       IntUniform(20, 500),     # tuned post-build
    }
    bo = MultiObjectiveBO(objectives=["recall","qps"], acquisition="EHVI")
    best_pareto = []
    for iter in range(30):
        cand = bo.suggest(space)
        if cand violates mem_budget or build_budget: bo.tell(cand, infeasible); continue
        if cand in cache: r,p99,build_t,size = cache[cand]
        else:
            build_t, size = build_index_on_staging(sample, cand.m, cand.ef_construction)
            r, qps, p99 = measure_on_staging(probes, truth, cand.ef_search)
            cache[cand] = (r, p99, build_t, size)
        bo.tell(cand, recall=r, qps=qps)
        update_pareto(best_pareto, cand, r, qps, p99, build_t)
        if converged(best_pareto, last_n=3, eps=0.01) and meets_targets(best_pareto):
            break
        if wall_clock > 4h: break

    winner = pick_cheapest_meeting_targets(best_pareto, recall_target, p99_target_ms)
    if winner is None:
        return advisory("no config meets targets — Pareto frontier attached; relax targets")

    # --- Phase 4: full-data validation ---
    full_build_t, full_size = estimate_full_build(winner, sample, table_rows)
    if full_build_t > build_budget_min: return advisory(winner, with_warning="build exceeds budget")
    if trust_level == AUTONOMOUS:
        rebuild_with_shadow_swap(winner)             # Section 3.8
        post_validate_against_holdout()
    else:
        emit_sql_plan(winner)
```

---

## 4. Brute-force ground truth at scale

The critical insight: **we never need brute-force on the full 10M-row table**. We need brute-force on a *fixed sample* large enough that recall ranking between candidate configs is stable.

Empirical guidance from ANN-Benchmarks runs ([Aumüller et al.](https://arxiv.org/pdf/1807.05614)):

- 10k probe queries against a 1M-row corpus is the standard.
- Sampled recall at S=100k correlates with true recall at S=10M with Spearman ρ > 0.95 across configs — i.e. **rank-preserving** even when absolute recall differs.
- For pg_sage: 100k sample + 500 probes is the sweet spot for sub-2-minute ground-truth setup. Bump to 500k + 1000 probes for high-stakes tuning campaigns.

Three failure cases to guard against:

1. **Sample doesn't represent the corpus.** If the corpus has clusters and we sample uniformly, rare clusters are under-represented. Mitigation: stratify the sample by any natural partition (tenant_id, category, time bucket).
2. **Probe queries don't represent the workload.** Frequency-weight the probe sample from pg_stat_statements; over-weight slow queries.
3. **Brute-force on prod melts the DB.** Always run on a replica; if no replica, throttle (`pg_sleep`, statement_timeout, low `work_mem`), schedule for off-peak, or use `pg_dump` of the sample to a sidecar Postgres pg_sage already manages.

---

## 5. Failure modes the autotuner must test against

These are the regressions a tuned config can silently induce. Each must have a corresponding test in pg_sage's verification suite (per CLAUDE.md "Required Test Categories"):

| Failure mode | Detection | Mitigation |
|---|---|---|
| **Embedding model swap** (`text-embedding-ada-002` → `text-embedding-3-small`, dimension or geometry change) | Compare `pg_sage`-tracked embedding model fingerprint (hash of a known input) against current; refuse to use a tune produced under a different model ([TianPan: index drift](https://tianpan.co/blog/2026-04-09-embedding-models-production-versioning-index-drift)) | Invalidate cached tune; re-run autotuner |
| **Distribution drift** (the corpus shifts — new domain, new product line) | Periodic recall-spot-check: sample 100 fresh queries, brute-force them against current sample, measure recall against current index. If recall drops > 5 pp, alert | Re-sample corpus, re-run autotuner |
| **Cardinality changes** (10x growth) | Track row count, alert at 2x since last tune. HNSW recall at fixed `m` degrades modestly with N; build time and memory grow linearly | Re-run autotuner if size or recall thresholds breached |
| **Dimensionality change** | Schema change detector: trigger on column type change | Hard-block the tune; require user confirm |
| **Query distribution shift between tuning and production** | Compare the tuning probe distribution to the live `pg_stat_statements` query distribution via centroid-distance. If drift > threshold, re-tune | Tune on representative recent traffic, not on a stale capture |
| **Markdown-wrapped LLM defaults from a Gemini path** | Lessons from CLAUDE.md: any LLM-generated tune justification must `stripToJSON` the response | Apply the existing utility |
| **Confidence-score boundaries** | Per CLAUDE.md: verify the optimizer reaches advisory threshold (0.5) without HypoPG. We confirmed Section 3.5 — HypoPG cannot model HNSW today, so pg_sage's confidence model must derive confidence from BO posterior variance, not HypoPG presence | Wire BO posterior into the existing trust score |

A tuning record should include: timestamp, embedding model fingerprint, table size, sample size, probe count, brute-force window, BO iteration count, achieved Pareto frontier, chosen config, post-validation results. If any of those inputs change materially, the tune is stale and must be rerun.

---

## 6. Risks and Open Questions

1. **HypoPG cannot model HNSW today.** Every BO evaluation is a real index build. Until pgvector or HypoPG land cost-modeling for proximity graphs (no such PR is open as of 2026-04), the autotuner is bottlenecked by build throughput, exactly as FastPGT predicted. Open question: is it worth pg_sage upstreaming a HypoPG-pgvector cost extension, or is sample-based estimation sufficient?

2. **Sampled recall is biased.** Ranking is preserved; absolute recall is not. The validation phase (Section 3.9, Phase 4) does a full-data measurement, but full-data brute-force ground truth at 10M+ rows is itself prohibitive. Open question: how much corpus is "enough" — and do we accept 99% confidence intervals on the final reported recall instead of point estimates?

3. **`CREATE INDEX CONCURRENTLY` on HNSW does not parallelize today** ([pgvector#822](https://github.com/pgvector/pgvector/issues/822)). Concurrent build can take 5–10× longer than blocking build. The shadow-swap pattern in Section 3.8 may exceed the user's build-time budget on large tables. Workaround: blocking build with a short read-only window during off-peak, communicated as a maintenance event.

4. **pgvector iterative_scan changes the recall function.** A tune done with `iterative_scan = off` can mispredict latency once the application turns it on for filtered queries ([pgvector 0.8.0](https://aws.amazon.com/blogs/database/supercharging-vector-search-performance-and-relevance-with-pgvector-0-8-0-on-amazon-aurora-postgresql/)). Autotuner must record and probe with the same scan mode the workload uses.

5. **Multi-tenant fleets break the assumption of one workload.** pg_sage's fleet mode may discover different tenant query distributions on the same table. Open question: per-tenant tunes (impossible — one index per table) or a tune that minimizes worst-case tenant recall (multi-objective with tenant-grouped recall as the objective)?

6. **Trust-ramp at "autonomous" requires the database to lose either downtime or disk headroom for the shadow build.** No autonomous tune should run if free disk < 1.5× current index size. pg_sage already tracks disk; this is a precondition, not a new check.

7. **VDTuner's headline numbers (14% QPS, 186% recall) come from defaults that were bad to begin with** — Milvus's `nlist`/`nprobe` defaults at 1M rows are notoriously underprovisioned. pg_sage's advantage over defaults will be smaller because pgvector's `m=16, ef_construction=64` are reasonable mid-recall values. Honest expectation: 5–30% recall improvement at fixed latency, or 2–5× latency reduction at fixed recall, on workloads where the user has not already hand-tuned.

8. **Stop criterion validity.** EHVI hypervolume convergence is a heuristic. We may converge to a local Pareto front and miss a better region. Mitigation: random-restart 2 of every 30 BO suggestions to maintain exploration. This is standard in [Optuna's TPE sampler](https://optuna.readthedocs.io/en/stable/tutorial/10_key_features/003_efficient_optimization_algorithms.html) and similar frameworks.

9. **No published benchmark of HNSW autotuners on pgvector specifically.** VDTuner targeted Milvus; FastPGT targeted Vamana/NSG/HNSW in C++ ANN libraries. pg_sage's internal benchmark, when built, will be the first public number on pgvector autotuning quality.

10. **Spec gap: what does "advisory" output look like?** A SQL block? A diff? A graph plot in the dashboard? The web UI direction (per project memory) suggests the latter — render the Pareto frontier in the dashboard with a "commit this config" button. Out of scope for this research doc; flagged for the spec.
