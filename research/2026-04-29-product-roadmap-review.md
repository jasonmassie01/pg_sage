# pg_sage Product Roadmap Review

Research date: 2026-04-30  
Repo: `C:/Users/jmass/pg_sage`  
GitHub state: `git pull --ff-only origin master` returned `Already up to date`; HEAD `113acbd`; `git describe` = `v1-1-g113acbd`.  
Note: no `v1.0` branch or tag exists on `origin`; the remote has a `v1` tag and current `master`.

## Executive Recommendation

pg_sage should lean into the gap between Postgres advisors and autonomous databases:

> Explain what is hurting, prove the smallest safe change, apply it only under trust policy, and keep checking whether the recommendation is still true.

The current repo is already past the older README/roadmap narrative. It has RCA, lock-chain, cases/shadow-mode, schema/migration safety, runaway query handling, stale stats, hint revalidation, and config metadata surfaces in code. The next high-leverage work is not generic "more rules"; it is productizing closed-loop workflows for newer workload shapes, especially vector search and agent-created databases.

Top recommendations:

| Rank | Recommendation | Why Now | First Slice |
|---|---|---|---|
| P0 | Vector Workload Advisor + HNSW Autoresearch | pg_sage has almost no vector-specific product surface, while pgvector pain is exploding around filtering, memory, recall, and index misuse. | Inventory vector columns/indexes, detect bad query shapes, benchmark `SET LOCAL hnsw.ef_search` against exact-search recall on sampled queries. |
| P0 | Extension Health Doctor | Extensions are a major source of hidden production risk: installed but stale, required but absent, loaded but misconfigured, provider-blocked, or silently producing bad planner behavior. | Inventory extension state, required GUC/preload state, provider support, version drift, dependent objects, and query patterns that imply missing/misused extensions. |
| P0 | JSON/JSONB Workload Advisor | JSONB is where Postgres teams accidentally recreate a document database with relational indexes bolted on. The current optimizer mentions GIN, but not enough to diagnose operator classes, expression indexes, generated columns, statistics, and schema drift. | Classify JSON query shapes, recommend `jsonb_path_ops` vs `jsonb_ops`, expression indexes, generated columns, extended stats, or schema normalization with LLM-written rationale. |
| P0 | Recommendation Proof Loop | The repo has cases, shadow mode, hint revalidation, and action logs, but docs and UX lag the "prove it improved" story. | Case detail page and API that shows evidence before, action, after, revalidation, rollback status, and confidence decay. |
| P1 | Agent Database Guard | AI agents are now provisioning schemas, migrations, vector stores, cron jobs, and RLS policies. They need a database-side safety inspector. | Read schema diffs and live DB state, produce PR/check reports for unsafe DDL, missing constraints, RLS gaps, vector anti-patterns, and unbounded costs. |
| P1 | Sandbox Benchmark Lab | Closed-loop DDL needs production-like validation without production blast radius. Neon/DBLab-style branches are a natural substrate. | Support "benchmark this recommendation on a disposable clone" as a workflow, with teardown, cost cap, and evidence artifact. |
| P1 | Provider-Aware Operations Intelligence | Managed Postgres has extension, permission, restart, pooler, replica, and index-build constraints that generic advice gets wrong. | Expand provider capability matrix and make each recommendation include "can this be done on RDS/Aurora/Cloud SQL/AlloyDB/Supabase/Neon?" |

## Current-State Assessment

The current codebase is stronger than public docs imply.

- The core architecture is still the right shape: an external Go sidecar, no required Postgres extension, optional LLM/extension depth, embedded API/UI, and Prometheus.
- Repo inventory found 75 unique route patterns, while README/CLAUDE still cite older API counts.
- v0.8.5/v0.9/v0.10 themes are partly present in code: hint revalidation, stale stats and scoped ANALYZE, work_mem promotion, extension drift, RCA/logwatch, lock chains, runaway query policies, schema lint, migration safety, incidents/cases, and shadow-mode proof.
- The product has moved toward a cases-first UI, with Findings/Forecasts/Query Hints/Schema Health/Incidents feeding case workflows.
- Safety is a real differentiator: trust ramp, emergency stop, action risk gates, maintenance windows, rollback SQL, LLM budgets, provider checks, and circuit breakers.
- Docs lag code in important places: API surface, cases, RCA/logwatch, schema lint, migration safety, shadow mode, extension drift behavior, and version/release claims.
- Claude's `docs/ROADMAP_v1.x_addendum.md` adds useful pressure on foundation readiness, but it should be treated as curated input rather than an authoritative schedule. Local code confirms the large-file/debt signals and shows an important risk-taxonomy gap: `ActionContract` exists, but optimizer recommendations still map `ActionLevel` into `Finding.ActionRisk`, while trust gates expect `safe`, `moderate`, or `high_risk`.

