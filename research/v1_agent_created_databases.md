# Agent-Created Databases: What pg_sage Should Do About Them

**Date:** 2026-04-29
**Scope:** Forward-looking exploration of databases created, owned, and operated by AI agents — explicitly excluding vector search, which is covered separately.
**Audience:** pg_sage roadmap.

---

## 1. What "Agent-Created Database" Actually Means in 2026

The phrase collapses several distinct phenomena. Conflating them is the first mistake; pg_sage needs vocabulary that separates them.

### 1a. Agent-provisioned databases (the demographic shift)

The single most important data point: as of late 2025, over 80% of databases provisioned on Neon were created by AI agents, not humans, up from 0.1% in October 2023; 97% of database *branches* on Neon are now created by agents ([SaaStr/Databricks](https://www.saastr.com/databricks-only-19-of-organizations-have-deployed-ai-agents-but-theyre-already-creating-97-of-databases/), [Threads/Bevzenko](https://www.threads.com/@sergebevzenko/post/DSZcIv0iiuO/of-databases-on-neon-serverless-postgre-sql-used-by-open-ai-replit-vercel-are)). Databricks' $1B Neon acquisition is the macro signal that this is now the dominant DB-creation pathway in serverless Postgres. The implication: **the median Postgres in 2026 has no human DBA and never had one.**

### 1b. Vibe-coded schemas (DDL-by-LLM)

A coding agent (Cursor, Claude Code, Devin, Codex, Windsurf) emits CREATE TABLE / ALTER TABLE based on natural-language requirements. Claude Code holds the schema in memory and applies migrations in response to instructions like "add a nickname column to users" ([56kode](https://www.56kode.com/posts/moving-from-cursor-to-claude-code/)). The DDL is not always reviewed before it lands — particularly in solo-developer "vibe coding" where the human approves at the feature level, not the schema level. The recurring failure mode is "almost right, that's the problem": columns missing FK indexes (Postgres doesn't auto-index FKs), wrong precision, unconstrained TEXT for what should be enums, missing NOT NULL ([boringSQL](https://boringsql.com/posts/dont-let-ai-to-prod/)).

### 1c. Agent-memory databases (the agent IS the primary client)

These are databases whose *only* meaningful client is an agent runtime: Letta/MemGPT core/archival/recall tiers, Mem0's three-tier user/session/agent stores, Zep's temporal episodic graph, Cognee, Memori, Anthropic's memory product, ChatGPT projects memory, Constructive's open-source agentic-db ([n1n.ai comparison](https://explore.n1n.ai/blog/ai-agent-memory-comparison-2026-mem0-zep-letta-cognee-2026-04-23), [Constructive PR](https://www.prnewswire.com/news-releases/constructive-open-sources-agentic-db-the-postgres-memory-layer-for-ai-agents-302755269.html)). The schema is not a business model — it is a *cognitive substrate*: facts, episodes, relations, tool-call traces, summarizations.

### 1d. Self-modifying schemas (the agent ALTERs its own tables)

A-Mem (arXiv 2502.12110) explicitly argues that "graph databases provide structured organization for memory systems but their reliance on predefined schemas fundamentally limits their adaptability" ([A-Mem](https://arxiv.org/pdf/2502.12110)). Agentic memory systems are increasingly designed to dynamically create new tables/columns/edges as new entity types are encountered. Letta's "self-editing memory" model means the agent decides what knowledge to preserve and edits its own memory blocks via tools.

### 1e. Per-agent vs shared multi-tenant agent DBs

Two architectural patterns are competing. (i) **Database per agent / per branch** — Neon's branching, TigerData's zero-copy forks, Dolt's branches. (ii) **Shared multi-tenant agent infra** with logical isolation via row-level filters. The per-agent pattern wins on isolation but creates a fleet-management problem (potentially thousands of small DBs per organization). The shared pattern wins on operational simplicity but is vulnerable to cross-agent leakage and noisy-neighbor effects ([Blaxel multi-tenant for agents](https://blaxel.ai/blog/multi-tenant-isolation-ai-agents)).

**These five categories collide.** A vibe-coded SaaS has an agent-memory DB embedded in it that itself self-modifies, runs across hundreds of Neon branches, one per tenant agent. pg_sage cannot just "support agent DBs" — it has to recognize which combination it is looking at.

---

## 2. Patterns That Diverge from Human-Designed OLTP

Surveying the memory-stack literature, agent-DBs exhibit a recognizable workload signature:

**Schema sprawl.** Many narrow tables (timestamp + value + JSONB) instead of normalized wide tables. TigerData notes the trade-off explicitly: agents lacking full schema knowledge engage in "speculative probing with exploratory micro-queries that create high fan-out of tiny queries" ([arXiv 2512.09548](https://www.arxiv.org/pdf/2512.09548)). Narrow tables are friendlier to a model that doesn't know the column list.

**JSONB as escape hatch.** When the agent sees a new entity field, the path of least resistance is to dump it into a `metadata jsonb` column. This is a known anti-pattern at scale — JSONB queries are slower, storage is larger, and ACID guarantees weaken at the query planner level ([2ndQuadrant](https://www.2ndquadrant.com/en/blog/postgresql-anti-patterns-unnecessary-jsonhstore-dynamic-columns/)). One observed remediation: a nightly job to "explode items into a narrow order_items table" cuts p95 latency from JSONB scan times to BTREE lookups, dropping CPU ~70% ([Medium/Nexumo](https://medium.com/@Nexumo_/7-postgres-jsonb-query-patterns-that-scale-79141d4f8784)).

**Generated columns from embeddings.** The vector lives next to the row, often as a `GENERATED ALWAYS` column or a trigger-maintained sibling table. Re-embedding when the model changes is a recurring chore ([Medium/Lakshmi](https://medium.com/@datascientist.lakshmi/the-hidden-cost-of-embeddings-storage-drift-and-model-staleness-in-vector-dbs-d586a39b969c)).

**High write amplification from tool-call logging.** Production agent observability requires every tool call, every step, every run be addressable and logged. JSONL-on-disk works for the first week; production-scale logging needs trace-id/agent-id composite primary keys and write rates that approach OLAP-style append loads ([dev.to/aishiteru](https://dev.to/aishiteru/how-we-built-agent-observability-at-100k-eventssec-pa1)).

**Skewed read patterns.** Memory recall is overwhelmingly point lookup ("what did the user say about their dog?"); tool-call logs are append-only with the occasional analytical scan; the conversation table is hot for the last N hours per session and ice-cold thereafter. A single DB has three different access shapes on three different tables. Standard tuning advice doesn't apply uniformly.

**Lifecycle blur.** With Neon branches at $0 idle cost, dev/test/prod boundaries dissolve. Agents create "temporary" branches that become permanent because a deployment pipeline never tore them down. Result: branch sprawl. 97% of Neon branches now agent-created means the branch graveyard is large.

**Data quality decay.** Agents write garbage into their own DB and read it back as ground truth — the amplification loop. A-Mem and SimpleMem both call out memory pollution as a primary failure mode; the system effectively confabulates *and persists the confabulation*.

---

## 3. What Goes Wrong in Production

Concrete failure modes I'd watch for:

**Index explosion.** Every "fact" the agent saves gets its own index because the agent (or its scaffolding) doesn't reason about index *budget*. PlanetScale and pganalyze both stress that "if you create too many indexes, writes on busy tables will be slowed down significantly" — and pganalyze argues indexing should be "AI-assisted but developer-driven" precisely because LLMs over-index ([PlanetScale blog](https://planetscale.com/blog/postgres-new-index-suggestions), [pganalyze](https://pganalyze.com/blog/automatic-indexing-system-postgres-pganalyze-indexing-engine)).

**Vacuum overload.** Tool-call logs and memory churn produce dead-tuple rates that shred default autovacuum settings. The 20% scale factor means autovacuum waits for millions of dead tuples on a 10M-row table before triggering; for an agent doing 1k tool calls/min this is catastrophic ([dev.to autovacuum guide](https://dev.to/philip_mcclarence_2ef9475/postgresql-autovacuum-tuning-a-practical-guide-4n48)).

**PII / secret leakage into agent memory.** Agents memorize and regurgitate. "LLMs are fundamentally non-forgetful and non-auditable" — secrets accumulate in long-term memory that may persist indefinitely ([Doppler](https://www.doppler.com/blog/advanced-llm-security)). The classical SQL-injection mitigations don't help because the data was *willingly written* by the agent itself.

**Memory poisoning.** MINJA (Memory Injection Attack) demonstrates that regular users with no elevated privileges can poison an agent's long-term memory through query-only interactions, with the malicious instructions surviving across sessions and triggering days later ([arXiv 2503.03704](https://arxiv.org/html/2503.03704v2), [Lakera](https://www.lakera.ai/blog/agentic-ai-threats-p1)). The DB is the persistence layer for this attack — and a DB-side observer has the best vantage point for detection.

**Prompt-to-SQL injection (P2SQL).** Distinct from classical SQLi: the attacker doesn't inject SQL syntax, they inject natural-language instructions that the agent then converts to SQL. CVE-2026-42208 against LiteLLM (CVSS 9.3, exploited within 36 hours of disclosure) is the canonical example ([Hacker News](https://thehackernews.com/2026/04/litellm-cve-2026-42208-sql-injection.html), [Keysight](https://www.keysight.com/blogs/en/tech/nwvs/2025/07/31/db-query-based-prompt-injection)).

**Cost explosions.** A runaway agent loop kicks off 100k embedding calls or scans the entire DB at $0.0001/row. Portal26 launched an entire product just for "Agentic Token Controls to cap runaway AI agent spend" — the market has decided this is a category ([SiliconAngle](https://siliconangle.com/2026/04/23/portal26-launches-agentic-token-controls-cap-runaway-ai-agent-spend/)).

**Stale embeddings after re-index.** When the embedding model upgrades, every vector in the DB is now in a different geometric space than incoming queries. Silent precision degradation. There is no error message — the agent just gets worse over time.

**Concurrent agent stomping.** Dolt reports that agents using SQLite's optimistic locking "struggled at more than four concurrent agents," and that without versioning, agents become "unrecoverably confused" when encountering deleted/modified rows from other agents ([DoltHub](https://www.dolthub.com/blog/2026-03-13-multi-agent-persistence/)). 160 concurrent agents per host on Dolt vs ~4 on SQLite is the magnitude of the difference.

**Agent identity blur.** With shared connection pools and generic `application_name`, you can't answer "which agent issued this query?" — the prerequisite for any per-agent quota, audit, or kill switch ([EDB](https://www.enterprisedb.com/blog/getting-most-out-applicationname)).

---

## 4. Existing Tooling and the Gaps

**Schema migration tools (Atlas, Sqitch, Liquibase, Flyway, Bytebase).** Atlas has a "Database Schema Migration Skill for AI Agents" ([Atlas Guides](https://atlasgo.io/guides/ai-tools/agent-skills)) and a drift detection module that diffs declared vs actual schema. Bytebase ships AI-powered change management with approval workflows. **Gap:** these tools assume a pre-declared "desired state" that the human controls. When the agent IS the change author, who declares desired state? The human reviews after-the-fact, often without understanding the diff.

**Online schema change (pgroll, pg-osc).** Both make schema changes safe. pgroll is reversible and supports parallel old/new schemas ([pgroll](https://pgroll.com/)). **Gap:** these are mechanisms, not policies. They don't decide *whether* to apply an agent's DDL.

**Versioned databases (Dolt, Doltgres).** Git-for-data with branch/merge/diff. Used in production to support 600 concurrent agents ([Dolt blog](https://www.dolthub.com/blog/2026-03-13-multi-agent-persistence/)). **Gap:** it's a separate engine, not Postgres. Adoption requires forklift migration and Postgres compatibility is incomplete.

**Branching (Neon, TigerData/Tiger Postgres).** Copy-on-write zero-cost branches. Neon spins a branch in 500ms; TigerData "Fluid Storage" delivers instant forks ([Neon for AI agents](https://neon.com/use-cases/ai-agents), [TigerData fast forks](https://www.tigerdata.com/blog/fast-zero-copy-database-forks)). **Gap:** branches solve isolation, not safety inside a branch. An agent can still corrupt its own branch.

**Agent memory frameworks (Letta, Mem0, Zep, Cognee, agentic-db).** These define schemas and provide retrieval APIs ([Constructive agentic-db](https://www.prnewswire.com/news-releases/constructive-open-sources-agentic-db-the-postgres-memory-layer-for-ai-agents-302755269.html)). **Gap:** they don't observe their own DB's *operational health*. They tell you the agent's facts are stored — they don't tell you autovacuum is falling behind, or that an index never gets used, or that one tenant's memory table is eating 80% of write IOPS.

**Schema-as-code intelligence (dryrun).** Reads a JSON schema snapshot from git, scores AI-generated SQL against it without DB connection ([boringSQL](https://boringsql.com/posts/dont-let-ai-to-prod/)). **Gap:** it's static analysis. It can't see runtime workload effects.

**Token/cost guardrails (Portal26, MLflow Gateway, Runyard).** Cap runaway agent spend at the LLM layer ([MLflow](https://mlflow.org/blog/agent-costs-mlflow-gateway), [Runyard](https://runyard.io/blog/swarm-budgets-cost-control)). **Gap:** they meter tokens, not DB cost. 100k embedding writes is cheap on tokens, expensive on storage and vacuum.

**The white space:** an agent-aware operational DBA that bridges the runtime (vacuum, index, locks, plans) with agent-specific context (which agent, which session, what tool, what cost). pg_sage sits exactly there.

---

## 5. pg_sage's Concrete Opportunity

These are features that fit pg_sage's existing architecture (advisor → optimizer → executor with trust ramp) and address the gaps above. Ordered by leverage.

**A. Agent-flavored workload detection.** Add a `WorkloadFingerprint` analyzer that classifies a database as agent-flavored when it sees enough of: heavy short-vector inserts, narrow-table dominance, JSONB column ubiquity, append-only log tables, low query diversity but high invocation rate, embedding-table churn, frequent tiny ALTERs. Surface this as a top-line property of the database; downstream advisors and the LLM router should know "this is an agent DB" because optimal advice differs (e.g., aggressive autovacuum is correct here; conservative might be correct on a human OLTP).

**B. Schema-drift tracker for self-modifying schemas.** When the agent ALTERs its own tables, surface the diff and a risk score before/after the change has landed. Atlas already does state-vs-desired diffs but only against a declared spec; pg_sage's variant uses *historical* state as the implicit spec — anomaly detection over schema events from `pg_event_trigger`. Score each change on (i) blast radius (is this a hot table?), (ii) safety (is this a column drop?), (iii) reversibility, (iv) novelty (have we seen this pattern before?). When score is high, post a finding instead of letting it pass silently.

**C. Per-agent identity correlation.** Recommend / enforce `application_name` conventions: `agent:<agent_id>:<session_id>:<tool_call_id>`. Then surface every pg_sage finding with the agent who caused it. This is the prerequisite for every other agent-aware feature; without it pg_sage is operating blind on the most important dimension. Cheap to implement, immediately useful, and unlocks per-agent quotas later.

**D. Runaway-agent isolation.** Detect a single `application_name` consuming N% of locks/IOPS/CPU and rate-limit it before it OOMs the DB. This is the database-side dual to Portal26's token caps. Modes: monitor (alert), advisory (suggest), auto (kill query / reduce statement timeout for that client). Slots into pg_sage's existing trust ramp.

**E. Memory hygiene.** Stale-embedding detection (vector column has data created with a model version that's been deprecated), orphan-row detection (memory rows referenced by no session and last-accessed > 90 days), dedup detection (cosine similarity > 0.98 across rows for the same agent). These are findings, not actions; the executor only runs them in advisory or auto mode.

**F. Cost guardrails surfaced as findings.** "Agent X has caused 12M embedding writes this week, projected $4,200 storage at end of month." This is operationally cheaper to compute from `pg_stat_statements` than from the LLM gateway because pg_sage already lives in the DB. Surface the budget delta even if pg_sage can't enforce it.

**G. Schema-review-for-agents (DDL gate).** Before an agent's DDL is applied, pg_sage scores it: missing indexes on FK, type mismatches, unbounded TEXT, ill-advised JSONB. Three modes: comment-only, block-with-override, auto-fix (e.g., adding the FK index alongside). This is the killer feature for vibe-coded DBs because the failure is always the same shape.

**H. Memory-poisoning detection.** Watch INSERT patterns into memory tables for known injection markers: zero-font/CSS-hidden-text patterns (after sanitization), unusual jumps in average row size, sudden bursts of write from one user_id, content with imperative-instruction shape. This is novel — no current tool watches the *DB* for poisoning, only the LLM input/output. pg_sage's vantage is unique here.

**I. Observability of self.** pg_sage IS an agent operating on a DB. Dogfooding: pg_sage should issue every query under its own `application_name=pg_sage:<correlation_id>`, write every action to the audit table, and apply its own findings to itself. This is the credibility play — "we run pg_sage on pg_sage."

**J. Agent-DB lifecycle.** Detect orphan branches (Neon-style: branches with zero connections in 30 days, zero data divergence from parent), suggest cleanup. Only useful in fleet mode. Cheap to compute, hard for users to do manually.

---

## 6. Top 5 Bets pg_sage Should Make

1. **Per-agent identity is the foundation.** Without `application_name` correlation, every other agent feature is performative. Ship this first; it's small and unblocks everything else. Every finding should have an `agent_id`, `session_id`, `correlation_id` field where available.

2. **DDL gate is the highest-leverage single feature.** Vibe-coded DBs ship broken schemas every day, and the failure modes are stereotyped (FK without index, missing NOT NULL, unbounded TEXT, JSONB-as-escape-hatch). A static DDL scorer that runs on `ddl_command_end` event triggers and posts a finding catches >70% of these before they bite. This is the feature competitors don't have because their target is "DBA running migrations," not "agent emitting DDL."

3. **Workload fingerprinting unlocks tone.** A two-bit classifier — `is_agent_flavored`, `is_memory_store_flavored` — flips pg_sage's defaults: more aggressive autovacuum, different index thresholds, attention to embedding columns, tool-call log partitioning recommendations. The fingerprint also rewires the LLM router prompt: "this is an agent memory DB, prioritize memory-hygiene findings." Same engine, different posture.

4. **Memory hygiene is the moat.** Atlas does drift, Neon does branches, Bytebase does approvals, Dolt does versioning — none of them watch the *content quality* of the agent memory itself. Stale embeddings, orphan rows, poisoning markers, dedup — all of this is in pg_sage's natural scope (it's already doing query-level analysis) and nobody else is doing it. This is the differentiated bet.

5. **Dogfood publicly.** Run pg_sage on pg_sage's own metadata DB and surface the findings in the marketing site. The credibility argument is "we are an agent operating a DB and these are the findings we generate against ourselves." This is also a forcing function: anything pg_sage can't catch about itself is a feature gap.

---

## 7. Questions You Should Be Asking

These are questions I don't think the spec or roadmap has confronted yet. Listed in order of "I'd want answer this week" to "I'd want this answered before v1.0."

1. **Should pg_sage offer a write path agents call instead of letting them issue raw SQL?** A `pg_sage.remember(agent_id, key, value, ttl, sensitivity)` API is fundamentally safer than the agent crafting INSERTs against a memory table. It also gives pg_sage the metadata it needs (agent_id, sensitivity tier) for free. Risk: this is a new product surface. Reward: it eliminates entire categories of failure (no SQLi possible if the agent never speaks SQL).

2. **What is the authn model when the agent is the operator?** Today, an "agent" is usually a service account with broad permissions. Should pg_sage encourage row-level security policies keyed on `application_name`-derived agent IDs? Should it ship with templates for "agent role"?

3. **Should pg_sage treat each Neon branch as a distinct database in fleet mode, or as a versioned copy of one logical database?** This decision determines whether 1000 branches show up as 1000 dashboard rows (overwhelming) or as one row with a "1000 branches" pivot (loses isolation).

4. **What's the right TTL story for agent memory?** Memory poisoning research recommends TTL on every memory entry. Should pg_sage advise (or auto-create) TTL policies for tables it identifies as memory tables? `pg_partman` + retention policy is one path; a pg_sage-managed `expire_at` column is another.

5. **How does pg_sage behave when the agent IS pg_sage?** When pg_sage is itself the runaway query, who detects it? The "self-observation" feature is more than a marketing exercise — it's a bootstrapping problem. Solution candidates: a watchdog mode that runs in a separate connection with low-privilege; a kill-switch that any operator can flip externally.

6. **What's the right granularity of "trust" in trust ramp for agent DBs?** Today trust is per-DB (monitor/advisory/auto). For agent DBs, the right axis might be per-table (memory tables: auto-vacuum aggressively; conversation tables: never touch). Worth modeling now before the trust ramp ossifies.

7. **Do we lean into branching as a primitive?** If 97% of branches on Neon are agent-created, pg_sage operating in fleet mode against Neon will see thousands of branches per customer. Should pg_sage *recommend* spinning up a branch to test a destructive change before applying it (analogous to dry-run, but with real data)? This makes pg_sage's executor markedly safer.

8. **Should pg_sage fingerprint specific memory frameworks?** Recognizing "this is a Letta archival_memory table" or "this is a Mem0 messages table" lets pg_sage ship framework-specific advice (e.g., Letta's recall_memory should be partitioned by month). Costs: tracking moving targets. Benefits: dramatically better advice.

9. **How does pg_sage handle agent-created tables that *are not* in any migration history?** Most of the schema-management world assumes a migration directory. An agent that ALTERs at runtime leaves no migration. Does pg_sage reverse-engineer migrations from event triggers? Materialize them so a human can review on demand?

10. **What's the kill-switch UX when an agent has gone rogue?** The DB-side circuit breaker has to exist *somewhere*. Is it a pg_sage HTTP endpoint? A trigger that watches for `pg_sage_emergency_stop=true`? The answer determines whether pg_sage is observability or control plane — and that's a strategic decision.

---

## Sources

- [Constructive Open Sources Agentic DB](https://www.prnewswire.com/news-releases/constructive-open-sources-agentic-db-the-postgres-memory-layer-for-ai-agents-302755269.html)
- [Databricks: 19% Have Deployed Agents, But They're Creating 97% of Databases (SaaStr)](https://www.saastr.com/databricks-only-19-of-organizations-have-deployed-ai-agents-but-theyre-already-creating-97-of-databases/)
- [Tiger Data: Postgres for Agents](https://www.tigerdata.com/blog/postgres-for-agents)
- [Tiger Data: Fast, Zero-Copy Database Forks](https://www.tigerdata.com/blog/fast-zero-copy-database-forks)
- [Neon for AI Agent Platforms](https://neon.com/use-cases/ai-agents)
- [Dolt: Multi-Agent Persistence](https://www.dolthub.com/blog/2026-03-13-multi-agent-persistence/)
- [boringSQL: Don't Let AI Touch Your Production Database](https://boringsql.com/posts/dont-let-ai-to-prod/)
- [Mem0 vs Zep vs Letta vs Cognee 2026](https://explore.n1n.ai/blog/ai-agent-memory-comparison-2026-mem0-zep-letta-cognee-2026-04-23)
- [A-Mem: Agentic Memory for LLM Agents](https://arxiv.org/pdf/2502.12110)
- [MINJA: Memory Injection Attack on LLM Agents](https://arxiv.org/html/2503.03704v2)
- [Lakera: Memory Poisoning & Long-Horizon Goal Hijacks](https://www.lakera.ai/blog/agentic-ai-threats-p1)
- [Doppler: Preventing Secret Leakage Across Agents](https://www.doppler.com/blog/advanced-llm-security)
- [Hacker News: LiteLLM CVE-2026-42208 SQL Injection](https://thehackernews.com/2026/04/litellm-cve-2026-42208-sql-injection.html)
- [Keysight: Database Query-Based Prompt Injection](https://www.keysight.com/blogs/en/tech/nwvs/2025/07/31/db-query-based-prompt-injection)
- [Atlas Schema Migration Skill for AI Agents](https://atlasgo.io/guides/ai-tools/agent-skills)
- [Atlas Drift Detection](https://atlasgo.io/monitoring/drift-detection)
- [pgroll: Zero-Downtime Migrations](https://pgroll.com/)
- [pg-osc: Online Schema Change](https://www.mydbops.com/blog/revolutionizing-postgresql-schema-changes-with-pg-osc)
- [PlanetScale: AI-Powered Postgres Index Suggestions](https://planetscale.com/blog/postgres-new-index-suggestions)
- [pganalyze: Automatic Indexing Engine](https://pganalyze.com/blog/automatic-indexing-system-postgres-pganalyze-indexing-engine)
- [pganalyze: VACUUM Advisor](https://pganalyze.com/blog/introducing-vacuum-advisor-postgres)
- [Portal26: Agentic Token Controls](https://siliconangle.com/2026/04/23/portal26-launches-agentic-token-controls-cap-runaway-ai-agent-spend/)
- [How We Built Agent Observability at 100K Events/Sec](https://dev.to/aishiteru/how-we-built-agent-observability-at-100k-eventssec-pa1)
- [Hidden Cost of Embeddings: Storage, Drift, Staleness](https://medium.com/@datascientist.lakshmi/the-hidden-cost-of-embeddings-storage-drift-and-model-staleness-in-vector-dbs-d586a39b969c)
- [arXiv: Supporting Dynamic Agentic Workloads](https://www.arxiv.org/pdf/2512.09548)
- [Blaxel: Multi-Tenant Isolation for AI Agents](https://blaxel.ai/blog/multi-tenant-isolation-ai-agents)
- [56kode: Moving from Cursor to Claude Code](https://www.56kode.com/posts/moving-from-cursor-to-claude-code/)
- [EDB: Getting the Most Out of application_name](https://www.enterprisedb.com/blog/getting-most-out-applicationname)
- [2ndQuadrant: PostgreSQL Anti-Patterns: Unnecessary JSON/Hstore](https://www.2ndquadrant.com/en/blog/postgresql-anti-patterns-unnecessary-jsonhstore-dynamic-columns/)
- [Threads/Bevzenko: 80% of Neon DBs Created by AI Agents](https://www.threads.com/@sergebevzenko/post/DSZcIv0iiuO/of-databases-on-neon-serverless-postgre-sql-used-by-open-ai-replit-vercel-are)
