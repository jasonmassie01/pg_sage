# pg_sage v2 Competitive Landscape — The Autonomy Axis

> Research date: 2026-06-10
> Lens: For each tool — what does it automate, and is it **observe-only**, **advise-only**, or **auto-apply**?
> Where is the autonomous frontier, and what white space can pg_sage own?
> Supplements (does not duplicate): [`competitive_landscape.md`](./competitive_landscape.md),
> [`v010_competitive_analysis.md`](./v010_competitive_analysis.md),
> [`2026-04-29-competitive-landscape.md`](./2026-04-29-competitive-landscape.md).
> pg_sage baseline (per [REVERSE_SPEC](../docs/REVERSE_SPEC.md)): external Go sidecar, no extension,
> deterministic rules + optional LLM, trust-gated SQL-whitelisted executor with rollback metadata.

---

## The Autonomy Axis, defined

The prior three research docs catalog *features*. This one re-cuts the field on a single dimension —
**how far down the loop a tool will go without a human** — because that, not "does it recommend indexes,"
is where pg_sage is actually different. Four rungs:

- **Observe** — collects metrics, shows dashboards, alerts. Never reasons about a fix. (pgwatch, pgBadger, Datadog)
- **Advise** — diagnoses and recommends a specific change (CREATE INDEX DDL, a config value, a query rewrite),
  but a human runs it. The DDL is copy-paste or a PR. (pganalyze, PgHero, PoWA, Aiven, Supabase, Neon online_advisor,
  Crunchy Insights, EDB advisors, Xata Agent, pgEdge)
- **Auto-apply (narrow ops scope)** — executes autonomously, but only inside a defensive envelope:
  failover, backup, infra elasticity. Never touches performance/schema. (Patroni, pg_auto_failover, pgBackRest,
  Neon autoscale, EDB storage autoscaling)
- **Auto-apply (tuning/schema scope)** — autonomously executes the risky stuff: creates indexes, mutates config,
  runs VACUUM/REINDEX, alters schema, **and validates + rolls back**. This is the contested frontier.
  Proprietary/locked-in incumbents live here (Oracle, Azure SQL); in open/portable Postgres it is **nearly empty**.
  pg_sage and (aspirationally) Postgres.ai's "Self-Driving Postgres" are the only credible entrants.

**The single most important finding of this scan:** across roughly twenty PostgreSQL tools, the AI-DBA cohort
has converged on **advise-only**, and the entire managed-Postgres market pivoted in 2025-2026 toward
*Postgres-as-substrate-for-AI* (vectors, MCP servers, agent forking) and **away from** *AI-that-operates-Postgres*.
The auto-apply-for-tuning rung in portable Postgres is white space pg_sage already occupies.

---

## 1. Deep Postgres advisors — all ADVISE-ONLY, by design

### pganalyze — advise-only, *philosophically committed* to it