Local evidence:

- `research/2026-04-29-repo-inventory.md`
- `research/2026-04-29-community-pain-points.md`
- `research/2026-04-29-vector-search.md`
- `research/2026-04-29-competitive-landscape.md`
- `docs/ROADMAP_v1.x_addendum.md`
- `research/v1_codebase_reality_check.md`

## Community Pain Signals

The repeated pattern across Reddit, Stack Overflow, mailing lists, and blogs is that people can see symptoms, but cannot safely choose the next action.

| Pain | Community Signal | pg_sage Opportunity |
|---|---|---|
| Slow query triage | Users are told to stitch together logs, `pg_stat_statements`, EXPLAIN, bloat, indexes, and hardware checks. | Case-based triage that ranks root-cause hypotheses and action safety. |
| Index confidence | Stack Overflow has many wrong-index, missing-index, and "why is my index ignored?" cases. | Explain "why this index," write overhead, rollback, and post-change proof. |
| Autovacuum/bloat | Reddit threads show confusion around VACUUM FULL, long transactions, and autovacuum aggressiveness. | Bloat prevention and xmin blocker RCA, not just bloat percent. |
| Lock and DDL outages | Long transactions and queued ACCESS EXCLUSIVE locks are still common self-inflicted outages. | Migration safety advisor and lock-chain resolution with trust-gated actions. |
| Managed Postgres | RDS/Aurora/Cloud SQL users hit provider-specific limits and extension friction. | Provider-aware recommendation feasibility and fallback paths. |
| Vector search | pgvector users struggle with HNSW memory, filters, index usage, build time, recall, and latency. | A vector DBA advisor that measures recall/latency and suggests safe knobs. |
| Agent schemas | Agent users want schema/docs kept current and validated without hallucination. | Agent Database Guard: deterministic DB facts for AI-generated changes. |

Representative public sources:

- Reddit bloat discussion: https://www.reddit.com/r/PostgreSQL/comments/103rxok/database_bloat/
- Reddit VACUUM FULL discussion: https://www.reddit.com/r/PostgreSQL/comments/1fl0ssh/is_it_common_to_need_to_do_regular_full_vacuum_on/
- Stack Overflow pgvector HNSW index not used: https://stackoverflow.com/questions/77757239/select-query-not-using-pgvector-hnsw-index
- Stack Overflow filtered pgvector plan issue: https://stackoverflow.com/questions/78759522/why-is-the-pgvector-index-not-being-used
- Reddit HNSW memory pressure: https://www.reddit.com/r/Supabase/comments/1snp27q/pgvector_hnsw_index_33_gb_causing_shared_buffers/
- Reddit agent/schema drift workflow: https://www.reddit.com/r/vibecoding/comments/1kx72kj/how_do_you_keep_your_ai_agents_vibing_with_your/

## Competitive Landscape

pg_sage should not compete as another observability dashboard. It should compete as the explainable, safety-gated action layer.

| Competitor Class | Examples | What They Teach | Opening for pg_sage |
|---|---|---|---|
| Postgres advisors | pganalyze, pgMustard, PoWA, Supabase index_advisor, Neon online_advisor, Cloud SQL Index Advisor | Workload-aware recommendations matter; deterministic planner logic is trusted. | Close the loop with execution, rollback, revalidation, and fleet policy. |
| Observability/APM | Datadog, New Relic, pgBadger, PgHero, AWS Database Insights | Historical visibility and APM/deploy correlation are valuable. | Integrate traces/deploy markers; do not become an APM clone. |
| Autonomous DBs | Oracle Autonomous DB, Azure SQL automatic tuning | Closed-loop tuning is credible when conservative defaults and rollback exist. | Bring transparent autonomous ops to ordinary Postgres. |
| Sandbox/branching | Postgres.ai DBLab, Neon branches | Production-like testing de-risks DDL and tuning. | Use clones as validation backends for pg_sage recommendations. |
| Cross-engine advisors | MongoDB Performance Advisor, MySQL HeatWave advisors | Rank by workload impact; avoid blind index creation. | Make every recommendation include impact, write cost, and proof. |

