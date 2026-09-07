# pg_sage v2 Research: LLM/ML-Driven Autonomous DBA — Prior Art and Defensible Differentiators

**Date:** 2026-06-10
**Scope:** Survey academic + industry prior art on LLM/ML for database administration and tuning, then isolate the LLM-native capabilities pg_sage can productize that competitors cannot easily copy. Companion to `research/llm_automation_opportunities.md` (feature roadmap) and `research/v1_hnsw_autotuning_prior_art.md` (vector-index autotuning). This document does **not** re-derive Bayesian-optimization mechanics covered in v1; it focuses on the LLM-as-reasoner frontier.

**Framing check (verified, not echoed):** The briefing claims text-to-SQL is "not our space." That is correct, and the prior art below makes the boundary sharp: text-to-SQL (DB-GPT, LlamaIndex, Spider benchmark) optimizes *human-to-data access* — translating an end-user question into a SELECT. pg_sage operates one layer down, on *database-to-database introspection* — reasoning over `pg_stat_*`, plans, and catalog to change the database's own behavior. The two share an LLM substrate and nothing else; conflating them would be a strategic error. The genuinely defensible ground, confirmed by the survey, is the narrow band where an LLM reasons over correlated *operational* telemetry and emits *grounded, validated, auditable* remediations — a band that every prior system touches partially and none owns end-to-end as an autonomous sidecar.

---

## 1. ML knob tuning: OtterTune and its lineage

### OtterTune (CMU → startup, 2017–2024)

