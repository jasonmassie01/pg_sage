# pg_sage Roadmap Skill Forward Test

Date: 2026-04-29  
Mode: miniature forward-test of `researching-pg-sage-roadmap`  
Scope: review pg_sage enough to propose next additions, including vector search
and agent-created databases. No source code was modified.

## Workflow Followed

1. Loaded the requested skill from
   `C:/Users/jmass/.agents/skills/researching-pg-sage-roadmap/SKILL.md`.
2. Checked local task context. `tasks/lessons.md` was absent; appended a small
   checklist to the local Codex `tasks/todo.md`.
3. Inspected the pg_sage repository state:
   - Branch: `master`
   - HEAD: `113acbd04a41`
   - Describe: `v1-1-g113acbd`
   - Dirty/untracked research and test artifacts already existed, including
     `research/2026-04-29-*.md`, `.gocache/`, `test-results/`, and
     `sidecar/coverage-target.out`.
   - I did not fetch or pull because the request did not ask for latest release
     context and this was a non-mutating forward-test.
4. Read local product evidence: `README.md`, `CLAUDE.md`, `roadmap.md`,
   `CHANGELOG.md`, `docs/`, `research/`, `sidecar/internal/`, and
   `sidecar/web/src/`.
5. Searched local code/docs for vector, HNSW, pgvector, pgvectorscale, branch,
   DBLab, clone, sandbox, and agent-created-database hooks. Result: vector and
   branch/database-lab ideas exist in research, but not as shipped product
   capabilities.
6. Checked targeted primary/current sources:
   - pgvector README: HNSW/IVFFlat, `m`, `ef_construction`, `ef_search`,
     `SET LOCAL`, filtering, partial indexes, partitioning, iterative scans:
     https://github.com/pgvector/pgvector
   - pgvectorscale README: StreamingDiskANN, label-based filtered search, query
     vs build-time tuning: https://github.com/timescale/pgvectorscale
   - Supabase HNSW docs: operator classes, `halfvec`, filtering behavior:
     https://supabase.com/docs/guides/ai/vector-indexes/hnsw-indexes
   - Cloud SQL Index Advisor docs: managed advisor scope and limitation to
     `CREATE INDEX` recommendations:
     https://docs.cloud.google.com/sql/docs/postgres/use-index-advisor
   - Postgres.ai DBLab README: thin cloning, automation API/CLI, pgvector and
     HypoPG support, quotas and clone lifecycle:
     https://github.com/postgres-ai/database-lab-engine
   - Neon branching docs: copy-on-write branches, isolated testing, TTL branches,
     AI-driven development workflows:
     https://neon.com/docs/introduction/branching
   - BranchBench paper: branchable databases for agentic workloads and current
     tradeoffs: https://arxiv.org/abs/2604.17180

## Sections I Would Produce In A Full Report

1. Current pg_sage capabilities by workflow: collect, analyze, advise, execute,
   verify, rollback, explain, alert, fleet, UI/API, security, tests.
2. Documentation/code drift: what README, roadmap, changelog, docs, research,
   code, and tests disagree on.
3. User pain synthesis from PostgreSQL communities and issue trackers.
4. Competitive map: pganalyze, pgMustard, PoWA, Cloud SQL Index Advisor,
   Supabase/Neon advisors, DBLab/Neon branching, Oracle/Azure autonomous tuning,
   and general APM tools.
5. Vector-search deep dive: pgvector, HNSW, IVFFlat, pgvectorscale, filtering,
   recall/latency benchmarks, memory/build cost, hosted-provider constraints,
   safe `SET LOCAL` tuning, and rebuild safety.
6. Agent-created databases: disposable lab databases, branch targets, generated
   schemas, seed data, vector corpora, workload synthesis, cost caps, teardown,
   audit logs, and production boundaries.
7. Ranked roadmap: user pain, differentiation, feasibility, safety risk,
   evidence strength, first shippable slice, tests, and open questions.

## Three Recommendations