Key sources:

- pganalyze Indexing Engine: https://pganalyze.com/docs/indexing-engine
- pganalyze What-if analysis: https://pganalyze.com/docs/indexing-engine/what-if-analysis
- Azure SQL automatic tuning: https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-enable
- Oracle automatic indexing: https://docs.oracle.com/en/cloud/paas/autonomous-database/serverless/adbsb/autonomous-auto-index.html
- MongoDB Performance Advisor ranking: https://www.mongodb.com/docs/atlas/performance-advisor/index-ranking/

## Vector Search Strategy

### The Product Thesis

Vector search is a DBA problem now. The hard parts are no longer just "store embeddings." They are:

- Which index type should I use?
- Why is my vector index not used?
- Why does filtering destroy recall or latency?
- What `ef_search` gives recall >= 0.95 under p95 <= 50 ms?
- Should I use HNSW, IVFFlat, pgvectorscale StreamingDiskANN, partial indexes, partitioning, `halfvec`, binary quantization, or hybrid search?
- Can I rebuild this index without wrecking production?
- How do I know the result quality did not get worse?

pg_sage can own this because it already has the core primitives: workload collection, EXPLAIN, rules, optional LLM explanation, trust-ramped execution, action logs, and revalidation.

### Build a Vector Workload Inventory

Add a vector collector/advisor that detects:

- Installed `vector` extension version and provider support.
- Vector columns: table, dimension, type (`vector`, `halfvec`, `bit`, `sparsevec`), row count, table/index size.
- Vector indexes: HNSW/IVFFlat/StreamingDiskANN, opclass, index size, build params when available from index definition.
- Query shapes from `pg_stat_statements`: distance operator, `ORDER BY`, `LIMIT`, filters, tenant/user predicates, joins, CTEs, thresholds, calls, mean/p95-ish deltas.
- Bad shapes: missing `ORDER BY ... LIMIT`, ordering by a derived expression, distance threshold in the wrong query level, high-selectivity filters after ANN, vector index bypass, no scalar/partial index for common filters.
- Memory pressure: vector index size vs `shared_buffers`, cache churn signals, long build progress, write latency after HNSW insert/update.

### HNSW Autoresearch / Tuning Loop

The safe design is a two-tier tuner:

1. **Runtime-safe tuning:** benchmark query-time settings in a transaction.
   - `SET LOCAL hnsw.ef_search = N`
   - `SET LOCAL hnsw.iterative_scan = strict_order|relaxed_order`
   - `SET LOCAL hnsw.max_scan_tuples = N`
   - `SET LOCAL hnsw.scan_mem_multiplier = N`
   - IVFFlat equivalents: `ivfflat.probes`, `ivfflat.iterative_scan`, `ivfflat.max_probes`

2. **Rebuild-required tuning:** recommend, but do not casually apply.
   - HNSW `m`
   - HNSW `ef_construction`
   - IVFFlat `lists`
   - `halfvec`, binary quantization, expression indexes on subvectors
   - partial vector indexes
   - partitioning by tenant/category/time
   - pgvectorscale StreamingDiskANN / label-aware filtering

Benchmark method:

- Sample real query embeddings from safe sources or user-provided eval sets.
- Compute exact-search ground truth on a bounded sample or disposable clone.
- Run candidate settings under warm and cold-ish cache conditions.
- Measure recall@k, p50/p95/p99 latency, returned row count, buffers, rows filtered, and memory/build impact.
- Fit a Pareto frontier and produce "best config for recall target" and "best config for latency target."
- Use adaptive search, Bayesian optimization, or multi-armed bandit style exploration once the basic grid is proven.

Initial product outputs:

- "For queryid 123, `hnsw.ef_search=80` hits recall@10 0.97 at p95 41 ms; current `40` hits 0.89 at p95 28 ms."
- "This query filters to ~0.8% of rows after ANN; enable iterative scan or create partial HNSW per tenant/category."
- "Your 33 GB HNSW index is 8.25x `shared_buffers`; expect churn. Consider `halfvec`, lower `m`, partitioning by tenant, or StreamingDiskANN."
- "Vector extension < 0.8.2; parallel HNSW build has a known CVE. Upgrade before building indexes."

Vector sources:

- pgvector README and HNSW docs: https://github.com/pgvector/pgvector
- pgvector 0.8.0 iterative scans: https://www.postgresql.org/about/news/pgvector-080-released-2952/
- pgvectorscale README: https://github.com/timescale/pgvectorscale
- Tiger Data hybrid BM25 + vector positioning: https://www.tigerdata.com/search
- Supabase AI/vector docs: https://supabase.com/docs/guides/ai
- Supabase automatic embeddings: https://supabase.com/docs/guides/ai/automatic-embeddings
- Neon AI/vector optimization positioning: https://neon.com/ai
- AutoRAG-HP tuning paper: https://arxiv.org/abs/2406.19251
- Text2Schema/SchemaAgent paper: https://arxiv.org/abs/2503.23886

## Extension Health Strategy

Extensions should become a first-class pg_sage workload domain, not just a validator footnote. The repo already has extension drift detection and optimizer checks for `pg_trgm`/PostGIS availability, but the product opportunity is larger: pg_sage can explain why an extension-backed feature is absent, stale, overloaded, unsafe to update, provider-blocked, or producing surprising planner behavior.

Commonly used extension families worth first-class support:

- Observability/planning: `pg_stat_statements`, `auto_explain`, `pg_buffercache`, `pgstattuple`, `pg_prewarm`, `hypopg`.
- Maintenance: `pg_repack`, `pg_cron`, `pg_partman`.
- Search/indexing: `pg_trgm`, `btree_gin`, `btree_gist`, `unaccent`, `pgvector`.
- Domain/data: PostGIS, `hstore`, `uuid-ossp`, `pgcrypto`, `citext`.
- Replication/logical: `pglogical`, `wal2json`.

The deterministic layer should collect facts: `pg_extension`, `pg_available_extensions`, `pg_settings`, `shared_preload_libraries`, dependent objects, extension-owned functions/opclasses, provider capability, schema placement, trusted/untrusted status, version drift, and queries that imply extension use. The LLM layer should interpret the evidence: "this workload looks like it needs `pg_trgm`, but the proposed GIN index would fail on Cloud SQL unless the extension is enabled"; "PostGIS is installed but spatial queries use `ST_DWithin` without a GiST/SP-GiST index"; "pg_stat_statements is loaded but near capacity, so top-query evidence is lossy."

Good findings here should be more than "install extension X." They should answer:

- Is the extension installed, available, and usable in this database/provider?
- Is it configured correctly, including preload or restart requirements?
- Are extension-backed indexes using the right operator class?
- Are there stale extension versions or upgrade blockers?
- Which user objects depend on this extension, and what could break during update/drop?
- Is this extension solving the wrong problem where schema/query changes would be safer?

## JSON/JSONB Workload Strategy

JSON/JSONB should be treated as its own workload type. Yes, the index optimizer can recommend GIN indexes, and the current optimizer prompt says "Consider GIN for JSONB/array columns." That is necessary but not sufficient. JSON performance issues often come from choosing the wrong operator class, indexing the wrong expression, hiding high-cardinality business fields inside blobs, missing statistics, or using operators that cannot use the existing index.

The product should classify JSON query shapes before recommending anything:

- Containment queries: `payload @> '{"status":"paid"}'` often point toward GIN, frequently `jsonb_path_ops` when containment dominates.
- Key existence queries: `payload ? 'foo'`, `?|`, `?&` need broader operator support, usually not `jsonb_path_ops` alone.
- Scalar extraction filters: `payload->>'tenant_id' = 'x'` often need expression indexes, generated columns, or schema promotion.
- JSON path predicates: `jsonb_path_exists`/`@@` need separate handling from simple containment.
- Sort/group/join on JSON-derived values usually means "promote this field" before "add another GIN index."
- Large mutable JSONB columns may create write amplification, TOAST churn, and GIN pending-list pressure.

