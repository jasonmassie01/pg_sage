# Cross-Database AI/Autonomous Landscape (Beyond Postgres)

> Research date: 2026-04-29
> Author: pg_sage research
> Companion to: `competitive_landscape.md` (Oracle / SQL Server / PostgresAI / pganalyze)
> Purpose: Map autonomous & AI features shipping on data warehouses, NoSQL, NewSQL, and adjacent managed Postgres clouds — and decide what pg_sage should mirror, leapfrog, or ignore.

This file deliberately does **not** re-cover what's already in `competitive_landscape.md` (Oracle 26ai, Azure SQL automatic tuning, pganalyze, PostgresAI, OtterTune, DBtune, EDB, Percona, Aiven AI Optimizer, AWS Performance Insights, AlloyDB, Neon's basic feature set). It picks up the gaps: warehouses, NoSQL, NewSQL, the Lakebase/Lakehouse axis, and the "agent-native" Postgres clouds.

---

## 1. Snowflake — Autonomous-ish on the Warehouse Side

### What ships today

Snowflake's "autonomous" surface area is split between **physical layout** services (which are genuinely automatic) and **Cortex AI** (which is application-AI, not DBA-AI).

- **Automatic Clustering** — define a clustering key on a table and Snowflake's serverless backend continuously reclusters micro-partitions during DML. No warehouse to provision. User levers are limited to `SUSPEND RECLUSTER` / `RESUME RECLUSTER` and a `SYSTEM$ESTIMATE_AUTOMATIC_CLUSTERING_COSTS` cost-estimator function. ([Snowflake docs — Automatic Clustering](https://docs.snowflake.com/en/user-guide/tables-auto-reclustering))
- **Search Optimization Service (SOS)** — opt-in per-table data structure that maintains a "search access path" so point lookups skip micro-partitions. Storage cost is roughly 1/4 of the source table; estimator is `SYSTEM$ESTIMATE_SEARCH_OPTIMIZATION_COSTS`. ([Snowflake docs — SOS cost](https://docs.snowflake.com/en/user-guide/search-optimization/cost-estimation))
- **Cortex Search** — managed hybrid (vector + keyword + semantic) search service for RAG, billed per token embedded plus per-GB-month of indexed data plus serving compute that runs 24/7 regardless of warehouse size. ([Snowflake docs — Cortex Search costs](https://docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-search/cortex-search-costs))
- **Cortex AISQL** (GA Nov 2025) — `AI_COMPLETE`, `AI_CLASSIFY`, `AI_FILTER`, `AI_AGG`, `AI_EMBED`, `AI_EXTRACT`, `AI_SENTIMENT`, `AI_SIMILARITY`, `AI_TRANSCRIBE`, `AI_PARSE_DOCUMENT`, `AI_REDACT`, `AI_TRANSLATE`. Token-priced; "most use cases $10–100/mo" but pathological queries hit thousands. ([Snowflake — AISQL GA notes](https://docs.snowflake.com/en/release-notes/2025/other/2025-11-04-cortex-aisql-operators-ga), [Seemore — $5K single-query](https://seemoredata.io/blog/snowflake-cortex-ai/))

### Autonomous vs advisory vs observability

| Feature | Mode |
|---|---|
| Automatic Clustering | **Autonomous** once a clustering key is set |
| Search Optimization Service | **Advisory + manual enable**, autonomous maintenance once on |
| Cortex Search | Managed service, not DBA automation |
| Cortex AISQL | Application AI inside SQL, not DBA AI |

There is **no Snowflake equivalent of an index advisor or config advisor for DBAs** — partly because Snowflake's architecture hides those knobs. The "autonomous DBA" surface is much smaller than Oracle's.

### Pricing structure

Credit-based serverless compute for Automatic Clustering and SOS, plus storage for SOS access paths. Cortex AI is token + serving-compute. ([Cortex pricing 2026 guide](https://dataengineerhub.blog/articles/snowflake-cortex-cost-comparison))

### Lessons / threats / opportunities

- **Lesson:** The "set a key, we maintain it forever" UX of Automatic Clustering is exactly what pg_sage's auto-index management aspires to — a declarative intent, with the system handling the maintenance loop. pg_sage should expose intents (e.g. "this table should be queryable by `tenant_id` cheaply") not just outputs ("create this index").
- **Lesson:** Cost-estimator functions (`SYSTEM$ESTIMATE_*`) before enabling expensive autonomous features are an underrated UX pattern. pg_sage's executor should always show a *projected* cost/benefit before enabling a moderate-or-higher action, especially for HypoPG-validated indexes.
- **Threat:** Low. Snowflake is not in the operational-Postgres market.
- **Opportunity:** None of Snowflake's "autonomous" features address vacuum, bloat, lock contention, replication, or query plan regression — the things that actually break Postgres in production.

---

## 2. Databricks Lakebase + Genie + Predictive Optimization

### What ships today

Databricks made three big moves relevant to pg_sage in 2026:

- **Lakebase (GA, FabCon March 2026)** — managed serverless Postgres with sub-second startup, autoscaling, scale-to-zero, branching, instant restore, automatic failover. Marketed explicitly as "the database for AI agents." ([Databricks blog — FabCon 2026](https://www.databricks.com/blog/whats-new-azure-databricks-fabcon-2026-lakebase-lakeflow-and-genie)) Lakebase Autoscaling billing started January 2026 after a free exploration period. ([HPCwire — Databricks Doubles Down](https://www.hpcwire.com/bigdatawire/2026/02/13/databricks-doubles-down-on-ai-with-lakebase-genie-and-a-surging-valuation/))
- **Predictive Optimization (PO) for Unity Catalog** — fully autonomous OPTIMIZE / VACUUM / ANALYZE / CLUSTER BY for Delta tables. Workload-driven, adaptive, no manual schedules. Now **default-on for all new UC managed tables** with rollout to existing accounts completing April 2026. Auto-VACUUM uses an optimized log-based path that skips directory listings when possible. ([Databricks — PO at scale](https://www.databricks.com/blog/predictive-optimization-scale-year-innovation-and-whats-next), [Databricks docs — PO for UC](https://docs.databricks.com/aws/en/optimizations/predictive-optimization))
- **AI/BI Genie + Genie Code** — Genie Agent Mode does multi-step reasoning over data; Genie Code (March 2026) is an autonomous coding agent that writes/debugs/executes SQL and Python, **proactively maintains and optimizes Lakeflow pipelines and ML models in the background**, triages failures, and analyzes agent traces to fix hallucinations. ([Databricks — Introducing Genie Code](https://www.databricks.com/blog/introducing-genie-code))

### Autonomous vs advisory vs observability

| Feature | Mode |
|---|---|
| Predictive Optimization (OPTIMIZE/VACUUM/ANALYZE/CLUSTER) | **Fully autonomous, default-on** |
| Lakebase autoscaling / scale-to-zero | **Autonomous** |
| Genie Agent Mode | **Conversational analytical agent** (advisory for humans) |
| Genie Code | **Semi-autonomous** — writes and runs code, with proactive background maintenance |

### Pricing

Lakebase: usage-based DBU pricing on the autoscaling compute model; details not publicly itemized but Lakeflow Connect now has a free tier of 100 DBUs/day (~100M records/day ingest). Predictive Optimization is bundled with Unity Catalog managed tables (no separate fee).

### Lessons / threats / opportunities

- **Threat (highest in this report):** Lakebase + Genie Code is the most direct conceptual competitor to pg_sage. Databricks is explicitly marketing a Postgres database designed for agents, with an autonomous coding agent that "proactively maintains pipelines in the background." If they apply the same pattern to Lakebase itself — autonomous schema, index, vacuum maintenance via Genie Code — they ship pg_sage's value proposition for free to every Lakebase customer.
- **Lesson:** "Default-on autonomous maintenance" (PO) is the endgame UX. Databricks decided the answer to "should I auto-VACUUM?" is *yes, by default, for every new table*. pg_sage's trust ramp is the right *transition* mechanism but the *destination* is default-on for SAFE actions.
- **Lesson:** PO's Data Governance Hub surfaces "bytes compacted, bytes vacuumed, bytes clustered → estimated $ savings". This is the cost-quantification messaging Revefi nailed for Snowflake. pg_sage should ship a "value report" page that quantifies what its actions saved.
- **Opportunity:** Lakebase is Databricks-only. pg_sage's portability across RDS/CloudSQL/Aurora/AlloyDB/self-managed remains the durable wedge.

---

## 3. Google BigQuery — Recommenders Everywhere, No Index Advisor

### What ships today

BigQuery's "autonomous" surface is mostly **recommendation-only** delivered through Active Assist:

- **Partitioning & Clustering Recommender** — analyzes 30 days of workload, uses ML to estimate savings, generates partition/cluster suggestions. Tables ≥100 GB for partitioning recommendations, ≥10 GB for clustering. Surfaced in BigQuery Studio's Active Assist panel. ([Google Cloud — Partitioning/clustering recommender](https://cloud.google.com/blog/products/data-analytics/new-bigquery-partitioning-and-clustering-recommendations), [Recommendations overview](https://cloud.google.com/bigquery/docs/recommendations-intro))
- **Slot autoscaling** — as of late 2025/early 2026, autoscalers scale in 50-slot increments (down from 100), scale-up is now "instant," and capacity can be reduced without resetting the 1-min minimum if >1 min has passed since last increase. Workload management got better, but slots autoscaling is **opt-in via Reservations**. ([oneuptime — BQ autoscaling 2026](https://oneuptime.com/blog/post/2026-02-17-how-to-set-up-bigquery-editions-and-configure-autoscaling-slots/view), [BigQuery slots autoscaling intro](https://docs.cloud.google.com/bigquery/docs/slots-autoscaling-intro))
- **BigQuery ML** — in-warehouse model training, including k-means clustering for *data* (segmentation), not for table layout.
- **Vector search / VECTOR_SEARCH function** — vector indexes (IVF, TreeAH) and `VECTOR_SEARCH()` SQL function. Maintenance is automatic for managed indexes.
- **Slot estimator** — historical 30-day slot-utilization analysis to right-size reservations. Capacity advice, not query advice.

### Autonomous vs advisory vs observability

Almost everything in BQ that touches DBA-style work is **advisory** delivered via Recommender. Slot autoscaling is autonomous within a configured reservation. Vector index maintenance is autonomous once enabled.

### Pricing

On-demand ($/TB scanned) or Editions (Standard/Enterprise/Enterprise Plus, slot-hour billing). Recommender is free. Slot estimator is free.

### Lessons / threats / opportunities

- **Lesson:** Even at BigQuery's scale, Google does **not** auto-execute partition or cluster changes. It surfaces high-confidence recommendations and lets the user accept. This validates pg_sage's trust-ramp middle tier (Advisory) — the world's largest data platform stops at advisory for layout changes.
- **Lesson:** Slot autoscaling moving to 50-slot increments is small but instructive: *granularity* of autoscaling is a quality dimension. pg_sage's connection-pooler / work_mem advice should think in fine-grained ramps, not big jumps.
- **Threat:** Low — BigQuery is not Postgres-compatible.
- **Opportunity:** BQ's Active Assist UX (a single panel of AI-generated, business-value-quantified recommendations) is a clean dashboard pattern. pg_sage's findings page should adopt the "recommender card with $ impact" format.

---

## 4. MongoDB Atlas — Performance Advisor + Autopilot

### What ships today

Mongo's advisor stack is the closest NoSQL parallel to pganalyze:

- **Performance Advisor** — analyzes slow queries from the profiler, recommends indexes to add and drop, surfaces "index ranking" by estimated impact, with the queries that would benefit. One-click create from the UI. ([Atlas docs — Performance Advisor](https://www.mongodb.com/docs/atlas/performance-advisor/))
- **Auto-Index Creation (Autopilot)** — for **Atlas Serverless** and now broader tiers, Atlas can proactively analyze unindexed query patterns and **automatically create indexes** when impact is high enough, with rollback if the index turns out to be unused or harmful. ([Atlas docs — Autopilot](https://www.mongodb.com/docs/atlas/performance-advisor/autopilot/), [MongoDB blog — Auto-Index for Serverless](https://www.mongodb.com/blog/post/introducing-auto-index-creation-atlas-serverless-instances))
- **Query Insights (2026 redesign)** — unified dashboard merging Namespace Insights and Query Profiler with ranked-by-impact slow query lists. ([MongoDB — Introducing Query Insights](https://www.mongodb.com/company/blog/product-release-announcements/elevating-database-performance-introducing-query-insights-mongodb-atlas))
- **Atlas Vector Search** — managed vector indexes (HNSW), also supports hybrid lexical+vector queries.
- **Atlas Search** — Lucene-based full-text indexes; index creation is manual but maintenance is auto.

### Autonomous vs advisory vs observability

| Feature | Mode |
|---|---|
| Performance Advisor index recs | **Advisory** (one-click apply) |
| Autopilot auto-index | **Autonomous on serverless**, advisory-by-default elsewhere |
| Query Insights | **Observability + advisory** |
| Vector Search index maintenance | **Autonomous** once created |

### Pricing

Bundled with the Atlas tier; Serverless includes Autopilot at no extra fee. Performance Advisor and Query Insights are free with Atlas.

### Lessons / threats / opportunities

- **Lesson (most important from this report):** Mongo's "rank suggestions by estimated impact, then offer one-click apply" is the right mid-trust UX pattern. It's exactly the gap between "pganalyze recommendation" and "pg_sage executor." pg_sage's web UI should adopt the *index ranking* pattern — every finding gets a numeric impact score, a one-click "apply now," AND an "auto-apply if score ≥ X" toggle that escalates from manual → ranged auto.
- **Lesson:** Autopilot on Serverless is auto, on dedicated tiers it's advisory. This **per-tier autonomy** is interesting — pg_sage could expose per-database trust levels (already does in fleet mode), but could also tie autonomy to instance class (e.g. dev/staging databases auto-apply, prod stays advisory).
- **Threat:** Low — different data model.
- **Opportunity:** Mongo doesn't have a config advisor (no equivalent of work_mem tuning), so the "advisor-like" UX is mostly about indexes. pg_sage's breadth (config + vacuum + bloat + replication) remains broader than Atlas's advisor.

---

## 5. CockroachDB Cloud — Mostly MCP, Limited Autonomy

### What ships today

CockroachDB's 2026 push is heavily into AI-assistance and MCP:

- **AskAI in CockroachDB Cloud Console** — in-product chatbot for help while using the service. ([Cockroach Labs — AI Assistance](https://www.cockroachlabs.com/blog/cockroachdb-ai-assistance-for-developers/))
- **CockroachDB Documentation MCP Server** — exposes docs, best practices, prescriptive how-tos to MCP-compatible coding assistants.
- **CockroachDB Cloud MCP Server** — cluster-level **read+write** access, lets AI assistants list databases/tables, describe schemas, inspect cluster health, run read-only SQL and EXPLAIN. ([CockroachDB & AI](https://www.cockroachlabs.com/docs/stable/cockroachdb-and-ai))
- **Automatic statistics** — Cockroach has long auto-collected table stats; configurable but on-by-default.
- **Auto-rebalancing** — data and load are automatically rebalanced across nodes; this is foundational, not a 2026 feature.
- **Auto-scaling (Cloud Serverless tier)** — RU-based auto-scaling.

What's notably absent: **no autonomous index management, no config advisor, no vacuum/bloat features specific to operations** — Cockroach's storage is LSM-based so the Postgres-style bloat problem doesn't directly apply.

### Pricing

CockroachDB Cloud Serverless is RU-based; Dedicated and Advanced tiers are vCPU/storage-based. AI MCP servers are free.

### Lessons / threats / opportunities

- **Lesson:** CockroachDB's two-MCP-server pattern (Docs MCP for guidance, Cluster MCP for action) is a clean separation pg_sage should mirror. A read-only "documentation/best-practices" MCP server can ship today. A "cluster MCP" with executor wiring is a v2 feature.
- **Lesson:** Cluster MCP is **read-only SQL** — even Cockroach won't let agents write to a production cluster via MCP. This validates pg_sage's trust-ramp gating: agents should not get unconstrained execute privileges.
- **Threat:** Very low — different ecosystem, different storage layer, different problems.
- **Opportunity:** None directly portable. The MCP-server pattern is the only takeaway.

---

## 6. PlanetScale — The Schema-Change Workflow Is the Crown Jewel

### What ships today

PlanetScale is the most interesting case study in this report because their workflow innovation is **portable to Postgres** even though their underlying engine (Vitess/MySQL) is not.

- **Deploy Requests** — schema changes happen on a branch, get reviewed (deploy request), then deploy non-blocking with zero downtime via Vitess online migrations. Three modes: standard (online migration), instant (`ALGORITHM=INSTANT` for column add/drop/default change), and gated (sync offline, manual cutover). ([PlanetScale docs — Deploy requests](https://planetscale.com/docs/vitess/schema-changes/deploy-requests))
- **30-minute revert window** — undo a deployment within 30 min while preserving data written after the deploy.
- **Pre-deployment validation** — automatic checks for charset issues, FK mismatches, unique-key conflicts, etc.
- **Schema Recommendations** — separate advisor surface; analyzes schemas for anti-patterns.
- **Insights** — query monitoring with deploy requests **overlaid on the perf graph** so you can correlate schema changes with performance regression.
- **Boost** — memory-backed cached read layer using Vitess VStream; up to 1000× speedup for repeated reads, similar consistency model to a fresh read replica. ([PlanetScale Boost — HN](https://news.ycombinator.com/item?id=33611759))
- **PlanetScale MCP server** — recently shipped a variant exposing Insights + Schema Recommendations **without** query execution. ([PlanetScale changelog](https://planetscale.com/changelog))

### Autonomous vs advisory vs observability

| Feature | Mode |
|---|---|
| Deploy Requests | **Workflow** — gated by humans, validated by system |
| Schema Recommendations | **Advisory** |
| Insights | **Observability** |
| Boost | **Autonomous-ish caching layer** (must be enabled per query) |

### Pricing

Database plans, scaler-based; free tier removed in 2024.

### Lessons / threats / opportunities

- **Lesson (highest-leverage in this report):** The deploy-request workflow is exactly what pg_sage's executor should expose for HIGH-risk changes — *and probably for some MODERATE ones too*. Imagine: pg_sage opens a "deploy request" in its dashboard for a `CREATE INDEX CONCURRENTLY`, the user reviews, approves, pg_sage runs the migration with progress, and offers a 30-min revert window (drop the index if perf got worse). Every action becomes a reviewable diff with rollback. This is a much stronger UX than "trust level → did or didn't run." **pg_sage v1.x should ship Deploy Requests for HIGH-risk and opt-in for MODERATE.**
- **Lesson:** Overlaying recent deploys on the performance graph is a tiny UX detail with huge value. pg_sage's dashboard should overlay executor actions on the slow-query / latency timeline so users can immediately see "oh, that index I created made p99 50% better."
- **Lesson:** MCP-server-without-query-execution is a useful product line. pg_sage could ship two MCP servers: one read-only (findings, recommendations) and one with executor access (gated by trust level).
- **Threat:** Low — MySQL/Vitess world.
- **Opportunity:** The 30-minute revert window is *generic*. Postgres can do this for index creation (drop the index), config changes (revert via reload), and even some vacuum settings. Adding "revert window" semantics to the executor is medium-effort, very high UX value.

---

## 7. Neon — The "Agent-Native" Reference Implementation

### What ships today

Neon's 2026 positioning is unambiguous: **Postgres for AI agents**. Most of the developer-experience features are covered in `competitive_landscape.md`; what's *new* and important here is the agent plan.

- **Agent Plan (live 2026)** — explicitly designed for platforms deploying Postgres on behalf of end users. Two-org structure (free + paid), each up to 30,000 projects. Agent-specific pricing: $0.106/CU-hour compute, $0.35/GB-month storage, $0.2/GB-month instant-restore. Up to $25k initial credits and infrastructure sponsorship for free-tier end users. ([Neon docs — Agent plan](https://neon.com/docs/introduction/agent-plan), [Neon — AI agent platforms](https://neon.com/use-cases/ai-agents))
- **OpenAI Codex plugin (April 17, 2026)** — Neon Postgres plugin officially in the Codex plugin directory; Codex agents can create/manage projects, branches, databases via the Neon MCP server with included Agent Skills.
- **Every primitive exposed via API** — provisioning, quotas, branching, instant restore, snapshots, project transfers — all programmatic, all designed for thousands-of-databases-per-tenant fan-out.
- **Branching as the agent's "git"** — instant copy-on-write database forks for experimentation; agents that screw up don't break prod.

### Autonomous vs advisory vs observability

Neon itself is not autonomous in the DBA sense. It is **agent-friendly substrate**: the autonomy lives in *whatever agent is driving Neon*, not in Neon. There is no Neon-native index advisor, config advisor, or vacuum tuner.

### Pricing

Agent plan is the relevant SKU. Per-CU-hour and per-GB metered, not per-database fixed cost (critical for fan-out).

### Lessons / threats / opportunities

- **Lesson (high):** "Agent-friendly" UX = every operation is an idempotent API call, every primitive has a well-defined lifecycle, no resource exhaustion when creating thousands of objects. pg_sage already exposes 17 REST endpoints; the test of agent-friendliness is "can an agent loop on these for a fleet of N=10,000 databases without falling over?" pg_sage should harden its API for that scale and explicitly market a "fleet API" alongside the dashboard.
- **Lesson:** Neon's plugin in the Codex directory matters. **Distribution via coding-tool plugin marketplaces** is a real channel. pg_sage should ship a Codex plugin and a Cursor plugin alongside the existing Claude Code skill, exposing the same MCP surface.
- **Threat:** Medium-low. Neon doesn't compete with pg_sage's DBA-automation surface — they're complementary. But if Databricks decides Lakebase/Neon should grow Genie-Code-style autonomous DBA features, that's a real threat.
- **Opportunity:** pg_sage as the "DBA brain" running against a Neon fleet is a coherent story. A turnkey integration ("`pg_sage --neon-org=$X` discovers all your Neon projects and starts monitoring") would be a strong 1.x feature.

---

## 8. Supabase — Splinter Lints, No Auto-Apply

### What ships today

Supabase ships an opinionated advisor layer driven by an open-source linter:

- **Splinter (open source)** — the engine behind Database Advisors. Each lint is a SQL query that detects a schema/security issue. Severity: ERROR/WARN/INFO. Categories: SECURITY and PERFORMANCE. Includes lints like unindexed foreign keys, RLS policy issues, security-definer view antipatterns, missing indexes. ([GitHub — supabase/splinter](https://github.com/supabase/splinter), [Supabase docs — Database Advisors](https://supabase.com/docs/guides/database/database-advisors))
- **Performance & Security Advisor in Studio** — runs Splinter, surfaces findings, lets users disable individual rules. Customizable.
- **index_advisor extension** (Postgres extension built by Supabase) — virtual-index "what if?" tester. Limited to single-column B-tree indexes. ([Supabase docs — index_advisor](https://supabase.com/docs/guides/database/extensions/index_advisor))
- **Query Performance Report** — slow-query view, integrates with index_advisor for per-query suggestions.

What Supabase **does not** do: auto-execute index creation, auto-tune config knobs, vacuum tuning, bloat remediation. It's a **lint + recommend** product, not an executor.

### Autonomous vs advisory vs observability

Pure **advisory + observability**. No autonomous execution.

### Pricing

Bundled free with all Supabase tiers.

### Lessons / threats / opportunities

- **Lesson:** Open-sourcing the linter (Splinter) drove ecosystem adoption — running Splinter against your own Postgres without Supabase is a real workflow. pg_sage could open-source its rule pack as a standalone CLI (`pg_sage lint`) that runs Tier 1 rules against any Postgres without the full sidecar. Low cost, big distribution.
- **Lesson:** Customizable rule disabling is table stakes. pg_sage already has finding suppression; should make sure each rule has a stable rule ID and a public list (like Splinter's enumerated lints).
- **Lesson (negative):** index_advisor is single-column B-tree only. pg_sage's HypoPG-driven optimizer covers multi-column and partial indexes — that's a real advantage to lead with in marketing.
- **Threat:** Medium. Supabase's distribution and free-tier reach are massive. If they ship an executor for Splinter findings, they immediately reach more Postgres developers than pg_sage will for years.
- **Opportunity:** Position pg_sage as "Splinter + executor + LLM RCA + config tuning" — what Splinter is, plus everything Splinter doesn't do.

---

## 9. Aiven / Crunchy Bridge / Timescale — The Managed-PG Long Tail

### Aiven for PostgreSQL

Aiven's AI Database Optimizer is already covered in `competitive_landscape.md`. Beyond that: standard managed-PG features (HA, automated minor upgrades, backups), 70+ extensions including TimescaleDB. No autonomous DBA features beyond the AI Optimizer add-on. Pricing on the higher end. ([Aiven for PostgreSQL](https://aiven.io/postgresql))

### Crunchy Bridge / Crunchy Data

- **Database Insights** — automated point-in-time insights with action recommendations: cache hit ratio, index hit rate, slow queries, unused indexes that can be dropped, table size, locks, long-running queries. ([Crunchy — Introducing Database Insights](https://www.crunchydata.com/blog/introducing-database-insights-effortless-postgres-management-with-crunchy-bridge))
- **CLI insights via `cb psql --menu`** — terminal-native browse-and-act. The CLI navigation pattern is a differentiator.
- **Production check** — startup-time check listing cluster status against recommended settings. ([Crunchy — Insights metrics](https://docs.crunchybridge.com/insights-metrics))
- **No autonomous executor** — recommendations are surfaced; user acts.

### Timescale Cloud / Tiger Cloud

- **Storage autoscaling** (compute autoscaling marketed as "in the works"). ([Timescale — Storage autoscaling](https://www.timescale.com/blog/grow-worry-free-storage-autoscaling-on-timescale-cloud/))
- **Built-in performance insights** — slow query tracking, autocomplete, dashboards, an SQL assistant.
- **Note:** Timescale archived their `pgai` repo Feb 2026 (covered in `competitive_landscape.md`). Direction of their AI play is unclear.

### Lessons / threats / opportunities

- **Lesson:** Crunchy's CLI menu (`cb psql --menu`) is a clever DX move — DBAs live in psql, surfacing advisor findings *inside* the psql session reduces friction enormously. pg_sage should ship a `pg_sage psql` wrapper or a server-side function pack callable as `SELECT * FROM sage.findings_summary();` so a DBA never has to leave their terminal.
- **Lesson:** "Production check at startup" is a great onboarding feature. pg_sage already has startup validation; the user-visible "here's your day-1 health report" should be more prominent in the UX.
- **Threat:** Low individually, but collectively these vendors all bundle "advisor lite" with their managed offering. pg_sage's free + executor + LLM-RCA combo remains differentiated.
- **Opportunity:** Crunchy and Timescale are good candidates for "pg_sage works great on Crunchy/Timescale" partnerships — neither has an executor, both have technical buyers.

---

## 10. ClickHouse Cloud, MotherDuck, DuckDB — Analytical Side

### ClickHouse Cloud

- **Auto-idling on primary services** (rolling to public preview, default-on for new services) — scale-to-zero analog. ([ClickHouse — Cloud changelog 2026](https://clickhouse.com/docs/whats-new/changelog/cloud))
- **AI-powered SQL console** — autocomplete, broken-query fixing, schema-aware optimization suggestions inline in the SQL editor.
- **Warehouses** (GA Jan 2026) — true compute-compute separation with read-write / read-only designations, similar to Snowflake's multi-warehouse model.
- **Make-Before-Break vertical scaling** — new replicas come online before old ones leave, no capacity gap.
- **Autonomous DBA features:** sparse — ClickHouse's column-store + part-merging design pushes much of the "maintenance" into background merges that have always been autonomous.

### MotherDuck

- **Dives** (April 2026) — embeddable React+SQL components with dual execution (cloud + DuckDB-WASM in the browser). Not a DBA feature but a notable AI-application pattern.
- **Remote MCP Server** — 95% functional correctness on text-to-SQL with schema context. ([MotherDuck blog April 2026](https://motherduck.com/blog/duckdb-ecosystem-newsletter-april-2026/))
- **Serverless execution** — no warehouses or clusters to manage.
- **DuckLake 1.0 support** — open lakehouse table format with clustering, bucket partitioning, geometry/variant types.

### Lessons / threats / opportunities

- **Lesson:** ClickHouse's "AI fixes broken queries inline in the SQL editor" is a query-rewriting product surface that's distinct from pg_sage's current scope. pg_sage already has LLM analysis; an inline "rewrite this slow query" suggestion in the dashboard is a feature pganalyze and Aiven AI Optimizer market heavily, and pg_sage should add.
- **Lesson:** Make-Before-Break scaling is the right safety pattern — never reduce capacity until new capacity is verified. pg_sage's executor could adopt this pattern for MODERATE actions: validate the new state on a test connection / hypothetical index before applying, rather than apply-then-revert.
- **Threat:** None — analytical engines, different niche.
- **Opportunity:** None directly portable.

---

## Synthesis

### A. Table Stakes — every cloud DB now has these, pg_sage should match

1. **Index recommendations with impact ranking and one-click apply.** Mongo, Supabase, pganalyze, Crunchy, Atlas all do this. pg_sage already has the engine; the *ranking + one-click* UX is the table-stakes presentation.
2. **Slow-query insights / query performance dashboard.** Universal. pg_sage has the data; the React dashboard already covers this.
3. **Unused-index detection and "drop me" recommendation.** Universal across Mongo, Supabase, Crunchy, pganalyze, PostgresAI. pg_sage already has this in Tier 1.
4. **Cost / value quantification** ("we saved you $X this month"). Databricks PO Governance Hub, Revefi for Snowflake. pg_sage should ship a value report.
5. **MCP server for the agentic-tools market.** PostgresAI, EDB, Cockroach, PlanetScale, Neon, MotherDuck all have one. pg_sage's roadmap has it; needs to ship.
6. **Programmatic API covering every primitive.** Neon's agent-plan UX. pg_sage's REST API already covers most of this; needs hardening for fan-out fleets.
7. **Customizable / disable-able rules with stable IDs.** Splinter, Mongo, BigQuery Recommender. pg_sage has finding suppression; needs public stable rule IDs.

### B. Differentiators — only one or two have these; pg_sage could leapfrog

1. **PlanetScale-style Deploy Requests for high-risk DDL.** Nobody in Postgres does this. Every executor action of MODERATE+ severity becomes a reviewable diff with: pre-flight validation, projected impact, manual approve, and a 30-minute revert window. Combined with pg_sage's trust ramp, this is a *materially* better UX than Oracle's black-box autonomy or pganalyze's read-only recommendations.
2. **30-minute revert window for executed actions.** PlanetScale-style. Drop the index if p99 didn't improve, revert config if checkpoint behavior changed for the worse. Generic, portable to Postgres, nobody has it.
3. **Two-MCP-server pattern: read-only docs/best-practices + cluster-action.** CockroachDB does this. Read-only ships today (zero risk); cluster-action gates through trust ramp.
4. **Default-on autonomous SAFE actions at the trust-ramp endpoint.** Databricks PO is *default-on autonomous* for OPTIMIZE/VACUUM/ANALYZE on every new managed table. pg_sage's destination should also be default-on for SAFE-tier actions after the trust ramp expires — and the marketing should say so explicitly.
5. **LLM-driven inline query rewrite suggestions.** ClickHouse and Aiven AI Optimizer have this; pganalyze does not; PostgresAI does not. pg_sage already has LLM infra; missing the surface.
6. **Open-source rule-pack lint CLI (`pg_sage lint`).** Splinter pattern; nobody else in the autonomous-DBA space does this. Massive distribution play.
7. **Production-check at startup with day-1 report.** Crunchy does this; pg_sage already has startup validation but the user-visible "day-1 health report" is underdeveloped.
8. **Recommendation overlays on performance timeline.** PlanetScale's "deploy requests on the Insights graph" is the tiny detail with huge value. Trivial to ship, no competitor in Postgres has it.
9. **Cluster MCP server with executor wiring gated by trust ramp.** Cockroach's Cluster MCP is read-only. pg_sage can go further: read-only + advisory-write for low-trust, full-write at autonomous trust. Differentiator.
10. **`SELECT * FROM sage.*` SQL surface for psql-native users.** Crunchy's `cb psql --menu` shows the appetite; pg_sage can do better with first-class SQL functions.

### C. Don't Bother — hyped, but niche or wrong fit for pg_sage

1. **Cortex AISQL / AI_*-style functions.** Application AI inside SQL is a different product (and a loss-leader for warehouse vendors selling tokens). pg_sage is operational, not analytical.
2. **Native vector search as a pg_sage feature.** pgvector exists; managed clouds (RDS, AlloyDB, Neon, Aurora) ship it. pg_sage doesn't need to build vector search; it just needs to *advise on* pgvector index choices (HNSW vs IVF vs DiskANN) — and that's already in scope as an index recommendation.
3. **Compute-compute separation / warehouses.** Snowflake/ClickHouse pattern, irrelevant for OLTP Postgres.
4. **Boost-style memory-cached read layer.** PlanetScale's distinguishing feature, but in Postgres world it's pgbouncer + replicas + ProxySQL, not a pg_sage scope item.
5. **Auto-idling / scale-to-zero compute.** Neon and Lakebase have it, but it's a *substrate* feature, not a *brain* feature. pg_sage runs as a sidecar; idling logic belongs to whoever owns the compute.
6. **Conversational BI agents (Genie, AskAI, Snowflake Copilot, Databricks One).** Different product category — analyst-facing, not DBA-facing. pg_sage's `diagnose` is the right surface for natural language; full BI agents are out of scope.
7. **Autopilot-on-serverless-only.** Mongo's tier-gated autonomy exists because their serverless tier is more constrained. pg_sage's trust ramp handles this conceptually; per-tier autonomy is over-engineering.

---

## What pg_sage Should Steal (Prioritized)

In rough order of impact ÷ effort. Numbers in parentheses indicate the section above where the lesson originated.

1. **Deploy Requests for HIGH-risk DDL** (§6 PlanetScale, §B-1). Every action above SAFE becomes a reviewable diff in the dashboard with pre-flight validation, projected impact, approve button, 30-min revert window. This single feature elevates pg_sage's executor from "autopilot toggle" to "GitOps-style change management for your DBA AI." Highest UX leverage.

2. **30-Minute Revert Window for executed actions** (§6 PlanetScale, §B-2). Generic across index creation, config reload, vacuum-flag changes. Show the action, show the metrics before and after for 30 minutes, give the user a one-click revert. Low engineering cost, massive trust win.

3. **Open-source `pg_sage lint` CLI with stable rule IDs** (§8 Supabase Splinter, §B-6, §A-7). Repackage Tier 1 rules as a standalone CLI with no DB writes, no scheduler, no LLM. Distribute via Homebrew/apt/release artifacts. This is the Splinter playbook: open-source the surface that drives adoption, monetize/differentiate the executor and LLM tiers above it.

4. **Recommendation overlays on the performance timeline** (§6 PlanetScale, §B-8). Add a layer to pg_sage's dashboard timeline showing each executor action as a vertical marker so users immediately see "this index is why p99 dropped." Engineering effort: a few days. UX impact: very high.

5. **Value Report / cost quantification page** (§2 Databricks PO Governance Hub, §A-4). Aggregate executor actions into "this month pg_sage saved you $X via Y dropped indexes, Z reclaimed bloat, W avoided sequence exhaustions." Revefi proved this messaging sells; Databricks proved it scales. Bring the table-stakes feature to Postgres.

6. **Two-tier MCP server: read-only docs/findings + cluster-action gated by trust ramp** (§5 Cockroach, §6 PlanetScale, §B-3, §B-9). Ship the read-only one this quarter (zero risk, big distribution); ship the executor-wired one once trust-ramp gating is hardened. Distribute via Codex/Cursor/Claude Code plugin marketplaces (§7 Neon).

7. **Inline LLM query-rewrite suggestions in the dashboard** (§10 ClickHouse, §1 Aiven, §B-5). pg_sage's LLM infra is built. Surface "rewrite this slow query — here's why, here's the new SQL" cards in the slow-query view. Aiven and ClickHouse already do this; pganalyze doesn't. Differentiator within the Postgres ecosystem.

8. **Index ranking + per-finding impact score with auto-apply threshold toggle** (§4 Mongo, §A-1). Every finding gets a numeric impact score 0–100. UI exposes a "auto-apply if score ≥ X AND severity ≤ MODERATE" toggle. This is the right way to spell "trust ramp" for the index-management subdomain — Mongo's pattern, ported to Postgres.

9. **Day-1 production-check report** (§9 Crunchy, §B-7). When pg_sage starts up against a new database, generate a one-page report: "Here's what's wrong today, here's what we'll watch, here's what we'd auto-fix once we hit AUTONOMOUS trust." Shows value in the first hour, not in week 5.

10. **`SELECT * FROM sage.findings_summary()` and friends — SQL-native surface** (§9 Crunchy CLI menu, §B-10). DBAs live in psql. Expose the dashboard's headline data as a small set of SQL functions inside the `sage` schema so a DBA never has to leave their terminal. Cheap, opinionated, reduces UX friction for the exact persona pg_sage targets.

---

## Sources (added since `competitive_landscape.md`)

### Snowflake
- [Cortex AISQL GA notes (Nov 2025)](https://docs.snowflake.com/en/release-notes/2025/other/2025-11-04-cortex-aisql-operators-ga)
- [Cortex Search costs](https://docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-search/cortex-search-costs)
- [Automatic Clustering docs](https://docs.snowflake.com/en/user-guide/tables-auto-reclustering)
- [Search Optimization cost estimation](https://docs.snowflake.com/en/user-guide/search-optimization/cost-estimation)
- [Cortex pricing 2026 guide](https://dataengineerhub.blog/articles/snowflake-cortex-cost-comparison)
- [The hidden cost of Snowflake Cortex AI ($5K query)](https://seemoredata.io/blog/snowflake-cortex-ai/)

### Databricks / Lakebase
- [What's new in Azure Databricks at FabCon 2026](https://www.databricks.com/blog/whats-new-azure-databricks-fabcon-2026-lakebase-lakeflow-and-genie)
- [Introducing Genie Code](https://www.databricks.com/blog/introducing-genie-code)
- [Databricks doubles down on AI with Lakebase, Genie](https://www.hpcwire.com/bigdatawire/2026/02/13/databricks-doubles-down-on-ai-with-lakebase-genie-and-a-surging-valuation/)
- [Predictive Optimization at scale](https://www.databricks.com/blog/predictive-optimization-scale-year-innovation-and-whats-next)
- [PO for Unity Catalog managed tables](https://docs.databricks.com/aws/en/optimizations/predictive-optimization)

### BigQuery
- [Partitioning and clustering recommendations](https://cloud.google.com/blog/products/data-analytics/new-bigquery-partitioning-and-clustering-recommendations)
- [Recommendations overview](https://cloud.google.com/bigquery/docs/recommendations-intro)
- [Slot autoscaling intro](https://docs.cloud.google.com/bigquery/docs/slots-autoscaling-intro)
- [BigQuery autoscaling 2026 setup guide](https://oneuptime.com/blog/post/2026-02-17-how-to-set-up-bigquery-editions-and-configure-autoscaling-slots/view)

### MongoDB Atlas
- [Performance Advisor](https://www.mongodb.com/docs/atlas/performance-advisor/)
- [Auto-Index Creation (Autopilot)](https://www.mongodb.com/docs/atlas/performance-advisor/autopilot/)
- [Auto-Index Creation for Serverless](https://www.mongodb.com/blog/post/introducing-auto-index-creation-atlas-serverless-instances)
- [Query Insights launch](https://www.mongodb.com/company/blog/product-release-announcements/elevating-database-performance-introducing-query-insights-mongodb-atlas)

### CockroachDB
- [CockroachDB AI assistance](https://www.cockroachlabs.com/blog/cockroachdb-ai-assistance-for-developers/)
- [CockroachDB and AI / MCP servers](https://www.cockroachlabs.com/docs/stable/cockroachdb-and-ai)

### PlanetScale
- [Deploy requests](https://planetscale.com/docs/vitess/schema-changes/deploy-requests)
- [PlanetScale changelog](https://planetscale.com/changelog)
- [Introducing PlanetScale Insights](https://planetscale.com/blog/introducing-planetscale-insights-advanced-query-monitoring)
- [How PlanetScale Boost serves SQL queries faster (HN)](https://news.ycombinator.com/item?id=33611759)

### Neon
- [Agent plan structure and pricing](https://neon.com/docs/introduction/agent-plan)
- [Neon for AI Agent Platforms](https://neon.com/use-cases/ai-agents)
- [Neon changelog](https://neon.com/docs/changelog)

### Supabase
- [Database Advisors guide](https://supabase.com/docs/guides/database/database-advisors)
- [Splinter — open-source linter](https://github.com/supabase/splinter)
- [index_advisor extension](https://supabase.com/docs/guides/database/extensions/index_advisor)

### Crunchy / Timescale / Aiven
- [Crunchy — Introducing Database Insights](https://www.crunchydata.com/blog/introducing-database-insights-effortless-postgres-management-with-crunchy-bridge)
- [Crunchy — Insights and metrics](https://docs.crunchybridge.com/insights-metrics)
- [Timescale storage autoscaling](https://www.timescale.com/blog/grow-worry-free-storage-autoscaling-on-timescale-cloud/)
- [Tiger Data Cloud](https://www.tigerdata.com/cloud)
- [Aiven for PostgreSQL](https://aiven.io/postgresql)

### ClickHouse / MotherDuck
- [ClickHouse Cloud changelog 2026](https://clickhouse.com/docs/whats-new/changelog/cloud)
- [ClickHouse April 2026 newsletter](https://clickhouse.com/blog/202604-newsletter)
- [MotherDuck DuckDB Ecosystem Newsletter — April 2026](https://motherduck.com/blog/duckdb-ecosystem-newsletter-april-2026/)
- [MotherDuck architecture](https://motherduck.com/docs/concepts/architecture-and-capabilities/)