| Rank | Recommendation | Why | First shippable slice |
|---|---|---|---|
| 1 | Add a pgvector/ANN Advisor | pg_sage has mature Postgres DBA scaffolding but no shipped vector advisor. pgvector users need help with HNSW/IVFFlat choice, filter selectivity, recall loss, build memory, and pooled-session tuning. | Read-only Vector Cases: inventory `vector` extension/version, vector columns, ANN indexes, operator classes, query shapes, filters, `LIMIT`, and whether the plan uses ANN. Emit explainable cases only. |
| 2 | Add a Disposable Lab Database Runner | Agent-created databases should start as isolated lab/branch databases, not production automation. DBLab and Neon prove that fast clones/branches are practical substrates. | A `lab` action target that can use an existing disposable connection or branch/clone adapter, run bounded SQL/workload replays, capture evidence, and tear down with TTL/cost/audit controls. |
| 3 | Add Closed-Loop Proof And Retirement | Competitors advise; pg_sage can differentiate by proving, revalidating, or retiring advice across production metrics and lab evidence. This also makes vector and agent-created DB work safer. | For any index/vector/query recommendation, store baseline, hypothesis, lab/prod verification SQL, observed result, confidence change, and retirement reason when evidence expires or regresses. |

### Recommendation 1: pgvector/ANN Advisor

Build a read-only advisor first. It should detect vector extension version,
vector columns, index type (`hnsw`, `ivfflat`, `diskann` where available),
operator-class mismatches, missing filter indexes, post-filter underfill risk,
and query-time tuning candidates. It should recommend safe transaction-scoped
experiments such as `SET LOCAL hnsw.ef_search`, `hnsw.iterative_scan`,
`hnsw.max_scan_tuples`, `hnsw.scan_mem_multiplier`, `ivfflat.probes`, and
`ivfflat.max_probes`. It should not recommend rebuilds without recall ground
truth, memory/build estimates, maintenance-window analysis, and rollback or
shadow-index planning.

Test slice: SQL inventory fixtures, EXPLAIN parsing for vector operators,
filter-selectivity boundary tests, pooled-connection `SET LOCAL` tests, and a
recall benchmark harness with exact search as ground truth.

### Recommendation 2: Disposable Lab Database Runner

Treat agent-created databases as disposable proof environments. The safe product
shape is "create or target a lab database, mutate freely inside that boundary,
measure, then destroy or expire it." Useful workloads include generated schemas,
seed data, sampled production-like data where allowed, synthetic vector corpora,
migration branches, and top-query replays from `pg_stat_statements`.

Required guardrails: explicit lab-only target, no production credentials, cost
cap, TTL, teardown verification, audit log, data-egress flag, seed provenance,
provider limit awareness, and no autonomous promotion to production.

Test slice: fake adapter contract, teardown-on-error, TTL expiry, cost-cap
rejection, blocked production URL patterns, audit-event assertions, and workload
replay result capture.

### Recommendation 3: Closed-Loop Proof And Retirement

pg_sage already has cases, executor state, trust ramps, shadow mode, rollback
metadata, and provider adapters. The next product step is to make every
recommendation age, prove itself, or disappear. That is especially important for
vector tuning because higher recall usually costs latency/memory, and for
agent-created databases because lab results must be tied back to production
evidence instead of becoming untrusted side experiments.

Test slice: recommendation lifecycle tests for baseline -> proposed -> lab
verified -> production verified -> stale/retired; failure cases for missing
baseline, changed schema, changed extension version, and provider advisor
conflicts.

## Skill Sufficiency Notes

The skill instructions were sufficient for the miniature workflow. They clearly
forced the right distinctions: current repo capability vs planned ideas, public
demand vs competitor pressure, vector recall/latency proof before rebuild advice,
and strict production boundaries for agent-created databases.

Ambiguities I noticed:

- "Fetch/pull the repo when the user asks for latest/current release context" is
  clear for full reports, but this miniature request did not say latest. I chose
  non-mutating inspection and recorded that choice.
- "Use `references/report-template.md` when producing a full report" does not
  say whether a miniature forward-test should use the template. I did not use it;
  I only listed the sections a full report would produce.
- "End with questions the user is not asking" is directionally useful but vague
  on quantity. For a full report I would include 5-8 open questions; for this
  artifact I kept them implicit in the recommendation guardrails.