The LLM should own the "why" and the shape recommendation, but validators should guard the SQL. For example, the LLM can decide "this key has become a real column" or "this query needs a partial expression index," while deterministic checks verify column/key evidence, operator support, volatility, duplicate indexes, write rate, estimated size, provider compatibility, and whether the recommendation can be benchmarked with HypoPG or a clone.

## Claude Addendum Curation

`docs/ROADMAP_v1.x_addendum.md` mostly aligns with pg_sage's mission, but only after tightening the claims and rejecting a few architectural jumps.

Accepted and merged:

- Foundation readiness before bigger autonomy: unify action risk vocabulary, make LLM/capability failures visible, validate required extensions before emitting dependent advice, split API handlers by domain, and create a retirement track for legacy surfaces.
- Recall-first vector work: every vector recommendation should include measured or bounded recall impact, not just latency. Query-time tuning comes before rebuild-required HNSW tuning.
- HNSW autotuning phasing: `ef_search`/iterative-scan sweeps are safe first slices; `m`/`ef_construction` tuning requires a clone or staging replica because every candidate needs a real index build.
- Agent-native operation: per-agent identity correlation, workload fingerprinting, agent-memory hygiene, cost/runaway guardrails, and deploy-request-style DDL review all fit the agentic DBA mission.
- Cross-database patterns worth borrowing: deploy requests for high-risk DDL, timeline overlays for recommendation impact, SQL-native summaries, and a stable-rule `pg_sage lint` surface.

Accepted with edits:

- "Foundation blocks all features" is too broad. It should block autonomous/vector rebuild execution, not read-only vector inventory, JSON advisors, extension inventory, or documentation work.
- "Risk tiers are retroactive" is only partly true. `ActionContract` already exists, but optimizer findings still mix `ActionLevel` with `ActionRisk`; the needed work is risk-policy unification and enforcement across all producers.
- Release slotting is useful as a planning sketch, but should not become a commitment until the v1 cases/shadow-mode state and current test gaps are reconciled.

Discarded or deferred:

- Do not make `pg_sage.remember(...)` or `embed_and_insert(...)` a v1.x write API. That would turn pg_sage from a sidecar/operator into an agent runtime dependency.
- Do not allow autonomous primary HNSW shadow-index swaps until clone proof, maintenance policy, rollback semantics, and recall measurement are battle-tested.
- Do not import specific market/CVE/security claims as load-bearing roadmap facts unless they have primary-source citations and local reproduction where relevant.
- Do not chase native vector database features, natural-language SQL, cache layers, or BI/dashboard agents.

## Agent-Created Databases

### What They Will Look Like

Agent-created databases will be messy, fast-moving, and partially undocumented:

- Natural-language generated schemas and migrations.
- Branch/fork databases per coding-agent task.
- Auto-created indexes, RLS policies, triggers, cron jobs, queues, and edge-function hooks.
- Embedded vector stores next to relational state.
- Synthetic seed data and generated eval sets.
- Rapid schema churn with docs, ER diagrams, TypeScript types, OpenAPI specs, and migrations drifting out of sync.
- Multi-tenant defaults created by people who may not understand tenant isolation.
- "Good enough" schemas that launch before anyone has checked constraints, cardinality, or query shape.

### What pg_sage Should Do

Build an **Agent Database Guard**:

- Snapshot schema, constraints, indexes, RLS, grants, triggers, extensions, background jobs, and vector tables.
- Compare DB reality to migrations, application models, generated docs, and OpenAPI/GraphQL surfaces.
- Detect unsafe agent-created patterns:
  - missing primary/foreign keys
  - missing tenant predicates or RLS
  - JSON blobs replacing relational structure without reason
  - vector columns with wrong dimensions or no eval set
  - HNSW indexes on tiny tables or write-heavy tables
  - no scalar filter index for vector metadata
  - unsafe `ALTER TABLE` or non-concurrent index builds
  - redundant indexes from repeated agent attempts
  - unbounded cron/queue/embedding jobs
  - secrets or PII in seed data