pganalyze remains the strongest Postgres-specific recommender and the most important competitor to benchmark
against, but it has **deliberately not crossed into auto-apply** — and said so loudly. Its **Index Advisor** and
**VACUUM Advisor** observe collected stats and recommend; neither executes DDL or writes config
([VACUUM Advisor docs](https://pganalyze.com/docs/vacuum-advisor)). The **Indexing Engine** runs hundreds of
hypothetical index combinations entirely inside the pganalyze app (no production extension), balancing query gain
against write overhead via a "Good Enough" algorithm, then surfaces a set — it does not build them
([Indexing Engine](https://pganalyze.com/postgres-indexing-engine)).

This is a stance, not a gap. In **"The Dilemma of the 'AI DBA'"**, pganalyze argues an LLM "cannot be held
accountable," high-risk actions "need approvals," and the goal is to "enable engineers and DBAs to own
responsibility" rather than hand production credentials to an autonomous agent
([AI DBA Dilemma](https://pganalyze.com/blog/the-ai-dba-dilemma)). They reiterate that indexing should be
"AI-assisted, but developer-driven, not a problem left to an AI system to do completely on its own"
([balanced approach](https://pganalyze.com/blog/automatic-postgres-indexing-balanced-approach)).

**2025-2026 updates:** the Index Advisor became **cluster-aware in January 2026** (recommendations across a
primary + replicas) ([cluster-aware](https://pganalyze.com/blog/cluster-aware-index-advisor)), and a
**pganalyze MCP Server** hit public preview (April 2026) so agents (Claude Code, Codex, Cursor) can query metrics,
inspect EXPLAIN plans, and run the Index Advisor — **with no direct DB access** and a human/agent still in the loop
([MCP preview](https://pganalyze.com/blog/mcp-server-public-preview)). **Pricing:** Cloud Production $149/mo
(1 server); Cloud Scale $349/mo (4 servers, +$84/extra); Enterprise custom ([pricing](https://pganalyze.com/pricing)).

**The gap pg_sage exploits:** pganalyze has publicly *ceded the auto-apply rung*. Their own framing — accountability,
approvals, rollback — is precisely the problem pg_sage's trust ramp + SQL whitelist + rollback metadata + emergency
stop is built to solve. pg_sage should not out-recommend pganalyze on index quality (they will win that); it should
own the rung past the recommendation: validate → apply → verify → revert. The MCP server also signals pg_sage should
ship a first-class MCP/agent surface (per the project's own web-UI-primary direction, as a complement not the core).

### PgHero, PoWA, pgMustard — advise-only, extension-dependent

- **PgHero** (ankane, OSS): index suggestions from `pg_stat_statements` — parses queries with `pg_query`, picks
  equality/IN/null/ORDER BY candidates, sorts by cardinality. **Displays suggestions; never auto-creates**
  ([Suggested-Indexes](https://github.com/ankane/pghero/blob/master/guides/Suggested-Indexes.md)). Sets the
  "easy to run, instantly useful" bar pg_sage should match.
- **PoWA**: the most extension-heavy — `pg_stat_statements` mandatory, `pg_qualstats` **required** for missing-index
  detection, HypoPG to confirm the planner would use a hypothetical index before you build it ([PoWA](https://powa.readthedocs.io/en/latest/)).
  Advise-only, and the in-DB extension footprint is exactly the deployment tax pg_sage's no-extension posture avoids.
- **pgMustard** (~€95/yr): single-plan EXPLAIN review, advise-only, manual paste workflow (covered prior; unchanged).

---

## 2. The OtterTune lesson — auto-apply, but DEAD (verified)

OtterTune **shut down June 14, 2024** — confirmed by co-founder Andy Pavlo the same day ("officially dead… we let
everyone go today") ([Pavlo on X](https://x.com/andy_pavlo/status/1801687420330770841); [ottertune.com](https://ottertune.com/)).
It was the CMU commercial spinout (Pavlo, Van Aken, Zhang) of the open-source `cmu-db/ottertune` project. What it
automated: **ML config-knob tuning** (transfer learning across deployments) plus index recommendations as a managed
service for RDS/Aurora. It genuinely **applied** tuning — so it lived on the auto-apply rung — but for **config only**.

**Why it died (the load-bearing lessons):** a PostgreSQL-focused PE firm **backed out of an acquisition** at the last
moment with no fallback; underneath that, **weak retention/stickiness** (users tried it, didn't depend on it) and
**competition from bundled cloud-provider tooling** (AWS "default loyalty")
([HN thread](https://news.ycombinator.com/item?id=40614634); [deferas review](https://deferas.com/tool/ottertune-review-rise-fall-20202024)).

**Verified status — dead, not acquired.** No company resurfaced. But the **open-source `cmu-db/ottertune` repo
(Apache 2.0) remains live**, and the CMU research thread continues (Pavlo et al.'s 2025 SIGMOD "Database Gym" on
automated vs. human tuning) ([Pavlo CV 2025](https://www.cs.cmu.edu/~pavlo/info/pavlo-cv2025.pdf)).

**Lessons for pg_sage (an ML/LLM-tuning post-mortem):**
1. **Config tuning alone is not a product.** OtterTune and (still-living) DBtune both bet the company on knob tuning;
   OtterTune proves the scope is too narrow to retain. pg_sage's breadth (index + vacuum + bloat + query + schema +
   config, all in one binary) is the correct hedge — config is *one finding category*, not the product.
2. **Retention = ongoing value.** One-time tuning has no recurring hook. pg_sage's continuous monitor + auto-remediate
   loop is inherently sticky in a way a tuning run is not.
3. **The cloud-bundling threat is real.** AWS/Azure/GCP bundle "good enough" advisors for free. pg_sage's answer is
   the rung they *won't* ship portably (auto-apply with transparency) plus provider-neutrality across all of them.
4. **The funding-runs-out failure mode** is avoided by the open-source/AGPL model — there is no acquisition cliff.

**DBtune** (the live successor in this niche): Stanford spin-off (Luigi Nardi), Bayesian-optimization config tuner,
shipped at PGConf.EU 2025 and now on the **AWS Marketplace** for RDS PostgreSQL ([AWS Marketplace](https://www.dbtune.com/blog/now-available-on-the-aws-marketplace-ai-powered-performance-tuning-for-your-amazon-rds-for-postgresql)).
It runs a **closed loop** (~30 iterations) and **applies** configs — including a Reload-Only mode that applies most
changes without restart and respects Patroni restart flags — so it is genuinely **auto-apply (config scope)**, the
only live auto-executing tool among the advisors ([opensource-db profile](https://opensource-db.com/from-manual-tuning-to-ai-intelligence-how-dbtune-is-revolutionizing-postgresql-performance-across-the-cloud/)).
Funding: €2.4M seed (42Cap, 2023). Same narrow-scope risk that killed OtterTune; pg_sage subsumes its capability.

---

## 3. Postgres.ai / Database Lab — the "verify before apply" substrate, and the only explicit autonomy peer

This is the most strategically relevant competitor for pg_sage's *safety* story. **Database Lab Engine (DBLab)** does
copy-on-write **thin clones / branching** via ZFS (default) or LVM — a 1 TiB database clones in ~10 seconds, dozens
of independent clones on one host — Apache-2.0, latest v4.1.3 (May 2026)
([database-lab-engine](https://github.com/postgres-ai/database-lab-engine)). The productized workflow *is*
"verify a change on a realistic thin clone before prod," which is exactly pg_sage's missing "benchmark before apply"
gate.

**Autonomy story (2025-2026):** Postgres.ai explicitly rebranded around **"Self-Driving Postgres,"** adapting the
SAE J3016 self-driving-car levels (0=manual → 5=full autonomy) to DB ops. Their own assessment: most areas sit at
**Levels 0-1 (advisory)** today — RCA, cost optimization, schema changes, bloat are "least mature" — with a stated
*goal* of reaching Levels 3-4 ([Self-Driving Postgres](https://postgres.ai/blog/20250725-self-driving-postgres)).
So the platform is **mostly advise-only**: it posts recommendations as **GitHub PR / GitLab MR comments** with
Cursor integration, and the legacy **Joe bot** optimizes SQL on a thin clone. The **one genuine auto-apply sliver**
is `pg_index_pilot` ("fully automated reindexing" for bloat) plus automated zero-downtime major upgrades (run at
GitLab/Supabase scale). **Pricing:** Hobby free; Express $16/cluster/mo; Starter $128; Scale $512; Enterprise custom;
DBLab SE add-on from $62/mo ([pricing](https://postgres.ai/pricing)).

**Gap pg_sage exploits:** Postgres.ai is the *closest philosophical competitor* — same autonomy-axis framing, same
ambition — but (a) their autonomy is mostly *aspirational* (Levels 0-1 shipped), (b) auto-apply is limited to
reindexing/upgrades, and (c) DBLab requires host-level ZFS/LVM infrastructure. pg_sage already ships the trust-gated
*executor* they are working toward, over-the-wire with no host access. **The synthesis move:** adopt DBLab (or Neon
branches) as an optional **verification adapter** — pg_sage proposes → benchmarks on a thin clone → applies via the
trust-ramped executor → verifies → reverts. That closes the loop *more completely than either tool does alone* and
neutralizes the "you let an AI touch prod without testing" objection.

---

## 4. Managed/cloud Postgres — advise-only, and pivoting to Postgres-FOR-AI

The whole managed market shifted its AI energy toward making Postgres a good *substrate for AI apps*, not toward
*AI that runs the DB*. None auto-execute DBA tuning by default.

- **Crunchy Data / Crunchy Bridge — advise-only; acquired by Snowflake.** Verified: Snowflake acquired Crunchy Data
  (announced June 2, 2025, ~$250M reported) to build **"Snowflake Postgres"**
  ([Snowflake](https://www.snowflake.com/en/blog/snowflake-postgres-enterprise-ai-database/);
  [TechCrunch](https://techcrunch.com/2025/06/02/snowflake-to-acquire-database-startup-crunchy-data/)). Bridge's
  **Database Insights** surface KPIs (cache-hit, slow queries, index usage, locks) with **recommendations a human
  applies** ([Insights docs](https://docs.crunchybridge.com/insights-metrics)). No auto-execution. **Lock-in risk
  rising** as the standalone story folds into Snowflake.

- **EDB Postgres AI — advise-only; branding is Postgres-FOR-AI.** The Q1 2026 release centers on the "Agentic Era":
  VectorChord vector search, an **Agent Studio** (Langflow + MCP), GPU analytics — i.e., AI *workloads on* Postgres
  ([Q1 2026 highlights](https://www.enterprisedb.com/blog/edb-postgres-ai-q1-2026-release-highlights)). The DBA tooling
  (**Query Advisor, Postgres Tuner, Wait States**, plus an NL chatbot that *recommends*) stays advisory; the only true
  autonomy is **storage autoscaling** (infra, not tuning) ([products](https://www.enterprisedb.com/products/edb-postgres-ai)).
  Enterprise pricing, per-vCPU metered.

- **Aiven AI Database Optimizer — advise-only (explicitly).** Powered by EverSQL; produces query rewrites + index
  add/remove suggestions, and the docs state users "apply the suggestion by running the provided SQL queries" — a
  separate user-initiated step ([AI insights docs](https://aiven.io/docs/products/mysql/howto/ai-insights);
  [launch](https://aiven.io/blog/aiven-ai-dboptimizer-launch)). A standalone web optimizer works outside Aiven-managed
  DBs, so it is not strictly platform-locked.

- **Supabase — advise-only; auto-apply only if *you* wire an agent.** The OSS `index_advisor` extension returns
  CREATE INDEX DDL ([index_advisor](https://supabase.com/docs/guides/database/extensions/index_advisor)); the AI
  Assistant runs "database advisors" for security/performance. Supabase *encourages agents* to modify schema directly
  in dev via the `execute_sql` MCP tool ([agent skills](https://supabase.com/blog/supabase-agent-skills)) — so the
  platform provides rails, not a self-driving DBA. Low lock-in (OSS, self-hostable).

- **Timescale → Tiger Data — Postgres-FOR-AI, no DBA autonomy.** Rebranded to **Tiger Data** (2025)
  ([Tiger blog](https://www.tigerdata.com/blog/timescale-becomes-tigerdata)). **pgai was archived (read-only)
  Feb 26, 2026** ([repo](https://github.com/timescale/pgai)) — confirmed. **Agentic Postgres** (Dec 2025) gives *agents*
  zero-copy forks, an MCP server, and BM25 + vector search ([Agentic Postgres](https://www.tigerdata.com/agentic-postgres));
  "agentic" means agents *consume* the DB, not that it tunes itself. No auto-index/vacuum/config.

**Cross-cutting gap pg_sage exploits:** every managed vendor is provider-bound, advise-only on DBA work, and busy
selling vector/agent infrastructure. pg_sage's wedge is the **neutral reconciler + executor**: ingest each provider's
advisor output (Cloud SQL Index Advisor views, Supabase `index_advisor`, Neon `online_advisor`, EDB/Crunchy insights),
dedupe against its own findings, then **apply the survivors under one consistent trust policy** across RDS, Aurora,
Cloud SQL, AlloyDB, Neon, Supabase, and self-managed — something no single-provider tool can do.

---

## 5. Neon — auto-apply, but only at the INFRA layer

Databricks' Neon acquisition is **closed** (announced May 14, 2025; **closed July 31, 2025**) for ~$1B
([Databricks](https://www.databricks.com/company/newsroom/press-releases/databricks-agrees-acquire-neon-help-developers-deliver-ai-systems);
[TechTarget](https://www.techtarget.com/searchdatamanagement/news/366623864/Databricks-adds-Postgres-database-with-1B-Neon-acquisition)).
Neon's autonomy **splits cleanly by scope**: the *infra* layer is genuinely **auto-apply** (autoscaling,
scale-to-zero, autonomous storage), while DBA/tuning is **advise-only** via the `online_advisor` extension, which
surfaces missing-index/stats recommendations and explicitly does **not** auto-create. The widely-cited
"80% of databases created by AI agents" has grown — the latest Databricks figure is **97% of database *branches***
created by agents ([SaaStr](https://www.saastr.com/databricks-only-19-of-organizations-have-deployed-ai-agents-but-theyre-already-creating-97-of-databases/)).
Pricing was cut aggressively post-acquisition (free tier 100 CU-hrs/mo; storage ~$1.75 → $0.35/GB-mo).

**Lesson:** Neon proves auto-apply is *accepted* when scoped to reversible infra elasticity. pg_sage targets a
riskier domain (schema/perf mutation), which is exactly *why* the trust ramp matters — and the "97% of DBs are agent-
created" trend is the tailwind: a fleet of agent-spawned databases nobody is babysitting is the ideal pg_sage customer.

---

## 6. HA / ops automation — auto-apply, but narrow defensive scope (the useful contrast)

These all **auto-execute without a human** — but only to *keep the database up and recoverable*, never to tune it.
They define the boundary pg_sage extends past.

- **Patroni** (OSS, actively maintained): full auto-failover — health monitoring, automatic primary promotion, leader
  election via etcd/Consul/ZooKeeper/Kubernetes, split-brain prevention through leader leases, **no human in the loop**
  ([patroni/patroni](https://github.com/patroni/patroni)). Touches availability, never indexes/queries/schema.
- **pg_auto_failover — CORRECTION: NOT deprecated.** A premise in earlier framing is wrong. Microsoft handed it to the
  community **`hapostgres`** org, where it is **actively maintained**: v2.2 (April 2025), supports Postgres 13-18
  ([hapostgres/pg_auto_failover](https://github.com/hapostgres/pg_auto_failover)). The quiet Microsoft-org repo seeded
  the "deprecated" myth. Auto-apply, HA scope only.
- **pgBackRest** (OSS, v2.58): automated full/diff/incremental backups, async parallel WAL, PITR, delta restore,
  S3/Azure/GCS, encryption ([pgbackrest.org](https://pgbackrest.org/)). Auto-executes backups on schedule;
  restores operator-triggered. Data-protection scope, no tuning.

**The contrast that defines pg_sage:** the industry *already trusts* autonomous execution — for failover and backup.
pg_sage's bet is that the same acceptance extends to performance/schema *if* the safety scaffolding (trust ramp,
whitelist, rollback, emergency stop, validation window) is as rigorous as Patroni's leader-lease discipline. The HA
tools are the proof that "auto-apply is fine when the safety model is sound," transposed to a harder domain.

---

## 7. LLM-for-Postgres startups / new 2025-2026 entrants — all advise-only

- **Xata** pivoted to "Postgres at scale," anchored by OSS **pgroll** (zero-downtime reversible migrations) and
  **pgstream** ([xata.io](https://xata.io/blog/xata-postgres-with-data-branching-and-pii-anonymization)). **Xata Agent**
  (Apache-2.0, [xataio/agent](https://github.com/xataio/agent)) is explicitly **advise-only and read-only** — it "will
  never run destructive (even potentially destructive) commands," uses preset SQL, and *suggests* index/vacuum/config
  fixes. Their ["From DBA to DB Agent"](https://xata.io/blog/dba-to-db-agent) post is the cleanest articulation of the
  advise-only AI-DBA thesis — and a direct admission of the rung they won't take.
- **pgEdge** (distributed active-active Postgres): a free **Agentic AI Toolkit** built around an MCP server for schema
  reasoning/SQL generation ([pgEdge](https://www.pgedge.com/products/agentic-ai-postgres)). Observe/advise; no
  documented auto-apply.
- **New entrants are overwhelmingly *Postgres-for-agents*, not *agents-for-Postgres*:** Tiger Data Agentic Postgres
  (forking + MCP), Supabase AI Assistant v2 (advise). The only explicitly autonomy-framed open entrant is
  **Postgres.ai "Self-Driving Postgres"** (§3) — and it is at Levels 0-1, steering toward auto-apply but not shipped.

**Finding:** in the genuinely-new 2025-2026 AI-DBA cohort, *every product that touches DBA tuning stops at advise.*
Xata even codifies "never run destructive commands" as a principle. pg_sage's auto-apply-with-rollback is, as of
June 2026, **unmatched in portable/open Postgres**.

---

## 8. The only auto-apply-for-tuning incumbents are proprietary & locked-in

For completeness, the closed-loop *blueprint* exists — just not portably:

- **Oracle Autonomous Database (26ai)**: automatic indexing builds candidate indexes invisibly, validates them against
  an automatic SQL tuning set, marks them visible only if SQL improves, and creates plan baselines for any regression —
  a full closed loop, "without human intervention" ([Oracle features](https://www.oracle.com/autonomous-database/features/);
  [auto-indexing](https://docs.oracle.com/en/cloud/paas/autonomous-database/serverless/adbsb/autonomous-auto-index.html)).
  Oracle-only, black-box, Exadata-priced.
- **Azure SQL Automatic Tuning**: applies CREATE INDEX / DROP INDEX / FORCE_LAST_GOOD_PLAN, **validates positive gain
  and auto-reverts** on regression (validation 30 min–72 hrs); DROP_INDEX conservative by default
  ([Azure docs](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql)).
  SQL-Server-only.

These two are pg_sage's *design blueprint* (validate + rollback + conservative drop defaults) and its *opportunity*:
they prove enterprises accept auto-tuning when validation/rollback are explicit — but **neither runs on portable
Postgres**, and both are opaque. pg_sage is the transparent, provider-neutral Postgres equivalent.

---

## Table-stakes vs differentiator vs don't-bother

| Capability | Verdict | Why |
|---|---|---|
| Monitoring dashboards, slow-query/wait views | **Table-stakes** | pgwatch/PgHero/Datadog/every cloud have it; necessary to be credible, not a wedge |
| Index recommendations (missing/unused/dup) | **Table-stakes** | pganalyze, PgHero, PoWA, Supabase, Neon, Cloud SQL all advise; expected baseline |
| HypoPG-style "what-if" / hypothetical validation | **Table-stakes** | PoWA + HypoPG, pganalyze Indexing Engine; needed for credible recs |
| VACUUM/autovacuum advice | **Table-stakes** | pganalyze VACUUM Advisor sets the bar for *advice* |
| MCP / agent-facing surface | **Table-stakes (emerging)** | pganalyze, EDB, pgEdge, Tiger, Supabase all shipped one in 2025-2026 |
| Config-knob tuning | **Don't-bother as a standalone wedge** | OtterTune died on it; DBtune is narrow; keep it as *one finding category* only |
| Being another EXPLAIN visualizer | **Don't-bother** | pgMustard owns single-plan; fold plan insight into actions instead |
| Becoming a general APM | **Don't-bother** | Datadog/New Relic own it; integrate (deploy markers, traces), don't clone |
| Postgres-FOR-AI (vector/RAG/agent forking) | **Don't-bother** | EDB/Tiger/Supabase/Neon saturate it; it is a *different market* from AI-for-DBA |
| Thin-clone / branching infrastructure | **Don't-bother (build the adapter, not the engine)** | DBLab + Neon exist; *consume* them as a verification gate |
| **Auto-apply with trust ramp (index/vacuum/config/DDL)** | **DIFFERENTIATOR** | Empty rung in portable Postgres; only Oracle/Azure (locked) and Postgres.ai (aspirational) nearby |
| **Validate-then-apply-then-verify-then-rollback loop** | **DIFFERENTIATOR** | The closed loop Azure/Oracle prove and everyone in OSS Postgres stops short of |
| **No-extension, over-the-wire deployment** | **DIFFERENTIATOR** | PoWA/Neon/online_advisor need shared_preload_libraries; pg_sage runs anywhere |
| **Provider-neutral reconciler of cloud advisors** | **DIFFERENTIATOR** | Every advisor is provider-locked; pg_sage can unify + apply across all of them |
| **Transparent evidence + audit per autonomous action** | **DIFFERENTIATOR** | Oracle is a black box; pganalyze refuses autonomy; pg_sage can be auto *and* auditable |

---

## Open Questions

1. **Does the market actually want auto-apply for tuning, or just trustworthy advice?** pganalyze and Xata bet — loudly
   — that the answer is *advice*, citing accountability. Oracle/Azure prove the opposite at the high end. pg_sage's
   entire thesis rests on auto-apply being the wedge. Is there validated demand, or is the trust ramp a feature DBAs
   will leave permanently in "advisory"? (REVERSE_SPEC notes trust **does not auto-ramp** today — so even pg_sage ships
   advisory-by-default.)

2. **Is the no-extension story strong enough to win, or merely nice-to-have?** PoWA/online_advisor get sharper signal
   *because* they have in-DB hooks (pg_qualstats predicates, executor hooks). How much recommendation quality does
   pg_sage sacrifice by staying over-the-wire, and does that erode the differentiator?

3. **Should pg_sage integrate DBLab/Neon branches as the verification gate before v1?** It is the single most credible
   answer to "you let an AI touch prod" — and the prior research already flags it as "next." Build vs. partner vs. adapter?

4. **Does provider-neutral reconciliation matter if clouds keep bundling free advisors?** The OtterTune post-mortem says
   cloud bundling is an existential threat. Is "we unify and *apply* across all your providers" a durable wedge, or do
   most shops live on one cloud and use its native advisor?

5. **Where does pg_sage sit relative to Postgres.ai's "Self-Driving Postgres"?** Same axis, same framing, overlapping
   roadmap, and they have DBLab + GitLab/Supabase scale. If they reach Level 3-4 first, what is pg_sage's defensible
   edge — the no-extension footprint, AGPL/self-host, or breadth of auto-actions?

6. **Is "auto-apply for schema/perf" acceptable the way failover is — and what is the Patroni-grade safety bar?** HA
   tools earned autonomous trust via rigorous, battle-tested safety (leader leases, fencing). What is the equivalent
   proof pg_sage must show (chaos tests? a public incident-free track record? formal rollback guarantees?) before a DBA
   flips it from advisory to autonomous on a production fleet?

7. **MCP surface vs. web-UI-primary direction:** the whole field shipped MCP servers in 2025-2026, but pg_sage's own
   memory says the direction is web-UI-primary (MCP de-emphasized). Is skipping a first-class MCP/agent surface a
   strategic miss now that agents create 97% of new databases?

---

*Generated 2026-06-10. Verified corrections vs. prior framing: pg_auto_failover is **not** deprecated (community
`hapostgres` org, v2.2, PG 13-18); Crunchy Data acquired by **Snowflake** (~$250M, June 2025), not a PG-focused PE
firm; Neon acquisition **closed** July 31, 2025; OtterTune **dead** (June 14, 2024) but `cmu-db/ottertune` repo lives;
Timescale **pgai archived** Feb 26, 2026 and the company is now **Tiger Data**; pganalyze added a **cluster-aware Index
Advisor** (Jan 2026) and an **MCP server** (April 2026) while explicitly **refusing** auto-apply.*