OtterTune is the canonical ML-for-DBMS-tuning system. The [VLDB 2017 paper](https://db.cs.cmu.edu/papers/2017/p1009-van-aken.pdf) (Van Aken, Pavlo, Gordon, Zhang) used Gaussian-Process regression with Expected-Improvement acquisition to recommend knob values, plus factor analysis to prune the knob space and workload mapping to transfer learning across past tuning sessions. It demonstrated configurations matching or beating expert DBAs on MySQL and Postgres. It spun out as a startup ([CMU project page](https://db.cs.cmu.edu/projects/ottertune/)), raised a $12M Series A in 2022 ([Accel](https://www.accel.com/companies/ottertune)), and **shut down in June 2024** after a Postgres-focused PE acquisition collapsed and forced layoffs ([ottertune.com obituary](https://ottertune.com/), [Pavlo announcement](https://x.com/andy_pavlo/status/1801687420330770841)).

**What it proved:** Black-box ML tuning *works* technically — GP/Bayesian search reliably finds knob settings competitive with experts, and transfer learning materially shortens the search.

**Why it folded (the lessons that matter for pg_sage):**
1. **Knob tuning alone is too narrow to sustain a business.** Knobs (`shared_buffers`, `work_mem`, `max_wal_size`) are a one-time-ish win; once tuned, the customer has little reason to keep paying. There is no recurring surface. pg_sage's answer: knob tuning is *one of fifteen* LLM call-sites, not the product. Recurring value comes from continuous remediation (vacuum, bloat, lock chains, regressions) that re-earns its keep every week.
2. **SaaS that ingests telemetry is a hard sell to DBAs.** Sending production stats to a vendor cloud triggers security review. pg_sage's sidecar architecture (data never leaves the network) is the direct antidote — and is the single most-cited differentiator in the competitive table in `llm_automation_opportunities.md`.
3. **Pure ML gives no explanation.** A GP posterior cannot tell a DBA *why* `work_mem=64MB` beat `16MB` in terms they trust. This is precisely the gap LLMs close, and where the "LLM-native" differentiators below live.

The "Son of OtterTune" pivot (toward a proxy/OEM model) never materialized publicly; the lesson stands that the *advisory-only, cloud-SaaS, knobs-only* shape is commercially fragile.

### DBtune (active competitor)

DBtune (Malmö; Lund/Stanford research lineage) continues the GP-tuning approach as a live SaaS across AWS/Azure/GCP. Same narrow scope (knobs), same SaaS trust problem. It validates that the *technical* approach survives OtterTune's death, but inherits the same business-shape risk.

### Where v1 already covers this

`research/v1_hnsw_autotuning_prior_art.md` §2.6 correctly notes OtterTune's GP+EI is *single-objective* and therefore the wrong spine for the multi-objective (recall vs latency) HNSW problem — VDTuner's EHVI is. That critique applies narrowly to vector-index tuning; for scalar knob tuning OtterTune's GP approach remains a reasonable Tier-1 baseline that an LLM layer should *augment* (GPTuner-style), not replace.

---

## 2. LLM-guided knob tuning: the GPTuner / λ-Tune frontier

This is the most directly relevant academic cluster — and the most defensible to productize, because it fuses the LLM's document-reading with grounded optimization.

### GPTuner (VLDB 2024)

[GPTuner](https://vldb.org/pvldb/vol17/p1939-tang.pdf) (Lao, Wang, …, Tang, Wang — Sichuan Univ. + Purdue) is the key paper. It uses an LLM to *read the DBMS manual and tuning blogs*, distill heterogeneous domain knowledge into a structured view, and use that knowledge to (1) **select** which knobs matter, (2) **narrow the value range** of each knob using documented guidance (e.g., "set `effective_cache_size` to ~75% of RAM"), and (3) feed a **Coarse-to-Fine Bayesian Optimization** that searches the LLM-pruned space. Reported ~16× faster convergence and ~30% better performance than prior SOTA ([SIGMOD Record writeup](https://sigmodrecord.org/?smd_process_download=1&download_id=14014), [GitHub](https://github.com/SolidLao/GPTuner)).

**The crucial insight:** the LLM is *not* the optimizer. It is a **prior generator**. It reads the white-box knowledge (manuals, release notes, Postgres wiki) that black-box BO can't see, and hands BO a far smaller, better-centered search space. BO still does the empirical validation. This is the safe shape: **LLM proposes, deterministic loop verifies.**

### λ-Tune (SIGMOD 2025)

[λ-Tune](https://arxiv.org/abs/2411.03500) (Giannakouris, Trummer, Cornell) goes further: instead of per-knob hints it has the LLM **generate an entire configuration script** from a large "tuning context document," then uses a principled candidate-selection procedure to pick the best of a *small set* of LLM-generated configs while bounding reconfiguration/evaluation cost. It frames prompt construction itself as a *cost-based optimization problem* — convey maximal relevant context under a token budget. Tested on Postgres + MySQL, more robust than prior LLM-hint approaches ([PACMMOD](https://dl.acm.org/doi/10.1145/3709652), [code](https://github.com/gsvic/lambda-tune)).

**Lesson for pg_sage:** λ-Tune validates *holistic* config generation (the whole `postgresql.conf` delta) over per-knob nibbling, and — critically — that **bounding the number of empirical evaluations** is the dominant cost concern, because each config change requires a restart + workload replay.

### How pg_sage productizes this (concrete LLM call shape)

pg_sage already does config advising (vacuum/WAL/connection/memory). The GPTuner upgrade:

> **Input to LLM:** `{pg_version: "16.3", target_knob: "max_wal_size", current_value, hardware: {ram_gb, cpu, disk_type}, workload_summary: {write_tps, checkpoint_frequency, wal_generated_per_hour}, relevant_doc_chunks: [retrieved chunks from PG16 docs + release notes for WAL/checkpoint]}`
> **Output (strict JSON):** `{recommended_value, value_range: [lo, hi], rationale, doc_citations: [{url, quote}], confidence}`

Grounding rules to avoid hallucinated values:
- The LLM only ever emits a value *inside a range it justifies with a retrieved doc quote*. No quote → no recommendation (fall back to Tier-1 deterministic rule).
- The recommended value is then **validated against the existing deterministic advisor's bounds** before it can become an action. The LLM narrows and explains; the rules engine vetoes.
- Version-pinned retrieval: the doc chunks must come from the *exact* PG major version, because checkpoint/WAL guidance changed materially across 12→14→16. This is a defensible moat — release-note-aware tuning that a static rules engine cannot match (PG18 adds `pg_stat_io`, async I/O — guidance shifts again).

**Defensibility:** GPTuner is research code, not a product; λ-Tune likewise. Nobody ships *version-aware, doc-grounded* config tuning as an autonomous sidecar. This is differentiator #2 below.

---

## 3. LLM database diagnosis agents: D-Bot, "LLM-as-DBA", Andromeda

This cluster is the closest prior art to pg_sage's root-cause and diagnose features — and the most important to study for *grounding technique*.

### "LLM As DBA" (Zhou et al., arXiv 2023)

The [original "LLM As DBA" paper](https://arxiv.org/abs/2308.05481) (Zhou, Li et al., Tsinghua) introduced the pattern: an LLM agent that (a) ingests *experience documents* (textbooks, manuals, internal runbooks) into a structured knowledge base, (b) uses external **tools/APIs** to fetch live metrics rather than guessing them, and (c) reasons via chain/tree-of-thought to localize root causes. It established that an LLM *with tool access to real telemetry* substantially outperforms an LLM answering from parametric memory alone.

### D-Bot (VLDB 2024) — the reference architecture

[D-Bot](https://www.vldb.org/pvldb/vol17/p2514-li.pdf) (Zhou, Li, Guoliang Li et al.) is the matured system and the one pg_sage should treat as the architectural template for diagnosis:

- **Knowledge from documents:** automatically extracts diagnosis knowledge (symptoms → causes → fixes) from manuals/blogs into a retrievable store, so diagnosis is *grounded in cited expertise*, not invented.
- **Tool calling:** the LLM invokes external monitoring tools/functions to read actual metrics (`pg_stat_*`, OS stats) — it never fabricates a metric value.
- **Tree-of-Thought (ToT):** explores multiple reasoning chains, votes on the most promising, and **backtracks** when a chain dead-ends — far more robust than single-shot CoT for multi-cause incidents.
- **Multi-agent collaboration:** specialized agents (e.g., CPU agent, memory agent, query agent) analyze in parallel and reconcile via asynchronous communication.
- **Evaluation:** verified on **539 real anomalies across six applications**; significantly outperforms traditional methods *and vanilla GPT-4* at identifying root causes of unseen anomalies ([ACM](https://dl.acm.org/doi/abs/10.14778/3675034.3675043), [arXiv](https://arxiv.org/abs/2312.01454)).

**Lessons for pg_sage's RCA feature (§4.1 of the opportunities doc):**
1. **Tool-grounded metrics are non-negotiable.** The LLM must read every number from a real query result injected into context, never generate one. pg_sage already collects all signals; the RCA prompt should pass *only observed values* and forbid the model from introducing unobserved facts.
2. **ToT + backtracking beats a fixed decision tree for the long tail.** The opportunities doc proposes a decision tree (locks → vacuum → bloat → plans → config) for 80% of cases; D-Bot shows the *remaining 20%* (multi-cause, novel correlations) is exactly where LLM ToT earns its keep. pg_sage should run the deterministic tree first and escalate to an LLM ToT loop only when the tree is inconclusive — cheaper and more grounded.
3. **Multi-agent maps onto fleet mode.** D-Bot's per-subsystem agents are analogous to pg_sage running per-database analysis that a fleet-level agent reconciles.

### Andromeda (SIGMOD 2025) — RAG for config debugging

[Andromeda](https://arxiv.org/pdf/2412.07548) (also "Automatic Database Configuration Debugging using Retrieval-Augmented Language Models") answers natural-language questions about config/performance issues by **RAG over manuals, forum threads (e.g., DBA StackExchange, Postgres mailing lists), and query/metric context**, returning context-aware, *citation-backed* remediation. It is the explicit RAG counterpart to D-Bot's knowledge-extraction approach ([ACM](https://dl.acm.org/doi/10.1145/3722212.3725080), [review](https://www.themoonlight.io/en/review/automatic-database-configuration-debugging-using-retrieval-augmented-language-models)).

**Lesson:** retrieval grounding (cite the forum post / manual section that justifies the fix) is the standard technique for keeping LLM DBA advice honest. pg_sage's "explain narrative" and diagnose features should attach citations the same way — and because pg_sage runs *inside* the customer's environment, it can also retrieve from the customer's *own* prior findings/actions log, which no external academic system can do.

---

## 4. Learned query optimizers: Bao, Neo, Balsa

A parallel ML-for-DB line that pg_sage should understand but mostly *avoid* re-implementing.

- **Neo** ([Marcus et al., VLDB 2019](https://www.semanticscholar.org/paper/Neo:-A-Learned-Query-Optimizer-Marcus-Negi/5ba52bbe1101939c490a06cc0cf316a09000834e)): a deep-RL optimizer that learns to produce full physical plans, bootstrapped from the existing optimizer. Proved an end-to-end learned optimizer can match/beat traditional ones — but needs heavy training and is hard to make safe in production (a bad learned plan can be catastrophic).
- **Bao** ([Marcus et al., SIGMOD 2021](https://arxiv.org/pdf/2004.03814)): the *practical* descendant. Instead of replacing the optimizer, Bao **steers** it with per-query hints (which join/scan operators to allow), using a tree-CNN + Thompson sampling. Crucially, Bao only chooses among **plans the native optimizer already considers valid**, so it can never emit an illegal plan — and it learns online with bounded regret.
- **Balsa** ([Yang et al., SIGMOD 2022](https://arxiv.org/pdf/2201.01441)): learns a query optimizer *without* expert demonstrations via RL, for environments lacking a good optimizer.

**Why this matters for pg_sage, by analogy not by adoption:** Bao's safety principle is the single most important transferable idea in this whole survey — **the learned/LLM component must only select among options the trusted system already deems valid; it never authors raw execution artifacts.** pg_sage should apply the "Bao pattern" to query *hints* and rewrites: the LLM proposes a rewrite, but the rewrite is only accepted if (a) it parses, (b) `EXPLAIN` confirms semantic-plan compatibility, and (c) ideally a result-equivalence check passes. pg_sage already has `pg_hint_plan`-style hint management; framing it as "steer, don't replace the planner" is exactly Bao.

pg_sage should **not** build a learned optimizer (Neo/Balsa) — that's a multi-year research program with production-safety landmines and no LLM-native edge. Steering (Bao) and *narrating* plan changes (see differentiator #6) are the right altitude.

---

## 5. Learned indexes (Kraska et al.) — note and dismiss

[The Case for Learned Index Structures](https://arxiv.org/abs/1712.01208) (Kraska, Beutel, Chi, Dean, Polyzotis, SIGMOD 2018) reframed an index as a model predicting a record's position, showing learned models beating cache-optimized B-trees by up to 70% in lookup speed with order-of-magnitude memory savings on read-only data. Follow-on work (ALEX, PGM-index, updatable learned indexes) added write support. **Relevance to pg_sage: low.** Learned indexes are a *storage-engine* change requiring deep Postgres internals (or a C extension) — explicitly against pg_sage's "external sidecar, no shared_preload_libraries" architecture. Worth citing as proof that ML touches every DB layer, but it is not a productization target. pg_sage's index work stays at the *recommendation/creation* level (which B-tree/GIN/HNSW to build), where the LLM adds value, not at the index-structure level.

---

## 6. Cost-based index tuning: AutoAdmin / DTA, and the LLM-vs-DTA reality check

Microsoft's [AutoAdmin](https://www.researchgate.net/publication/220282504_AutoAdmin_Self-Tuning_Database_SystemsTechnology) lineage (Chaudhuri, Narasayya) shipped the first commercial physical-design tuner (Index Tuning Wizard, SQL Server 7.0, 1998) and matured into the **Database Tuning Advisor (DTA)**, built on the **"what-if" optimizer API** — ask the optimizer to cost a query *as if* a hypothetical index existed, without building it. Azure SQL's [Automatic Tuning](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview) productized this with a conservative safety loop: create an index, watch Query Store for ~7 days, and **auto-revert if performance regresses**. Postgres's HypoPG is the open-source what-if analog pg_sage already uses.

### The 2026 LLM-vs-DTA evaluation — the most important grounding result in this survey

A [Microsoft Research paper](https://arxiv.org/html/2603.09181) (2026) directly pitted LLM-driven index tuning against DTA. Key findings, which should shape pg_sage's index-LLM design:

- **LLMs largely do *not* hallucinate indexes.** "Most of the indexes that LLM proposes for single queries are effectively utilized by the corresponding query plans. In many cases, LLM recommends *fewer* indexes than DTA." This contradicts the naive fear that an LLM will invent garbage DDL — when prompted with the actual schema and query, it proposes plausible, used indexes.
- **But LLM output has high variance.** Across five invocations on the same multi-query workload, results ranged from "4× speedup beyond DTA" (Real-D) to "all five failed to match DTA" (Real-M). The LLM is sometimes brilliant, sometimes worse than the cost-based baseline, and *you can't tell which without measuring.*
- **The real barrier is validation cost, not hallucination.** "The cost of performance validation of recommended configurations is often significantly higher than the cost of index tuning itself, due to the overhead of index creation."

**Three concrete design rules for pg_sage from this:**
1. **Always validate LLM index recommendations through HypoPG (what-if) before any real `CREATE INDEX`.** This is the cheap validation that sidesteps the Microsoft paper's cost concern — HypoPG costs the index hypothetically, no build. The confidence-boundary tests in CLAUDE.md (optimizer must reach advisory threshold *without* HypoPG, better *with*) already encode this; the survey confirms HypoPG is load-bearing for safety.
2. **Treat LLM index variance as a feature: generate N candidates, what-if-cost all N, recommend the cheapest that clears threshold.** This is the λ-Tune candidate-selection pattern applied to indexes.
3. **The LLM's edge over DTA is multi-query/semantic reasoning** (it recommended *fewer, better-targeted* indexes). That's where pg_sage's LLM optimizer should lean — covering query *families* (the N+1 / workload view) rather than per-query greedy indexing.

---

## 7. Industry DBA automation

Concrete public engineering writeups on *LLM*-driven DBA automation at Meta/Uber/Shopify are thin (their published work skews to ML cost models and index recommenders, not LLM agents). What exists:

- **Cloud-vendor auto-tuning** (Azure Automatic Tuning above; [Google Cloud SQL Recommender / Active Assist](https://cloud.google.com) for right-sizing and index advice; Aiven AI Optimizer for index recs). All are *vendor-locked* and *narrow* (indexes or sizing), and none expose plain-English justification or autonomous multi-domain remediation.
- **ML execution-cost models for index tuning** (Microsoft patents/papers cited above): replace the optimizer's estimated cost with an ML-predicted execution cost to drive index selection. Reported as hard to train accurately across diverse workloads — a reminder that pg_sage should keep the *optimizer/HypoPG* as ground truth and use the LLM for *reasoning and explanation*, not cost prediction.
- **Semi-Automatic Index Tuning** ([Schnaitter & Polyzotis, VLDB 2011](https://arxiv.org/pdf/1004.1249)): "keep the DBA in the loop" online index tuning. The philosophical ancestor of pg_sage's trust-ramp: act, but keep a human escalation path. The trust-ramp (OBSERVATION → ADVISORY → AUTONOMOUS) is a more granular, modern version of this principle.

**Net:** the industry frontier is *narrow, vendor-locked, advisory-mostly*. The LLM-native autonomous-sidecar lane is genuinely open.

---

## 8. The grounding playbook (how to keep all of this safe)

Synthesizing the survey, every LLM call-site in pg_sage that can drive an action should obey the same five-rule contract. This is the defensible *engineering* moat — competitors can copy a prompt, not a disciplined grounding pipeline integrated with a trust ramp.

1. **Inject, never invent (D-Bot tool-grounding).** Every metric/plan/catalog fact in the prompt comes from a real query result. The system prompt forbids introducing unobserved facts. Outputs that reference an unobserved table/column/metric are rejected.
2. **Cite the source (Andromeda RAG / GPTuner manuals).** Config and diagnosis recommendations must attach a doc/forum citation (version-pinned) or a pointer to the observed signal that justifies them. No citation → demote from action to advisory finding.
3. **Validate before acting (DTA what-if / HypoPG / Bao-steer).** DDL is what-if-costed (HypoPG) before build; query rewrites must parse + `EXPLAIN`-validate + ideally result-equivalence-check; config deltas are bounded-checked against the deterministic advisor. The LLM proposes; a deterministic check vetoes.
4. **Strict-JSON + `stripToJSON` everywhere (CLAUDE.md known-failure).** Every LLM response parsed through the markdown-fence-stripping path; schema-validate before use.
5. **Trust-ramp gates execution (Semi-Automatic-Index-Tuning / Azure auto-revert).** OBSERVATION → ADVISORY → AUTONOMOUS, HIGH-risk always manual, every action logged with rollback metadata, and — borrowing Azure's loop — *auto-revert on measured regression* after autonomous changes.

---

## Top 6 LLM-native differentiators (defensibility-ranked)

Ranked by how hard each is for a competitor to copy, given pg_sage's sidecar + trust-ramp + fifteen-call-site position. "Defensible" = requires the LLM's reasoning *and* deep grounding infrastructure *and* the sidecar's data position simultaneously.

1. **Natural-language root-cause across correlated signals (D-Bot pattern, sidecar-grounded).**
   *Why defensible:* requires (a) collecting *all* signals in one place (pg_sage already does), (b) ToT reasoning that no rules engine replicates on the long tail, and (c) tool-grounded metrics. Generic AIOps lacks Postgres-specific knowledge; pganalyze lacks autonomous action + on-prem data. The combination — *correlate locks + vacuum + bloat + plan-change + recent DDL into one cited narrative, then act* — is owned by nobody. Call shape: inject observed deltas across all collectors → ToT prompt with backtracking → cited RCA + ranked remediation actions, each gated by trust ramp.

2. **Version-aware, doc-grounded config tuning (GPTuner/λ-Tune productized).**
   *Why defensible:* requires version-pinned retrieval over PG manuals + release notes and a BO/deterministic verifier behind the LLM prior. The moat is *currency* — PG18's `pg_stat_io`/async-I/O shifts WAL/checkpoint guidance; a system that reads the new release notes out-tunes a static rules engine the day the version ships. Research-only today (GPTuner, λ-Tune); no autonomous sidecar productizes it.

3. **Generate + risk-classify migrations with real table stats (no prior art combines both).**
   *Why defensible:* Squawk/dryrun do *static* DDL linting; DTA does cost-based indexes; neither produces *time-estimated, zero-downtime migration plans grounded in this database's actual row counts, write rates, and live connections*. pg_sage has the live stats. LLM generates the expand-contract plan + rollback DDL; deterministic lock-analysis vetoes unsafe steps. "This `ALTER` locks your 500M-row table ~45 min; here's the 3-step zero-downtime path" is a sentence only pg_sage can truthfully emit.

4. **Plain-English justification of every autonomous action (audit trail).**
   *Why defensible:* this is the direct answer to OtterTune's fatal "ML gives no explanation" gap and to the enterprise blocker on autonomous DBAs ("I won't let a black box ALTER my prod"). Every Tier-3 action ships with a cited, human-readable *why*, logged for audit/compliance. Trivial-sounding, but it is the trust unlock that makes AUTONOMOUS mode sellable — and it's only credible when paged to the *same grounded signals* that drove the action (no post-hoc rationalization).

5. **Cross-database fleet learning (transfer + reconciliation).**
   *Why defensible:* OtterTune's workload-mapping transfer learning, reframed for LLM + fleet mode: a fix validated on DB-A becomes grounded prior knowledge for DB-B with a similar workload fingerprint, and a fleet-level agent reconciles per-DB diagnoses (D-Bot multi-agent at fleet scale). Requires the multi-DB sidecar position pg_sage already has and competitors (single-DB SaaS, vendor-locked tuners) structurally lack. The retrieval corpus includes the customer's *own* prior findings/actions — a private knowledge base no external system can access.

6. **"Why did the plan change?" narratives (Bao-steer + explanation).**
   *Why defensible:* combines plan-history capture (deterministic) with LLM narration of *what* changed in the plan and *why it matters* ("the planner switched from index scan to seq scan because stats went stale after the bulk load; the row estimate jumped 200×"). Bao proves steering-not-replacing is the safe altitude; pg_sage adds the explanation layer DTA/Bao never had. Lower rank only because it overlaps query-explanation features competitors partially touch (pgMustard), but the *regression-narrative-with-remediation* form remains unowned.

---

## Open Questions

1. **Validation-cost wall (Microsoft DTA paper, λ-Tune).** HypoPG sidesteps it for B-tree/GIN indexes, but **HypoPG cannot what-if HNSW/IVFFlat** (confirmed in v1 §3.5) and cannot cost a *query rewrite's* runtime. For rewrites and vector indexes, every validation is a real build/execution. How much validation can pg_sage afford on a production replica vs. a sampled sidecar copy before the LLM's edge is eaten by validation overhead?

2. **LLM output variance (Microsoft DTA paper: 4× better to worse-than-baseline across 5 runs).** Single-shot LLM recommendations are unreliable. Is the N-candidate-generate-then-what-if-rank pattern (λ-Tune) worth the extra LLM cost on every index/config decision, or only for HIGH-impact ones? Where's the budget cutoff in fleet mode with per-DB token budgets?

3. **Doc-retrieval currency and trust.** Version-aware tuning needs a fresh, version-pinned corpus of PG docs + release notes + reputable forum threads. Who curates it, how is it kept current across PG minor releases, and how do we prevent retrieving *wrong-version* or low-quality forum advice? Is an embedded, signed doc bundle per PG major version the answer?

4. **Result-equivalence for query rewrites.** The Bao principle says only accept plans the optimizer deems valid — but a *rewrite* can be syntactically valid and semantically different (NULL handling, duplicate rows, collation). What's the affordable equivalence check? Full result-set diff is expensive; can we bound it (sample + checksum, or formal equivalence on a restricted SQL subset)?

5. **ToT cost vs. decision-tree coverage.** D-Bot's tree-of-thought is powerful but token-expensive. What fraction of real incidents does the deterministic decision tree actually resolve (the opportunities doc guesses 80%), and is the LLM-ToT escalation worth its cost on the remaining tail — or does a cheaper single-shot CoT with good grounding capture most of it?

6. **Auto-revert telemetry loop (Azure pattern).** Azure auto-reverts index changes that regress over a 7-day Query Store window. pg_sage's autonomous actions need the same closed loop, but 7 days is slow for, e.g., a bad `work_mem` change. What's the right per-action observation window and regression metric, and can the LLM *narrate the revert* ("I created idx_x on Monday; p99 rose 18% by Wednesday with no benefit, so I dropped it") to keep the audit trail honest?

7. **Differentiator durability as frontier models improve.** If GPT-5/Claude-class models internalize Postgres tuning knowledge, do the GPTuner-style "read the manual" and D-Bot-style "extract from docs" moats erode — leaving only the *grounding/validation/trust-ramp/sidecar-data-position* moats? Likely yes: the defensible core is the **infrastructure around the LLM** (grounding, what-if validation, trust ramp, on-prem fleet data), not the model's raw DBA knowledge. Build accordingly.