- Produce PR comments, CLI reports, and case records.
- Offer a disposable branch/clone workflow for experiments.

Safety rules:

- Disposable lab DBs are allowed; production DB creation or destructive changes require explicit user intent and safety gates.
- Never use production credentials for synthetic experiments.
- Set cost and time caps on generated workloads.
- Teardown must be audited.
- Keep sensitive data local unless the user explicitly authorizes transfer.

## Recommended Backlog

### 1. Vector Workload Inventory

- Problem: pg_sage has no first-class vector awareness.
- First slice: catalog vector extension, vector columns, vector indexes, query shapes, and bad-shape findings.
- Tests: fixture tables with HNSW/IVFFlat, missing LIMIT, wrong ORDER BY expression, selective filters, extension absent.
- Metric: percent of vector workloads with actionable inventory and at least one safe finding.

### 2. HNSW Query-Time Autotuner

- Problem: users guess `ef_search` and filtering settings.
- First slice: benchmark `SET LOCAL hnsw.ef_search` candidates against exact recall on sampled queries.
- Tests: exact-vs-ANN recall harness, latency budget enforcement, no persistent GUC leakage, pooled connection safety.
- Metric: generated Pareto frontier and selected config for target recall/latency.

### 3. Vector Filter Advisor

- Problem: filters commonly break vector recall/latency.
- First slice: detect selective predicates after ANN and recommend iterative scans, partial HNSW, scalar indexes, or partitioning.
- Tests: tenant/category filters with underfilled LIMIT and index-bypass cases.
- Metric: fewer underfilled vector search results and lower p95 for filtered queries.

### 4. Recommendation Proof Loop

- Problem: advice without aftercare is not autonomous DBA.
- First slice: one unified "case" view that shows evidence, recommendation, action, after metrics, revalidation, and rollback.
- Tests: action improves, action regresses, action becomes stale, queryid disappears, index dropped externally.
- Metric: every persistent recommendation has a last-verified state.

### 5. Agent Database Guard

- Problem: AI agents will create production-adjacent database changes faster than teams can review manually.
- First slice: schema/migration/vector/RLS risk report as CLI/API and GitHub PR comment.
- Tests: unsafe DDL, missing FK, missing RLS, vector dimension mismatch, redundant indexes, stale schema docs.
- Metric: blocked unsafe migrations and reduced schema drift.

### 6. Sandbox Benchmark Lab

- Problem: safe automation needs production-like proof.
- First slice: adapter interface for "run recommendation on clone/branch"; start with local Docker fixture, design Neon/DBLab adapters.
- Tests: clone creation mocked, workload replay bounded, teardown always runs, cost cap enforced.
- Metric: recommendations with benchmark evidence before production execution.

### 7. Extension Health Doctor

- Problem: extensions fail in non-obvious ways: absent preloads, stale versions, provider gaps, unsafe upgrades, missing opclasses, lossy `pg_stat_statements`, and extension-backed query patterns with no usable indexes.
- First slice: extension inventory plus findings for drift, missing preload/config, unavailable provider capability, required extension absent for a proposed recommendation, and dependency-aware update warnings.
- Tests: installed vs available version drift, missing `shared_preload_libraries`, unavailable provider extension, `pg_trgm`/PostGIS/pgvector absent, extension-owned dependency report.
- Metric: every extension-related recommendation includes capability, config, dependency, and action-risk evidence.

### 8. JSON/JSONB Workload Advisor

- Problem: GIN advice alone is too coarse for real JSONB workloads.
- First slice: classify JSON query shapes and ask the LLM to choose among GIN operator class, expression index, generated column, extended statistics, partial index, or schema normalization.
- Tests: containment, key existence, scalar extraction, JSON path, sort/group on extracted value, high-write JSONB table, duplicate/wrong-opclass GIN index.
- Metric: JSON recommendations distinguish `jsonb_ops`, `jsonb_path_ops`, expression indexes, generated columns, and "normalize this field" cases.

### 9. Foundation Readiness Gate

