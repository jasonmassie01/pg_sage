# pg_sage v1.x Roadmap — Strategic Addendum

> **Status:** Draft for review
> **Codex review:** Partially adopted into `research/2026-04-29-product-roadmap-review.md`.
> Treat this file as source material, not the authoritative roadmap. Accepted items:
> foundation readiness, recall-first vector work, HNSW tuning phases,
> agent-native observability/guardrails, deploy-request patterns, and rule surfaces.
> Deferred or discarded items: v1.x agent write API, autonomous primary HNSW swaps,
> uncited market/security claims as load-bearing roadmap facts, and out-of-scope
> native vector DB / NL-SQL / cache / BI-agent directions.
> **Date:** 2026-04-29
> **Author:** Claude Opus 4.7, synthesized from 5 parallel research subagents + 7 prior 2026-04-29 codex artifacts + the 2026-04-27 autonomous-DBA spec
> **Theme:** Two new theaters — **Vector Intelligence** and **Agent-Native Operation** — plus a sober foundation-risk gate before either ships.
> **Builds on (do not duplicate):**
>
> - `research/2026-04-29-product-roadmap-review.md` — top-level P0/P1 ranking (vector workload advisor, recommendation proof loop, agent database guard, sandbox benchmark lab, provider-aware ops)
> - `research/2026-04-29-vector-search.md` — pgvector knob reference and ecosystem
> - `research/2026-04-29-competitive-landscape.md` — five competitor categories
> - `research/2026-04-29-community-pain-points.md` — 10 Postgres operator pain points
> - `research/2026-04-29-repo-inventory.md` — code-grounded feature inventory
> - `specs/autonomous-dba-product-spec-2026-04-27.md` — Cases-and-Actions pivot
> - `research/v1_codebase_reality_check.md` — production-readiness audit (this batch)
> - `research/v1_vector_search_landscape.md` — operator failure modes (this batch)
> - `research/v1_hnsw_autotuning_prior_art.md` — VDTuner / FastPGT prior art + algorithm sketch (this batch)
> - `research/v1_agent_created_databases.md` — agent-DB workload patterns (this batch)
> - `research/v1_cross_db_ai_landscape.md` — Lakebase/PlanetScale/Mongo Atlas patterns (this batch)

---

## 1. Thesis Update

The 2026-04-27 autonomous-DBA spec set the right product center: pg_sage is an **operator that closes the loop** (Observe → Diagnose → Decide → Act → Verify → Remember), not a metrics console. This addendum does not contest that. It extends it.

Two shifts in the world have happened faster than the existing roadmap accounts for:

1. **Postgres became the default vector store of the AI stack** — 80% of new Neon databases and 97% of branches are now agent-created (per the agent-DB research). pgvector is everywhere; the pain is concentrated in HNSW tuning, filtered ANN, build-time OOM, and recall drift. None of pg_sage's existing rules touch this.
2. **The buyer of an autonomous DBA changed** — it is increasingly *another agent*, not a human DBA. Agents provision schemas, embed corpora, run cron jobs, and stomp each other under the same Postgres role. pg_sage's trust-ramp is well-suited to this, but its *evidence model* (rules over `pg_stat_statements` snapshots) is built for a stable workload that agent traffic does not produce.

So the v1.x thesis sharpens to:

> pg_sage is the autonomous DBA agent for *agent-shaped workloads*: Postgres clusters with vector indexes, dynamic schemas, and untrusted SQL writers. It closes the loop with proof, gates risky DDL behind deploy requests, and gives every recommendation a measurable success criterion.

---

## 2. Confirmed-by-Research Findings

The following claims are validated across multiple research artifacts and should be treated as load-bearing assumptions:

1. **No PG tool ships continuous recall@k measurement.** Pinecone, Qdrant, Milvus, Weaviate autotune *server-side* params operators don't see; they don't autotune the user-facing knobs that affect recall (m, ef_construction, ef_search, lists, probes). pg_sage's wedge is operator-facing recall measurement and tuning. (`v1_vector_search_landscape.md`)
2. **HypoPG cannot model HNSW.** Every Bayesian-optimization evaluation of HNSW params is a real index build. This is the dominant cost. FastPGT (2026) measured 98.67% of tuning cost in evaluation. (`v1_hnsw_autotuning_prior_art.md`)
3. **`CREATE INDEX CONCURRENTLY` does not parallelize HNSW.** Concurrent builds are 5–10× slower than blocking. ([pgvector#822](https://github.com/pgvector/pgvector/issues/822)) Any autonomous rebuild path must reckon with this.
4. **pg_sage's risk-tier assignment is currently retroactive** — advisors generate recommendations without assessing safety; the executor guesses risk from SQL syntax later. This is a correctness bug, not a style issue. (`v1_codebase_reality_check.md`) **It blocks safe v1.x autonomy expansion.**
5. **Databricks Lakebase + Genie Code is the strongest conceptual threat.** Predictive Optimization is default-on, and Genie Code (March 2026) is an autonomous agent for Lakeflow pipelines. If Databricks ports Genie patterns to Lakebase Postgres, pg_sage's value prop ships free to every Lakebase user. (`v1_cross_db_ai_landscape.md`)
6. **PlanetScale Deploy Requests + Mongo Atlas autopilot + Supabase Splinter are the most portable patterns to steal.** The Postgres autonomous-DBA space has nobody shipping deploy-request-style gated DDL. (`v1_cross_db_ai_landscape.md`)
7. **The codebase is not yet production-ready for unattended operation.** Reality-check rates it 3.2/5. ReAct loop missing, LLM failures silent, 11 files over the 500-line limit, testify dependency present despite CLAUDE.md prohibition. (`v1_codebase_reality_check.md`)

---

## 3. Foundation Risks — Fix Before Adding Features

The codebase reality-check surfaces tech debt and correctness gaps that compound when v1.x adds new theaters. Rank-ordered:

| # | Risk | Why it must come first | Estimated effort |
|---|---|---|---|
| F1 | **Risk tiers assigned retroactively in the executor.** Advisor returns a recommendation; executor parses the SQL and *guesses* risk after the fact. Bypasses the trust-ramp's intent. | Vector autotuner and DDL gate both produce recommendations whose risk depends on table size, lock blast radius, and rebuild duration — none derivable from SQL syntax. The risk model must be authoritative *before* this code lands. | 3–5 days |
| F2 | **LLM failures silent.** Misconfigured Gemini → advisors return `(nil, nil)` with no log. Operators see no findings, assume system healthy. | Vector autotuner depends on LLM for query-pattern classification; agent-DB guard depends on LLM for DDL safety analysis. Silent failure is unacceptable in either. | 1 day |
| F3 | **`internal/api/handlers.go` is 2105 lines** (4× the 500-line cap). The Cases UI work in progress is all routed through this file. | New endpoints for vector inventory, recall reports, deploy requests, and cases will pile on. Split now or it becomes 4000 lines and unmaintainable. | 2–3 days (mechanical split into domain-grouped handlers) |
| F4 | **Per-Query Tuner silently degrades without `pg_hint_plan`.** No startup check; advisor emits hints that nobody applies. | When v1.x adds vector query rewrites, the same anti-pattern ("emit advice that requires an extension we never validated") will recur. Fix the pattern, not just this instance. | 1 day (startup capability check + warning finding) |
| F5 | **Dead C code in `src/` + testify dependency.** 19 abandoned C files (legacy extension); testify imports despite CLAUDE.md ban; `ha/` package with zero coverage. | Each is small but they are signals the codebase doesn't enforce its own rules. Fix the policy, not just the files: golangci-lint rule for testify, CI check for file-length cap, delete `src/`. | 2 hours |

**Recommendation:** Ship a v1.0.1 patch release titled "Foundation" that addresses F1–F5 before any v1.1 feature lands. The autonomous-DBA spec's product principles (7.3 Trust Is A Product Surface, 7.4 Deterministic First) cannot be honored while F1 is unfixed.

---

## 4. New Theater A — Vector Intelligence

The autonomous-DBA spec does not mention vectors. The product-roadmap-review puts it as P0. This addendum gives it shape.

### 4.1 Why now

- pgvector 0.8.2 is stable and ubiquitous; pgvectorscale, VectorChord, Lantern, ParadeDB pg_search are all viable.
- Operator pain is concrete and citable: HNSW build OOMs ([pgvector#822](https://github.com/pgvector/pgvector/issues/822) — 19h stalls), vacuum brutality on HNSW indexes, filtered ANN underfill, recall drift on IVFFlat, JIT/work_mem interactions, indexes growing larger than the heap, dimension-change downtime.
- No PG advisor (pganalyze, Supabase, Neon) has more than basic vector awareness. Greenfield.
- Threat: if Lakebase/Genie ships vector-aware autonomy, pg_sage loses this theater.

### 4.2 Scope (in priority order)

| # | Feature | Slice | Trust mapping | Depends on |
|---|---|---|---|---|
| V1 | **Vector inventory + health rules** | Detect vector columns, indexes, dimension, build status, growth rate. New rule pack. | Read-only | F1 |
| V2 | **Recall measurement framework** | Sample N representative ANN queries from `pg_stat_statements`, brute-force on a sample (~100k rows, 500 probes) for ground truth, compute recall@k and recall drift. | Read-only | V1 |
| V3 | **Query-time autotuner (`ef_search` × `iterative_scan`)** | `SET LOCAL` per session, no rebuild. Highest value-per-effort. | Advisory → Auto | V2 |
| V4 | **Build-cost & OOM forecaster** | Predict `m × ef_construction` impact on memory and build time before issuing `CREATE INDEX`. Prevents the most common vector outage. | Advisory | V1 |
| V5 | **Hybrid plan-shape diagnostic** | Detect filter+vector queries that bypass the index or apply filter post-ANN. Recommend partial indexes / `iterative_scan` mode. | Advisory | V2 |
| V6 | **Build-time autotuner (m, ef_construction)** | Multi-objective Bayesian optimization on a staging replica; shadow-index swap. See §5 for design. | Auto only with explicit policy | V2, V3, F1 |
| V7 | **Vacuum / reindex scheduler for vector indexes** | HNSW vacuum is brutal; schedule low-traffic windows; surface as a maintenance plan. | Advisory → Auto | V1 |

### 4.3 Recall-first design principle

The dedicated vector DBs hide knobs and report QPS. pg_sage's wedge is the opposite: **report recall@k continuously and let the operator see the tradeoff.** Every vector recommendation must include a measured-or-estimated recall delta. No "this looks faster" without a recall number. This is non-negotiable; without it the autotuner is dangerous.

### 4.4 What we are NOT doing in v1.x

- Not adding pg_sage as a vector-database competitor. We do not store embeddings, do not run inference, do not host indexes.
- Not autotuning beyond pgvector and pgvectorscale in v1.x. VectorChord, Lantern, ParadeDB get inventory + health rules only.
- Not making recommendations for `halfvec` / `bit` / `sparsevec` quantization in v1.x — observe only.

---

## 5. The HNSW Autotuner — Concrete Design

This is the single most technically interesting feature in the addendum. It has direct prior art (VDTuner, FastPGT) and a concrete algorithm. See `research/v1_hnsw_autotuning_prior_art.md` for full citations.

### 5.1 Inputs

- Representative query set: top-K vector queries from `pg_stat_statements` (filtered to `<->` / `<=>` / `<#>` operators), sampled to ~200 distinct query shapes.
- Recall target: e.g. `recall@10 ≥ 0.95`.
- Latency target: e.g. `p99 ≤ 50 ms`.
- Build budget: e.g. `≤ 2 hours` and `≤ 8 GB` working memory.
- Trust level: monitor / advisory / auto (per-database).

### 5.2 Four-phase loop

**Phase 1 — Workload capture.** Snapshot the target table's vector column distribution (dim, n, distance metric), capture a query sample with `auto_explain` plan shapes, fingerprint the embedding model if detectable (column comment, generated-column expression, named extension).

**Phase 2 — Sampled brute-force ground truth.** Build the ground-truth set on a *sample* of the table (~100k rows by default; adaptive cap based on table size) using exact search. The Aumüller et al. 2017 ann-benchmarks paper validates that sampled brute force is rank-preserving across configs, which is what BO needs.

**Phase 3 — Cheap online sweep on `ef_search`.** Without rebuilding, run the query sample at `ef_search ∈ {40, 80, 160, 320, 640}` against the *existing* index. This produces a recall/latency Pareto frontier for query-time tuning. Most workloads will see a usable gain here without a rebuild — V3 in §4.2 ships from this.

**Phase 4 — Multi-objective BO over `(m, ef_construction, ef_search)` on a staging replica.** Only entered if Phase 3 cannot meet the recall+latency target. Uses VDTuner-style EHVI acquisition (Expected Hypervolume Improvement) to learn the Pareto frontier rather than a single point. Each evaluation requires a real rebuild on the staging replica — this is the bottleneck. FastPGT's batched-candidate trick (2.37× speedup) should be ported.

### 5.3 Deployment path

- **Monitor mode:** Phase 1 + 2 + 3 only; report current recall and the no-rebuild improvement available. Never modifies the index.
- **Advisory mode:** Phase 4 emits a recommendation with shadow-index SQL. Operator reviews and applies during a maintenance window.
- **Autonomous mode:** Phase 4 runs on a staging replica during a configured window, builds the shadow index on the primary via `CREATE INDEX CONCURRENTLY` (despite the parallelization gap — see §2 finding 3), validates recall ≥ target on production sample, atomic-renames to swap.

### 5.4 Honest expectation-setting

VDTuner reports 14% QPS / 186% recall improvement, but that is on un-tuned defaults. On workloads where pgvector defaults are already in the right ballpark, expect **5–30% improvement**, not 186%. The pitch must reflect this. Overpromising kills trust faster than underpromising.

### 5.5 Risks

- Sampled brute-force ground truth may underestimate tail recall on rare query shapes. Mitigation: stratified sampling by query frequency.
- Concurrent rebuild lock contention on the primary. Mitigation: a hard concurrency cap (e.g. one in-flight rebuild per database) and explicit maintenance-window check.
- Embedding model swap mid-tuning invalidates the ground truth. Mitigation: model fingerprint check before each Phase 4 entry; abort if changed since Phase 2.
- Trust-ramp gap: a 2-hour autonomous rebuild is qualitatively different from a 5-second `ANALYZE`. Risk-tier model in F1 must support "long-running with checkpoints" as a tier.

---

## 6. New Theater B — Agent-Native Operation

This is the harder strategic question. The 2026-04-27 spec's Cases-and-Actions architecture is *agent-friendly* (typed actions, identity keys, evidence) but does not ship anything *agent-aware*. The agent-DB research surfaces concrete reasons why this matters:

- 80% of Neon DBs are agent-created.
- Dolt sees 4 → 600 concurrent agents per host.
- LiteLLM CVE-2026-42208 was exploited within 36 hours of disclosure.
- MINJA: privilege-less memory poisoning that survives across agent sessions.

### 6.1 Scope

| # | Feature | Slice | Trust mapping |
|---|---|---|---|
| A1 | **Per-agent identity correlation** | Read `application_name`, statement comments, header propagation; correlate findings to the agent that issued the SQL. | Read-only |
| A2 | **DDL gate (deploy-request style)** | Intercept agent-issued DDL via log parsing, score it against schema-lint and migration-safety rules, surface a deploy-request-shaped artifact to a human (or to a higher-trust agent) before execution. Mirrors PlanetScale Deploy Requests. | Advisory (default) — Auto for SAFE-tier DDL only |
| A3 | **Workload fingerprinting** | Detect "agent-flavored" workloads: heavy embedding writes, narrow tables, JSONB blobs, high write amplification, schema sprawl. Tag the database for agent-specific rule packs. | Read-only |
| A4 | **Memory hygiene rules** | Stale embeddings (model fingerprint mismatch), orphan rows, duplicate facts, unbounded conversation logs. New rule class for agent memory tables. | Advisory |
| A5 | **Cost guardrails per agent** | Per-`application_name` token / embedding / row-write budgets, surfaced via `cases` when exceeded. | Advisory → Auto-suspend at budget breach |
| A6 | **Runaway-agent detection + isolation** | Detect agent stomping (concurrent transactions, lock contention, query-pattern explosion); rate-limit at the connection-string level. Higher-priority than the existing runaway-query feature. | Advisory → Auto-cancel at policy threshold |
| A7 | **Public dogfooding** | pg_sage operates a Postgres. Document what *it* writes to its own DB, what its own memory hygiene rules are, what its own DDL gate caught. Distribution play. | N/A (docs + blog) |

### 6.2 The strategic question this theater forces

> Should pg_sage offer a write API (`pg_sage.remember(...)`, `pg_sage.embed_and_insert(...)`) that agents call instead of issuing raw SQL?

A write API would:
- Centralize agent identity, cost accounting, schema validation.
- Eliminate prompt-injection-into-SQL as an attack surface (P2SQL).
- Give pg_sage a control plane, not just an observability plane.

But it would also:
- Make pg_sage a runtime dependency, not a sidecar (architectural shift).
- Compete with LangChain SQL toolkits and LlamaIndex SQL.
- Bind pg_sage to specific agent-side schemas (e.g. memory-store opinions).

The agent-DB research lands on "ship A1–A6 first; revisit the write-API question in 6 months when we have data on what agents do that violates the SAFE tier." This is correct. Do not ship A1–A6 *and* a write API in v1.x. Ship the observability + advisory side, learn, then decide.

---

## 7. Cross-DB Strategic Moves

From `v1_cross_db_ai_landscape.md`, the highest-leverage portable patterns:

### 7.1 Steal (high priority)

- **Deploy Requests for HIGH-risk DDL.** PlanetScale's pattern: a DDL change opens a request, pre-flight runs, a human (or higher-trust agent) reviews, the change executes within a 30-minute revert window. Maps directly onto pg_sage's existing migration-safety + cases work. This is A2 above.
- **30-minute revert window for executed actions.** Not just rollback SQL — *automatic* revert if a post-action verification fails or a regression metric trips. Strengthens the "Verify" leg of the Observe→Verify loop.
- **Open-source `pg_sage lint` CLI with stable rule IDs.** Mirror Supabase Splinter's distribution play. Anyone can run `pg_sage lint schema.sql` in CI. Rule IDs live forever and become the lingua franca.
- **Recommendation overlays on the performance timeline.** "Latency improved 40% at 14:32 because pg_sage applied recommendation R-1234." This is the cost-justification screenshot.
- **`SELECT * FROM sage.findings_summary()` SQL-native surface.** psql users see findings without leaving the terminal. Crunchy `cb psql --menu` pattern.

### 7.2 Match (table stakes)

- Impact-ranked recommendations (Mongo Atlas).
- Per-finding impact score with auto-apply threshold toggle.
- Day-1 production-check report.
- Programmatic API (already partly there; document and stabilize).

### 7.3 Don't bother

- Native vector search as a *core* feature — pg_sage manages pgvector, not its own engine.
- Cortex AISQL-style natural-language SQL — out of scope.
- Scale-to-zero compute — managed-platform concern.
- Boost-style cache layer — niche, doesn't fit the spec's non-goals.
- BI / dashboard agents — not the buyer.

---

## 8. v1.x Roadmap Slot-In

Slotting the new work into the existing release train. Does not replace the autonomous-DBA spec's planned cases/shadow-mode work — it sits alongside.

### v1.0.1 — "Foundation" (1–2 weeks)
F1 risk-tier model fix, F2 LLM logging, F3 handlers split, F4 capability checks, F5 lint policy.

### v1.1 — "Vector Intelligence Phase 1" (~6 weeks)
V1 inventory + health rules, V2 recall measurement framework, V3 `ef_search` query-time autotuner, V5 hybrid plan-shape diagnostic, V7 vacuum/reindex scheduler. **No build-time autotuner yet.** Plus 7.1 #4 (recommendation overlays on timeline).

### v1.2 — "Agent-Native Phase 1" (~6 weeks)
A1 per-agent identity, A3 workload fingerprinting, A4 memory hygiene rules, A5 cost guardrails. Plus 7.1 #1 Deploy Requests for HIGH-risk DDL (which doubles as A2 for agent DDL).

### v1.3 — "HNSW Autotuner + Agent Isolation" (~8 weeks)
V4 build-cost forecaster, V6 build-time autotuner (Phase 4 of §5), A6 runaway-agent isolation. This is the heaviest release. Plus 7.1 #2 30-minute revert window. The autotuner *requires* F1 done and risk-tier model extended for "long-running with checkpoints."

### v1.4 and beyond
- 7.1 #3 `pg_sage lint` open-source CLI.
- A7 public dogfooding (this is a content/marketing track, can run in parallel).
- Revisit the agent write-API question (§6.2) with 6 months of A1–A6 data.

### Success criteria per release

Every v1.x release ships with **measured success criteria**, not aspirational ones:

- v1.0.1: zero new CRITICAL findings in `golangci-lint`, zero files > 500 lines, every advisor has a startup capability check.
- v1.1: recall@10 reported continuously for ≥ 3 reference workloads; query-time autotuner produces ≥ 10% latency improvement on demo with no recall regression > 1pp.
- v1.2: A4 memory-hygiene rules detect ≥ 5 distinct agent-DB anti-patterns on dogfood Postgres.
- v1.3: V6 autotuner converges on a Pareto-improving config in < 2h for a 1M-row reference dataset; produces a no-op when defaults are already on the frontier.

---

## 9. Questions You Should Be Asking That You Are Not

These are the strategic questions surfaced by the research that *no current spec or roadmap document addresses*. They are not in any priority order — every one of them is decision-forcing.

1. **Who is the buyer?** The autonomous-DBA spec assumes "operator." The agent-DB research suggests it is increasingly "platform team that runs agents." These have different budgets, different procurement paths, different reference-deployment shapes. Until you decide, the pricing page and the docs both pull in two directions.

2. **What is the right deployment unit?** Sidecar-per-database (current), sidecar-per-cluster (fleet mode), or *control-plane-as-a-service*? The Lakebase/Genie threat is strongest against sidecar deployments because Lakebase ships its own. A managed pg_sage service may be defensive necessity, not optional.

3. **How does pg_sage tell the difference between "an agent did something dumb" and "an agent did something we haven't seen before"?** Workload fingerprinting (A3) detects new patterns; the harder question is whether to alert, observe, or learn. This is a policy question, not a code question, and you have not written the policy.

4. **What happens when pg_sage IS the runaway query?** When the autotuner takes 6 hours and consumes 12 GB on the staging replica because the workload changed mid-tuning, who detects it? Currently nothing. The dogfooding plan in A7 is not optional — it is a safety check on the product.

5. **Are vector recommendations ever safe to apply autonomously?** Index rebuilds are minutes-to-hours, lock contention is real, the rebuild blocks vacuum, and recall improvement is a probability not a guarantee. The current trust-ramp tiers (SAFE / MODERATE / HIGH) may not be the right vocabulary for vector actions. Do you need a new tier called "patient" or "scheduled-maintenance"?

6. **What is the ground-truth obligation?** If pg_sage recommends a change and it makes things worse, is pg_sage liable? AGPL helps but doesn't answer the trust question. Are you willing to ship "we measured recall@10 = 0.91 ± 0.02 on 200 sample queries" or is that a footgun for an enterprise sales conversation?

7. **What about embedding model versioning?** When the agent re-embeds the corpus with a new model, every existing index is wrong, recall reports become misleading, and previously-good recommendations turn bad. Detection requires a model fingerprint that is currently nowhere in the schema. Do you mandate a `sage.embedding_model` registration table, or do you fingerprint by column statistics? Both are research projects.

8. **Is `pg_stat_statements` enough?** Vector queries are often parameterized in ways that produce wildly different `queryid`s for semantically identical operations. The cross-domain landscape research notes that pg_stat_statements alone is insufficient for index ranking — Mongo Atlas combines it with Performance Advisor traces. Do you ship your own light statement-tracking layer, or accept the gap?

9. **What is the "minimum viable agent" pg_sage assumes the user is running?** Different agents emit different SQL shapes, different memory schemas, different cost profiles. Do you ship reference rule packs for Letta, Mem0, Zep, Claude memory, ChatGPT projects? Or do you make rule packs configurable and let the community publish them?

10. **What is your story on managed-Postgres pgvector limits?** Cloud SQL caps `maintenance_work_mem`. AlloyDB has its own ANN-aware extensions. Aurora ships pgvector 0.8.0 with a custom optimizer path. Each one breaks an autotuner assumption. The provider-aware-ops feature in the existing roadmap is the right answer; it must ship before V6, not after.

11. **What does pg_sage *retire*?** Every roadmap document adds. None subtract. The reality-check identifies orphan endpoints, the C extension legacy, and the unused MCP server. The autonomous-DBA spec lists Non-Goals but no removal plan. A "v1.x retirements" track is mandatory if the codebase is to stay maintainable.

12. **Should pg_sage open-source the rule definitions while keeping the executor proprietary?** Splinter's distribution play (open-source linter) drove ecosystem adoption beyond Supabase's platform. The same applies to pg_sage's deterministic Tier 1 rules. AGPL on the whole binary is the current answer; a dual-license with permissively-licensed rule definitions could 10× distribution at low cost — but only if the rules are clean enough to publish. They aren't yet (see F-risks).

13. **Do you actually need an LLM, or do you need a *constrained* LLM?** Several reality-check findings (silent LLM failures, opaque confidence, hardcoded threshold) suggest the LLM is a soft point. Vector autotuner doesn't need LLM at all — it's a BO loop. Agent-DB rules can be deterministic. Maybe v1.x is the release where pg_sage becomes "deterministic-first, LLM-augmented" instead of "deterministic + LLM." This was already principle 7.4 of the spec, but the code doesn't enforce it.

14. **What's the recall test for the recall framework?** When V2 reports `recall@10 = 0.94`, how do you know it's right? You need a meta-test: synthetic dataset with known ground truth, V2 measurement, expected ≥ 0.99 of true recall. Without this, V2 is unverifiable infrastructure underneath every vector recommendation.

15. **Is "Cases" the right unit, or is "Workloads" the right unit?** The autonomous-DBA spec centers cases (incident-driven). The agent-DB research suggests workloads (per-agent, per-app, per-tenant) are the unit operators care about. They are not the same thing — a vector workload generates many cases over time. Do they nest? Are workloads first-class? The spec doesn't say.

---

## Appendix — Files written by this exercise

- `research/v1_codebase_reality_check.md` (~2,200 words; 3.2/5 audit, F1–F5)
- `research/v1_vector_search_landscape.md` (~3,500 words; ecosystem + 8 failure modes + Top 5 features)
- `research/v1_hnsw_autotuning_prior_art.md` (~3,500 words; VDTuner + FastPGT + 4-phase algorithm)
- `research/v1_agent_created_databases.md` (~3,400 words; 5 agent-DB definitions + 10 features + Top 5 bets)
- `research/v1_cross_db_ai_landscape.md` (~4,800 words; 10 targets + table stakes / differentiators / don't-bother)
- This addendum: `docs/ROADMAP_v1.x_addendum.md`

A reusable Claude Code skill for this exercise lives at `~/.claude/skills/roadmap-research/SKILL.md` (sister to the existing `~/.agents/skills/researching-pg-sage-roadmap/` used by Codex).