- Problem: new autonomous theaters magnify existing safety and maintainability debt.
- First slice: unify action risk taxonomy across findings, optimizer recommendations, action contracts, approval readiness, and executor trust gates; make LLM/provider/extension capability failures visible findings.
- Tests: optimizer recommendation cannot carry `advisory` as an executable risk tier, missing LLM config emits degraded-mode finding, missing `pg_hint_plan`/`pg_trgm`/`pgvector` blocks dependent recommendations with a reason.
- Metric: every actionable recommendation has an explicit action type, risk tier, evidence bundle, provider capability, precheck list, and postcheck list before it can be queued or executed.

### 10. Deploy Requests and Rule Surfaces

- Problem: high-risk DDL and agent-generated migrations need a review artifact, not just a warning.
- First slice: produce deploy-request-shaped artifacts for high-risk DDL with lock/rewrite analysis, forward-fix plan, verification SQL, and rollback or mitigation notes.
- Tests: unsafe `ALTER TABLE`, index build, extension update, and generated-column migration produce stable rule IDs and reviewable artifacts without executing production SQL.
- Metric: high-risk recommendations are reviewable in CI/API/UI before execution, with stable rule IDs usable by `pg_sage lint`.

### 11. Agent-Native Operations Pack

- Problem: agent-created databases need attribution and containment, not just generic query findings.
- First slice: correlate findings to `application_name`/SQL comments/client metadata; add workload fingerprints for agent memory tables, embedding writes, schema sprawl, and query-pattern explosions.
- Tests: multiple agents sharing a database produce separate cases, runaway-agent patterns map to the right actor, stale embedding/model mismatch is detected when metadata exists.
- Metric: agent-generated workload issues can be grouped by actor, workload, and case rather than only table/queryid.

## Questions You Should Be Asking

1. Who is pg_sage primarily for now: DBA, SRE, app developer, AI coding agent user, or managed-service operator?
2. What is the product promise: "advisor," "autonomous DBA," "database safety system," or "agent database guardrail"?
3. Which actions should pg_sage never do automatically, even at high trust?
4. What proof is required before a recommendation can be called successful?
5. Should pg_sage optimize for open-source local trust, SaaS fleet control, or both?
6. What data is allowed to leave the user's environment for LLM analysis?
7. Should vector recall benchmarks be built into pg_sage, or imported from user eval sets?
8. What is the minimum useful clone/branch integration for safe DDL proof?
9. Should pg_sage generate GitHub PR comments before it executes production SQL?
10. How will pg_sage price or message ROI: CPU saved, incidents prevented, DBA hours saved, or query latency improved?
11. What does trust mean for agent-created databases where no human fully understands the schema?
12. Should pg_sage maintain an agent-readable database manifest so coding agents stop guessing?
13. What is the support matrix for Supabase, Neon, RDS, Aurora, Cloud SQL, AlloyDB, Azure, and self-managed Postgres?
14. How will pg_sage handle extension unavailability without giving weak or unsafe advice?
15. What is the product's "wow" demo for v1: RCA case, autonomous index proof, vector tuner, or agent DB guard?
16. Which extensions should pg_sage treat as first-class product domains versus generic catalog facts?
17. When should JSONB tuning recommend an index, and when should it recommend schema promotion or normalization?
18. How much LLM autonomy is allowed in extension/JSON recommendations before deterministic validators must veto?
19. What is the right deployment unit: sidecar per database, fleet sidecar per cluster, or managed control plane?
20. Are cases the top-level product object, or do workloads become first-class with many cases nested underneath?
21. What is pg_sage's embedding model versioning contract for vector indexes and recall reports?
22. Is `pg_stat_statements` enough for vector and agent workloads, or does pg_sage need optional lightweight statement tracing?
23. What should pg_sage retire in v1.x so the product does not accumulate old extension-era or dashboard-era surfaces?
24. Should all high-risk DDL become deploy requests by default, even when generated by pg_sage itself?

## Research Gaps

- X/Twitter was only partially accessible through indexed public pages; most direct X content is login-gated.
- Vector benchmark claims from vendors and Reddit need local reproduction before product claims.
- Need direct code read on all v1 cases/shadow-mode files before writing implementation plans.
- Need current release docs reconciled with code; public README/CLAUDE/roadmap are behind the current route and feature surface.
- Need provider-by-provider extension capability matrix for vector and tuning features.
