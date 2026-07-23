# pg_sage — Feature Research & Specification

**Prepared:** 2026-07-22 · **Basis:** direct codebase read (post-audit) + cited web research · **Output:** research report + buildable specs, no code.

> Executor note: this brief was labeled "For execution by GPT 5.6 Sol." It was executed in a Claude Code session with web-search tools and full repository access. Where the brief's summary of pg_sage conflicts with the code, the code wins (see §6, the MCP finding).

---

## 1. Executive summary

**Thesis.** Postgres is the one major RDBMS with *no native verify-and-revert autonomy loop*. Oracle (Automatic Indexing) and Azure SQL (Automatic Tuning) both create a change, measure its real effect on the live workload, and **revert automatically on regression** — the single most valuable pattern in this space, and the one thing no Postgres tool does. pg_sage already has every scaffold for it (LLM index optimizer, HypoPG validation, a trust-ramped executor, a rollback monitor). The scaffold's verification is currently a coarse global cache-hit heuristic, so the loop exists but does not actually *verify*. **Closing that loop is the product.**

The second, differentiated lane is the **no-human database**: databases created and written by agents, watched by nobody. Here the deliverable is not "autopilot for humans" — it is *guarantees* (continuously enforced invariants) and *fail-closed autonomy under a standing policy*, because there is no escalation target when the XID clock or the disk is 12 hours from a hard stop.

**Top features (deep-spec'd in §8):**

1. **Verify-and-Revert Index Lifecycle** *(upgrade; rung 3→4)* — port the Azure/Oracle create→validate→retain-or-revert loop to Postgres, replacing the current global-cache-hit heuristic with per-query workload verification. The headline differentiator.
2. **Rehearse-on-Clone Migration Gate** *(new; rung 3)* — expand/contract online DDL, lock-safety linted, rehearsed on a thin clone against captured traffic before it touches production, applied under a lock-timeout budget with cancel=rollback.
3. **Wraparound & Bloat Custodian** *(new/upgrade; rung 4)* — enforce the invariant "no table crosses its XID / freeze / bloat deadline," acting with escalating aggression as the deadline nears. A *guarantee*, grounded in the Sentry and Mandrill wraparound outages.
4. **Replication-Slot & WAL Disk Guardian** *(new, agent-native; rung 3→4)* — enforce "no inactive slot can fill the disk," acting before WAL exhaustion flips the primary to read-only. The classic 2am disk-full, delegated.
5. **Agent-Native Schema Custodian** *(Workstream C; rung 2→3)* — detect and self-remediate agent-generated schema pathologies (FKs with no supporting index, unbounded append tables, random-UUID PK locality), enforced as invariants.
6. **Standing-Policy & Change-Serialization Control Plane** *(Workstream C, upgrade; rung 4 enabler)* — the declarative operating-limits object + DDL serialization/drift reconciliation for multi-writer no-owner databases + fail-closed parking of undecidable changes. Everything rung-3/4 depends on this; it also fixes three trust-gate defects found in the audit.

**Sequencing spine (one line):** Build **#6 first** (policy object + fixed trust gate + evidence ledger — nothing acts autonomously without it) → **#1** (mostly present; earliest differentiation) and the two custodians **#3/#4** (guarantees; need #6, not clones) in parallel → integrate the **thin-clone enabler**, which unlocks **#2** and deepens #1/#5 → **#5** (needs #6's serialization) → fleet staged-rollout (deferred, §7 F7).

**The tension you asked me to flag (§2):** the roadmap is drifting toward observability the filter rejects — "historical trend analysis," "query plan diffing / alert on regression," "cost per query in the dashboard." Those *tell*; they do not *do*. Worse, **the product removed its only machine-facing interface**: MCP is retired (`config.go` errors "use the REST API instead"), and the REST API is mechanism-level (findings/actions/config), not intent-level. Question 2 (agent-to-agent delegation) has no surface to land on. If the no-human lane is real, an **intent-level MCP surface must be re-introduced**, and the web UI must be scoped to reviewing *decisions and evidence*, not database telemetry.

---

## 2. Assumptions (decisions made without clarification)

- **A1.** "GPT 5.6 Sol" is not available in this environment; I executed the brief myself rather than dispatch it. No agents were spawned.
- **A2.** The thin-clone enabler (Database Lab Engine or a Neon-style branch) is *not yet integrated* in pg_sage (no evidence in the repo). Features that need it are marked enabler-dependent; I assume it is acquirable, not free.
- **A3.** HypoPG is present-but-optional (roadmap confirms); specs degrade gracefully without it.
- **A4.** The "no-human" deployment is real and strategic, not hypothetical — I weight Workstream C accordingly.
- **A5.** pg_sage keeps its architecture: single Go sidecar over the wire protocol, no C extension required, pluggable OpenAI-compatible LLM, trust-ramped executor. Specs respect that (no `shared_preload_libraries` requirement unless explicitly flagged).
- **A6.** Managed-provider constraints (no `ALTER SYSTEM`, no superuser, no filesystem) are in scope — pg_sage already targets RDS/Aurora/Cloud SQL/AlloyDB.
- **A7.** "Verifiable" means *measurable against a pre-declared, objective, per-query or per-invariant criterion*, not a global health score.

---

## 3. Workstream A — DBA toil taxonomy (with autonomy ceiling)

Ceiling = highest rung (§7) an agent can *safely* reach today. Freq/Toil/Judgment are H/M/L. "Verifiable" = can the effect be measured objectively and reverted cleanly.

| Task area | Freq | Toil | Judgment | Verifiable | Ceiling | Agent end-to-end? | Notes / evidence |
|---|---|---|---|---|---|---|---|
| **Missing index** | H | M | M | **Yes** (per-query latency, HypoPG pre-est) | **4** | Yes | The canonical win. Azure/Oracle both do this autonomously with revert. |
| **Redundant/duplicate index** | H | L | L | Yes (writes drop, no read regress) | **4** | Yes | Deterministic detection; safe drop w/ recreate-rollback. |
| **Unused index** | M | L | M | Partial (must observe long window) | **3** | Yes | Azure: 90-day unused window, never drops unique. Copy the window discipline. |
| **Invalid index (failed CONCURRENTLY)** | M | L | L | Yes | **4** | Yes | pg_sage already auto-drops these; pure mechanical cleanup. |
| **Bloated index** | M | M | M | Yes (size, REINDEX CONCURRENTLY) | **3** | Yes | REINDEX CONCURRENTLY is online; verify size + no invalid result. |
| **Online DDL / lock-avoidance** | H | H | H | Yes (rehearse on clone) | **3** | Partial→Yes w/ clone | ACCESS EXCLUSIVE on a hot table is the #1 self-inflicted outage. Squawk rules + expand/contract. |
| **Long backfill** | M | H | M | Yes (batched, measured) | **3** | Yes | Batch size + throttle on replication lag; pgroll does this. |
| **Add NOT NULL / constraint safely** | H | M | M | Yes | **3** | Yes | `NOT VALID` + `VALIDATE CONSTRAINT`; Squawk-linted path. |
| **Column type change** | M | H | H | Partial | **2** | Partial | Often needs expand/contract + app coordination; rehearse, don't auto-apply blind. |
| **Plan regression (post-stats/upgrade)** | H | H | H | Yes (Query Store-style) | **3** | Partial | Postgres has **no plan pinning** — big gap (§6). Mitigate via stats/hints. |
| **Parameter-sensitive plan** | M | H | H | Partial | **2** | Partial | Hard; needs per-parameter capture. Rung-1 recommend at best without extensions. |
| **Missing extended statistics** | M | M | M | Yes (row-estimate error before/after) | **3** | Yes | `CREATE STATISTICS` on correlated predicates; measurable est-error drop. |
| **Stale statistics / ANALYZE** | H | L | L | Yes | **4** | Yes | pg_sage does this; low blast radius. |
| **Per-table autovacuum tuning** | H | M | M | Yes (dead-tuple ratio, bloat trend) | **3→4** | Yes | The steady-state toil sink; feeds the Wraparound Custodian (#3). |
| **Bloat remediation (pg_repack)** | M | H | M | Yes (size before/after) | **3** | Yes (orchestrated) | pg_repack is online but fiddly to orchestrate; strong agent candidate. |
| **XID wraparound risk** | L-but-**catastrophic** | H | M | Yes (age(relfrozenxid)) | **4** | Yes | Sentry (2024) & Mandrill/Mailchimp (~40h outage) both wraparound. Deterministic deadline math. |
| **Long-running txn blocking cleanup** | M | M | M | Yes | **3** | Yes (cancel/terminate w/ evidence) | pg_sage's runaway tracker already does warn→cancel→terminate. |
| **Connection exhaustion** | M | H | M | Partial | **2-3** | Partial | Terminate idle-in-txn offenders (has evidence gate); pooler config is advisory. |
| **Idle-in-transaction offenders** | M | M | L | Yes | **3** | Yes | `idle_in_transaction_session_timeout` + targeted terminate. |
| **Partitioning: create/drop on schedule** | M | M | L | Yes | **4** | Yes | Deterministic calendar; attach/detach without long locks. A guarantee-shaped task. |
| **Deciding when/how to partition** | L | H | H | Partial (rehearse) | **2** | Partial | Judgment-heavy; recommend + rehearse. |
| **Replication lag** | M | M | M | Partial | **2** | Partial | Often external cause; act = throttle backfills, not fix the replica. |
| **Replication slot WAL retention** | M-but-**catastrophic** | M | M | Yes (slot age, WAL bytes) | **3-4** | Yes | Inactive slot → disk full → read-only. `max_slot_wal_keep_size`; drop provably-abandoned slots. |
| **Backup restorability** | M | H | H | **Yes** (test restore) | **3** | Yes | Backups are monitored; *restores* aren't. "Verified restore in last N days" is a guarantee. |
| **Minor version patching** | M | M | L | Yes | **3** | Partial | Rehearse + staged; provider-gated on managed. |
| **Major upgrade + plan regression sweep** | L | H | H | Yes (post-upgrade sweep) | **2** | Partial | High blast radius; recommend + rehearse, human/second-agent gate. |
| **Role/grant sprawl, least-privilege drift** | M | M | M | Yes (diff vs intended) | **3** | Yes | Drift reconciliation; guarantee-shaped. |
| **Credential rotation** | M | M | L | Yes | **3** | Yes | Mechanical; coordinate with dependents. |
| **RLS policy correctness** | L | H | H | Partial | **2** | No (auto) | Security-critical; **do not auto-apply** (see §3 no-act list). |
| **Retention / TTL / unbounded growth** | H | M | L | Yes | **4** | Yes | Agent-native: agent-written tables rarely have retention. Enforce it. |
| **Config drift** | H | M | M | Yes (verify-and-revert) | **3-4** | Yes | Every "temporary" change becomes permanent; reconcile to intended. |
| **Incident: lock storm / runaway** | M | H | H | Yes | **3** | Yes | Triage + mitigate + postmortem artifact; pg_sage has the runaway ladder. |
| **Disk full (any cause)** | M-**catastrophic** | H | M | Yes | **3-4** | Yes | Predict + act (slot, bloat, retention) before the wall. |

**Where an agent should NOT act autonomously (and why):**
- **RLS / grant *expansion* / `SECURITY DEFINER` changes** — a wrong call is a silent data-exposure breach, not a perf regression; not cleanly reversible (data may already have leaked). Recommend only; require a human or a second, differently-authorized agent.
- **Major version upgrades** — blast radius = the whole instance, rollback = restore-from-backup (expensive, lossy). Rehearse + gate.
- **Dropping any object that could hold the only copy of data** (tables, non-duplicate unique indexes, replication slots that *might* be live) — fail closed; require positive proof of non-use over a long window (Azure's 90-day rule is the model).
- **Anything unrollbackable** as a class — the standing policy (#6) must mark these and refuse them at rungs 3-4.

**Prioritization signal:** high-frequency × high-toil × mechanically-verifiable = index lifecycle, autovacuum/bloat/wraparound, retention, config drift, online DDL. Those are where the agent wins first; they map onto features #1–#5.

---

## 4. Workstream B — Competitive matrix, mechanisms, gaps, and the failures

### 4.1 Matrix (does it *act*, *verify*, *revert*?)

| System | Domain | Acts (not just recommends)? | Verifies outcome? | Reverts on regression? | Trigger | Source |
|---|---|---|---|---|---|---|
| **Azure SQL Automatic Tuning** | index create/drop, plan force | **Yes** (autonomous mode) | **Yes** | **Yes, immediately on regression** | continuous; applies at low CPU/IO | [MS Learn](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql) |
| **Oracle Autonomous DB — Auto Indexing** | index lifecycle | **Yes** | **Yes** (forces candidate, compares stats) | **Yes** (marks unusable/drops) | background every 15 min | [Oracle docs](https://docs.oracle.com/en/database/oracle/oracle-database/19/arpls/DBMS_AUTO_INDEX.html), [ORACLE-BASE](https://oracle-base.com/articles/19c/automatic-indexing-19c) |
| **SQL Server Auto Plan Correction** | plan force | **Yes** | **Yes** | **Yes** | Query Store regression | [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/automatic-tuning/automatic-tuning?view=sql-server-ver17) |
| **AWS DevOps Guru for RDS / Perf Insights** | anomaly detection | **No** — recommend only | No | No | anomaly/threshold | [AWS docs](https://docs.aws.amazon.com/devops-guru/latest/userguide/working-with-rds.analyzing.recommend.html) |
| **pganalyze Indexing Engine** | index advice | No (advises) | Pre-est via generic plans | N/A | continuous workload | [pganalyze](https://pganalyze.com/docs/indexing-engine/cp-model) |
| **Postgres.ai / Database Lab** | thin clones, migration CI | Enables rehearsal | Yes (real plans on clone) | N/A (pre-prod) | CI / on-demand | [postgres.ai](https://postgres.ai/products/database-migration-testing) |
| **pgroll (Xata)** | online schema migration | **Yes** (applies) | Structural (dual-schema views) | **Yes** (cancel = rollback) | operator/CI | [xata.io](https://xata.io/blog/pgroll-internals) |
| **PlanetScale Deploy Requests** | online DDL (MySQL) | **Yes** | Workflow-gated | **Yes, 30-min revert window** | deploy request | [planetscale](https://planetscale.com/docs/vitess/schema-changes/deploy-requests) |
| **HypoPG** | hypothetical index | No (estimator) | Planner cost only | N/A | on-demand | [readthedocs](https://hypopg.readthedocs.io/en/rel1_stable/hypothetical_indexes.html) |
| **Dexter** | PG auto-index | Can create | HypoPG est | No | continuous | (rung-1 tool; never climbed) |
| **Squawk** | migration lint | No (blocks in CI) | Static | N/A | pre-merge | [squawkhq](https://squawkhq.com/docs/rules) |
| **pgHero / pgwatch / pganalyze dashboards** | observability | **No** | No | No | — | (the anti-goal) |

### 4.2 Mechanism extractions (the transferable parts)

- **Azure verify-and-revert (the crown jewel).** Autonomous mode "automatically validates there exists a positive gain … and if there's no significant performance improvement detected or if performance regresses, the system automatically reverts." **Validation window: 30 min – 72 h**, longer for infrequent queries; "if at any point during validation a regression is detected, changes are reverted immediately." Changes are **applied only at low CPU/Data IO/Log IO**; the system can self-disable to protect the workload. History retained 21 days. **Critical caveat:** when a recommendation is applied *manually via T-SQL*, the verify-and-revert machinery *does not run* — autonomy and verification are coupled. **Transfer:** pg_sage's executor must own both the apply *and* the measurement window, apply in low-load windows, and treat "no significant gain" as a revert trigger, not just "regression." ([source](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql))
- **Oracle candidate→validate→retain lifecycle.** Candidates created **INVISIBLE + UNUSABLE** (metadata only); the auto-index task **runs the workload's SQL with the candidate forced and compares real execution statistics**; if I/O or elapsed time improves past a threshold → **VISIBLE**, else → dropped; unused visible indexes dropped after 373 days. **Transfer:** HypoPG is pg_sage's "invisible candidate," but Oracle goes further — it *forces the real index and measures real execution*. pg_sage should mirror the two-stage gate: HypoPG pre-estimate (cheap) → build CONCURRENTLY → measure real per-query effect → retain or revert. ([source](https://oracle-base.com/articles/19c/automatic-indexing-19c))
- **pganalyze CP-SAT index selection.** Decompose queries into scans, enumerate candidate indexes, cost each with the planner on a **generic plan**, then a **constraint-programming (CP-SAT) solver** picks the index *set* that balances read gain vs. write overhead. **Transfer:** replace/augment pg_sage's LLM-only index generation with a deterministic candidate-set optimizer; use the LLM for the *judgment* layer (is this workload write-heavy enough to skip?), not the arithmetic. ([source](https://pganalyze.com/docs/indexing-engine/cp-model))
- **pgroll expand/contract.** Two schema versions coexist as **views over the physical table**; clients pick a version via `search_path`; data back-filled in **batches** into hidden columns; **cancel = rollback** because the old version stayed live. **Transfer:** this is the migration execution engine for feature #2; it makes DDL rollback cheap and online. ([source](https://xata.io/blog/pgroll-internals))
- **PlanetScale deploy-request workflow.** Branch → change on branch → deploy request → online DDL (copy + keep-in-sync) → **30-minute post-deploy revert button** that restores dropped objects and retains subsequent data changes. **Transfer:** the *workflow shape* — a reviewable change artifact plus a bounded post-apply revert window — is exactly pg_sage's decision-artifact + auto-revert model, validated commercially. ([source](https://planetscale.com/blog/instant-deploy-requests))
- **Database Lab thin clones.** 1 TiB clone in ~10 s, fully writable, "same data … same query plans"; dozens of clones per host; CI **DB Migration Checker**. **Transfer:** the rung-2 rehearsal substrate for #2 and stronger #1 verification (measure on a clone with real data before prod). ([source](https://github.com/postgres-ai/database-lab-engine/blob/master/README.md))
- **Squawk rule set.** Flags ADD UNIQUE (ACCESS EXCLUSIVE), CREATE INDEX without CONCURRENTLY, ADD COLUMN NOT NULL/DEFAULT (rewrite), destructive renames; prescribes `lock_timeout`. **Transfer:** a deterministic lock-safety linter is the *gate* in front of every autonomous DDL; pg_sage has partial equivalents (migration risk cases) — formalize them as a rule engine. ([source](https://squawkhq.com/docs/rules))

### 4.3 Gap list (the primary feed into ideation)

**Exists elsewhere, no Postgres equivalent:**
- **Verify-and-revert autonomy loop** (Azure/Oracle) → *no Postgres product does this.* ← the headline gap (feature #1).
- **Native plan management / plan pinning** (SQL Server Query Store + FORCE_LAST_GOOD_PLAN, Oracle SQL Plan Management) → Postgres has none. A sidecar can approximate only via `pg_hint_plan`, forced re-planning, or statistics manipulation — be honest about the ceiling (rung 2-3, not 4).
- **First-class deploy-request revert window** (PlanetScale) → Postgres migration tools (pgroll) support rollback but not a managed, measured, time-boxed post-apply auto-revert tied to workload metrics.
- **Managed autonomous index drop with long unused-window discipline** (Azure 90-day) → Postgres tools recommend drops; none run the long-window safety loop autonomously.

**Exists nowhere (green field):**
- **Agent-generated-schema remediation** — no product targets the specific pathologies agents produce (feature #5).
- **Multi-writer, no-owner DDL serialization + drift reconciliation** (feature #6).
- **Autonomous restore-verification as a continuous guarantee** ("a restore has succeeded within N days") — everyone monitors backup success, nobody proves restorability continuously.
- **Fail-closed autonomy with no escalation target** — every "autonomous" product assumes a human eventually reviews; none specify degraded-state behavior when nobody will (feature #6, §8).
- **Cross-instance fleet learning with blast-radius-bounded staged rollout** for thousands of ephemeral agent DBs (§7 F7).

### 4.4 Why it failed / stalled (≥3, as required)

1. **OtterTune (2020–2024) — ML knob tuning.** Proximate cause was a collapsed acquisition (a PE firm "backed out"), but the deeper lesson is stickiness: community and post-mortem accounts describe a product users *tried but didn't depend on* — "not sticky enough," retention and integration cost, and a hard-to-verify value prop (a knob change's benefit is workload-specific and slow to prove). **Lesson for pg_sage:** knob tuning as a *headline* is a corpse. The missing ingredient was *per-instance verification and reversibility* — which is precisely why pg_sage must lead with verify-and-revert, not knob ML. ([dbtune](https://www.dbtune.com/blog/ottertune), [dang.ai](https://dang.ai/tool/ai-database-optimizer-ottertune))
2. **The "recommend-only" ceiling (AWS DevOps Guru for RDS / Performance Insights).** Not a commercial failure — a *stall*. Enormous telemetry and ML anomaly detection that **never acts**: "you must decide whether to implement the recommendations." It is the anti-goal at industrial scale: sophisticated *telling* that ends at a human. **Lesson:** observability, however good, does not eliminate toil; the value is in crossing from recommend to act-and-verify. ([AWS docs](https://docs.aws.amazon.com/devops-guru/latest/userguide/working-with-rds.analyzing.recommend.html))
3. **Academic "self-driving database" research (Peloton / autonomous DBMS tuning).** A decade of research (CMU Peloton, the OtterTune papers) produced impressive demos but little durable autonomous product in Postgres. The gap between "the model recommends a good config" and "an operator trusts it to change production unattended" was never closed by accuracy alone — it needed verification, blast-radius bounds, and reversibility, the exact scaffolding the research under-weighted. **Lesson:** trust is an engineering property (verify + revert + audit), not a model-quality property. ([VLDB OtterTune paper](https://www.vldb.org/pvldb/vol11/p1910-zhang.pdf))
- *Honorable mention (rung-1 tool that never climbed):* **Dexter** — a clean open-source Postgres auto-indexer using HypoPG, but it recommends/creates without continuous verification or revert and never became a managed autonomous service. Confirms the pattern: index *suggestion* is solved; index *lifecycle ownership* is not.

---

## 5. Workstream C — the agent-native (no-human) problem set

The differentiated lane. Each item below is a *distinct requirement*, not autopilot-for-humans.

- **Agent-generated schema pathologies (detectable, remediable).** Recognizable anti-patterns from LLM/agent-authored DDL: (a) no foreign keys, or FKs with **no supporting index** (every FK delete/update scans the child); (b) **everything `text`** (no domain constraints, no type safety, planner mis-estimates); (c) **random-UUID primary keys** destroying B-tree locality and inflating WAL (vs. UUIDv7/time-ordered); (d) no indexes, or an index on every column (write amplification); (e) **unbounded append-only tables with no retention** (the silent disk-filler); (f) missing NOT NULL / CHECK constraints the app assumes; (g) denormalization without cause; (h) N+1-shaped access patterns visible in `pg_stat_statements`. → **Feature #5.** Remediation the agent performs itself: add FK-supporting indexes (CONCURRENTLY), propose/enforce retention, add validated constraints via `NOT VALID`→`VALIDATE`, recommend PK migration (rehearsed, not auto — locality change rewrites the table).
- **Multi-writer schema drift (no arbiter).** Several agents issue DDL against one database, no owning human. Requires: (1) a declared **intended schema state** (a versioned baseline); (2) **change serialization** — a DDL lease/queue so two agents don't race conflicting migrations (Postgres advisory locks + a `sage.schema_change` ledger); (3) **conflict detection** (incoming DDL vs. in-flight/queued); (4) **drift reconciliation** — periodic diff of live schema vs. intended, with the agent either reconciling (safe, additive) or **parking** (destructive/ambiguous) the delta. → part of **Feature #6**.
- **Escalation with no escalation target.** Rungs 3-4 normally assume review-eventually. Specify the *no-reviewer* posture: (a) a **fail-safe default** = fail closed (refuse the change, keep the database safe-but-suboptimal); (b) an explicit **refusal set** (unrollbackable ops, security-sensitive ops, anything above the standing budget); (c) **park-and-degrade** — record a durable "decision I could not make" artifact and hold; (d) a **degraded-state clock** — how long the agent may hold a parked-degraded state before it must force a *stop-the-bleeding* safe action (e.g., extend the maintenance window, or take a bounded protective action like throttling writes) rather than let a deadline (XID, disk) pass. → **Feature #6**.
- **Standing policy & budgets (in place of approval).** A declarative object: allowed change classes, maintenance windows, **lock-duration ceilings**, storage/spend caps, **blast-radius limits** (max rows rewritten, max tables touched per window), **rate limits on self-initiated change** (anti-oscillation). Enforced at the executor gate. → **Feature #6** (see §8 for the object shape).
- **Evidence for a reviewer who may never arrive.** Every autonomous action leaves a **durable, self-contained record**: trigger, evidence (the metrics/plan that justified it), alternatives considered and rejected, action taken (exact SQL), rehearsal result, measured effect, rollback availability + cost. Retention outlives the action. This is the audit surface — and the *correct* job for a web UI (review decisions, not telemetry). pg_sage's `sage.action_log` + Cases + Shadow Mode are the seed; formalize into an **Evidence Ledger** (cross-cutting requirement, surfaced in every feature's "Evidence artifact" section).
- **Agent-to-agent delegation (MCP).** The consumer is another agent. Requires an **intent-level** surface: the caller expresses *intent* ("make this query fast," "keep this table's writes under Xms," "this table is append-only, enforce retention") not *mechanism* ("CREATE INDEX ..."). pg_sage decides the mechanism, gates it through the same trust/policy path, and returns machine-consumable results (decision + evidence + rollback handle). **Calling-agent claims are untrusted input, never authorization** — a caller saying "I approve autonomous mode" grants nothing; authority comes only from the standing policy. **⚠ Blocking gap:** MCP is *retired* in the current repo (§6); this requirement has no surface today.
- **Fleet reality (many, ephemeral, heterogeneous).** Per-instance **policy inheritance** (fleet default → tag/class → instance override); **cross-fleet learning** (a fix validated on instance A informs A's cohort — but as a *prior*, re-verified per instance, never blind-applied); **safe staged rollout** (canary a fleet-wide change on k instances, measure aggregate regression, halt on breach); **blast-radius containment** (a bad decision must not fan out to thousands). → §7 F7 (deferred deep-spec; depends on #6).
- **Self-verification (nobody's checking).** Verified restores ("a restore of this DB succeeded within N days"), invariant checks (the guarantees), post-change measurement (the verify loop), and **periodic self-audit** that past autonomous changes still hold (the index it built 30 days ago is still used; the retention it set is still running).

**Three features that only make sense with no human present** (required): **#3 Wraparound Custodian** (a human watches the XID clock in a staffed shop; here nobody does — the guarantee *is* the product), **#5 Schema Custodian** (a DBA would prevent these pathologies at design time; with agent-authors there's no design-time review), and **#6 Standing-Policy Control Plane** (with a human, "approval" is the policy; without one, the declarative policy object *is* the governance). #4 (Slot/WAL Guardian) is nearly in this class too — a staffed shop has disk alerts and a pager; the no-human DB has neither.

---

## 6. Workstream D — existing-feature audit (verified from code)

Grounded in the codebase read (module `github.com/pg-sage/sidecar`) and the prior correctness audit.

### 6.1 Feature-by-feature

| Feature | What it does today (from code) | Ladder now | Anti-dashboard? | Upgrade path |
|---|---|---|---|---|
| **Tier-1 rules engine** (`internal/analyzer`, 20+ checks) | Deterministic findings: dup/unused/missing index, seq scans, bloat, dead tuples, seq exhaustion, repl lag, config drift, security | **0-1** | **Borderline** — findings alone *tell*; passes only because they feed the executor/Cases | Keep as the deterministic substrate; ensure every rule maps to an action or an invariant, not just a finding |
| **Index Optimizer** (`internal/optimizer`, LLM + HypoPG + 8 validators) | Generates index DDL, validates via 8 deterministic checks + HypoPG cost, confidence-scored | **1** (2 w/ HypoPG rehearsal) | **Pass** (produces action + rollback) | → **Feature #1** (add real post-apply per-query verification + auto-revert; add CP-SAT candidate-set selection à la pganalyze) |
| **6 LLM Advisors** (`internal/advisor`) | GUC tuning proposals (vacuum, WAL, conn, mem, rewrite, bloat), doc-range validated | **1** | **Pass** (produce ALTER SYSTEM/DATABASE actions) | Add verify-and-revert for GUC changes (config drift is verifiable); enforce via #6 |
| **Trust-ramped Executor** (`internal/executor`) | OBS→ADVISORY→AUTONOMOUS, typed contracts, risk tiers, rollback monitor, emergency stop | **3** (partial) | **Pass** (the core "do" engine) | → **Feature #6** + fix audit defects (below) |
| **Rollback Monitor** (`rollback.go`) | Post-action window, then **global cache-hit ratio** regression check (per-query F1 only if queryids present) | **3** | Pass | **This is the weak link** — global cache-hit is not per-change verification; #1 replaces it |
| **Runaway Tracker** (`runaway.go`) | warn→cancel→terminate state machine w/ live-evidence revalidation | **3-4** | **Pass** (acts on processes) | Solid; wire into incident postmortem artifact |
| **Cases / Shadow Mode** (`internal/cases`) | Findings→ranked cases w/ why-now + next action; shadow shows avoided toil | **1** (decision artifact) | **Pass** — this is the "PR not panel" model | Promote to the **Evidence Ledger** / approval surface (§5) |
| **Forecaster** (`internal/forecaster`) | Predicts disk/conn/cache/seq/checkpoint pressure | **0** | **FAILS** — output is a prediction for a human | **Redesign:** feed predictions *silently* into the custodians (#3/#4) as triggers; kill the standalone "forecast" as a deliverable |
| **Fleet Mode** (`internal/fleet`) | N DBs, per-DB trust/budget/health | **n/a** (substrate) | n/a | Substrate for §7 F7; audit found several fleet features dead (health history, ANALYZE semaphore) |
| **Alerting** (`internal/alerting`) | Slack/PD/webhook per-severity + cooldown/quiet-hours | **0-1** | **FAILS if terminal** — an alert is *telling* | Keep only as a *side-effect* of an action/park event, never as the deliverable |
| **Prometheus `/metrics`** | Gauges | **0** | Input only | Fine as an input; not a feature |

### 6.2 Trust-ramp assessment (the core differentiator)

The trust ramp is the sellable thing. Findings from the code:

- **Risk classification** is by **SQL shape** (`actionTypeForProposalSQL` → `ContractForActionType`), fail-closed on unknown contract (`contractForFinding` rejects SQL that fails `ValidateExecutorSQL`). This is *good* — deterministic, not LLM-trusted. The allowlist blocks `VACUUM FULL`, non-CONCURRENT `CREATE INDEX`, arbitrary `ALTER SYSTEM` (30-GUC whitelist), restricts `SELECT` to `pg_terminate/cancel_backend`+`pg_reload_conf`, and `ALTER TABLE` to SET/RESET/TABLESPACE. **Gap:** `checkProtectedSchemaUsage` protects `pg_catalog`/`information_schema`/`google_ml`/timescale but **not the `sage` schema itself** — the agent's own state.
- **Two divergent gates.** `ShouldExecute`/`shouldExecute` (`trust.go`) is **dead in production** (only tests call it) — the live path is `EvaluateActionPolicy`. The dead gate has *diverged*: it lacks the cancel/terminate→approval rule and the exec-mode logic. Latent hazard if re-wired. **Fix:** delete the dead gate; single source of truth.
- **Maintenance-window parser ignores day-of-week/day-of-month.** Confirmed by reproduction: `"0 2 * * 0"` (Sundays) returns *in-window* on a Monday. Moderate autonomous DDL fires outside the operator's intended days — **over-permissive**, exactly wrong for a safety gate. **Fix before any rung-4 expansion.**
- **Guardrail strings are descriptive, not enforced.** Contracts carry "approval required" guardrails; `EvaluateActionPolicy` only forces approval for backend signals — every other moderate action auto-executes at autonomous+`tier3_moderate`+window. The strings are copied into decisions and never consulted, so **audit surfaces misstate behavior**. (Mitigated: `tier3_moderate` defaults false.) **Fix:** make the guardrail the enforcement input, not decoration.
- **Escalation paths** (`SetTrustLevel`, `SetExecutionMode`, config PUT) are authenticated + role-gated (post-v0.8.4 RBAC); the audit found **no** improper escalation via LLM output (LLM self-rated risk is ignored; deterministic tiering wins). Good.
- **Posture on ambiguous classification** = fail closed (no contract → blocked). Good, and exactly what §5 requires.

**Verdict:** the ramp is sound in architecture, with three concrete defects (dead/divergent gate, cron window, unenforced guardrails) and one missing capability: **it has no standing-policy object** — trust is a global level + a few booleans, not the declarative budgets/windows/blast-radius object the no-human lane needs. **→ Feature #6.**

### 6.3 MCP surface assessment — ⚠ the strategic finding

**MCP is retired.** `internal/config/config.go` explicitly errors on an `mcp:` config key: *"configuration key 'mcp' is retired; use the REST API instead."* The only MCP artifacts are the **legacy C-extension** helpers in `src/mcp_helpers.c` (the old master-branch extension, not the Go sidecar) and adversarial test harnesses in `cloudsqltests/`. The current product interface is **REST + embedded React dashboard**.

Two problems for this brief's mission:
1. **The REST API is mechanism-level, not intent-level.** Endpoints are `findings`, `actions`, `config`, `emergency-stop` — CRUD over pg_sage's internal state. A calling agent must already know *what SQL it wants*. That is the opposite of §5's "make this query fast" intent surface. Agent-to-agent delegation (Question 2's core) **has no surface to land on.**
2. **The move to dashboard-first is the §2 tension made concrete.** The product removed its machine-facing interface and doubled down on a human web UI, while the differentiated lane is explicitly *no human*.

**Recommendation:** if the no-human lane is real, **re-introduce MCP as an intent-level surface** (tool names like `optimize_query(query_intent)`, `enforce_retention(table, policy)`, `ensure_fk_indexes(schema)`), gated through the *same* trust/policy path, returning decision+evidence+rollback-handle. Treat calling-agent claims as untrusted input. Scope the web UI to the **Evidence Ledger / approval surface**, not telemetry. This is a roadmap-level decision, flagged as required.

### 6.4 Roadmap items that fail the §2 filter (flag)

From `roadmap.md` "Considering for Future Releases": **Historical trend analysis** (weekly/monthly finding trends), **Query plan diffing → alert on plan regression**, **Cost attribution → cost per query in the dashboard**. All three are *observability deliverables* — charts/alerts for a human. Under the filter they are cut *as deliverables*; each is permitted only as a **silent input** to an action (plan-diff → trigger the verify-and-revert or a re-plan action; cost → a budget-enforcement input for #6; trend → a custodian trigger). Recommend re-scoping these from "features" to "signals."

---

## 7. Candidate feature inventory (scored; cuts kept)

Score 1–5; inverse-scored dimensions marked (↓ = 5 is best/cheapest). Rank metric R = (Toil × Autonomy × Verifiability × Differentiation) / (Effort_cost × Enabler_dep_cost), where Effort_cost/Enabler_cost are the *inverse* scores (5=cheap/none).

| # | Feature | Toil | Autonomy | Verif | Blast(↓) | Diff | Agent-native | Effort(↓) | Enabler(↓) | R | Verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| F1 | **Verify-and-Revert Index Lifecycle** | 5 | 5 | 5 | 4 | 5 | 3 | 3 | 4 | **52** | **DEEP-SPEC** |
| F2 | **Rehearse-on-Clone Migration Gate** | 5 | 4 | 5 | 4 | 5 | 3 | 2 | 2 | 25 | **DEEP-SPEC** |
| F3 | **Wraparound & Bloat Custodian** | 4 | 5 | 5 | 3 | 5 | 5 | 3 | 5 | **33** | **DEEP-SPEC** |
| F4 | **Replication-Slot & WAL Disk Guardian** | 3 | 5 | 4 | 3 | 5 | 5 | 4 | 5 | 15 | **DEEP-SPEC** |
| F5 | **Agent-Native Schema Custodian** | 4 | 3 | 4 | 4 | 5 | 5 | 3 | 4 | 13 | **DEEP-SPEC** |
| F6 | **Standing-Policy & Change-Serialization Control Plane** | 5 | 5 | 3 | 5 | 4 | 5 | 2 | 5 | 15 | **DEEP-SPEC** (foundation) |
| F7 | Fleet Staged-Rollout & Cross-Instance Learning | 4 | 5 | 4 | 2 | 5 | 5 | 2 | 3 | 13 | **DEFER** — depends on F6 + a stock of validated single-instance features; spec after F1/F3/F4 exist to roll out |
| F8 | Autonomous Restore-Verification guarantee | 3 | 4 | 5 | 4 | 5 | 4 | 2 | 2 | 12 | **HOLD** — high value, but needs backup/restore integration + clone target; fold pilot into F2's clone substrate |
| F9 | Plan-regression capture + re-plan/hint action | 4 | 3 | 3 | 4 | 4 | 3 | 2 | 3 | 5 | **CUT (as feature)** — Postgres has no plan pinning; ceiling is rung 2-3; keep the *capture* as a silent trigger, not a deliverable |
| F10 | Connection/pooler autotuning | 3 | 2 | 2 | 3 | 3 | 3 | 2 | 3 | 2 | **CUT** — low verifiability, pooler often external; advisory only |
| F11 | Extended-statistics autopilot | 3 | 4 | 4 | 5 | 3 | 3 | 4 | 5 | 9 | **FOLD into F1** — same verify loop (measure row-estimate error), smaller surface |
| F12 | Partition lifecycle autopilot | 3 | 4 | 4 | 4 | 4 | 4 | 3 | 4 | 9 | **HOLD** — strong guarantee-shape; sequence after custodians prove the "enforced invariant" pattern |
| F13 | Least-privilege drift reconciliation | 3 | 3 | 4 | 3 | 4 | 4 | 3 | 4 | 6 | **HOLD** — security-sensitive; recommend-only until F6 governance is proven |
| F14 | "Natural-language to SQL" tool | — | — | — | — | 1 | — | — | — | — | **CUT** — explicitly an anti-pattern (§14); table stakes, not DBA work |
| F15 | Standalone forecasting dashboard | — | — | — | — | 1 | — | — | — | — | **CUT** — fails §2; redesign into custodian triggers (see §6.1) |

**Reordering by judgment (over raw R):** F6 ranks mid-numerically but is sequenced **first** because F1/F3/F4/F5 cannot safely reach rung 3-4 without the policy object + fixed gate + evidence ledger it provides. F2's R is depressed purely by the thin-clone enabler dependency; its *value* is top-tier, so it stays in the deep-spec set with the enabler called out. F4's low R (blast-radius and toil-frequency both modest) understates its *catastrophe-avoidance* value (disk-full → read-only outage), so it stays in.

**Selection satisfies constraints:** 6 deep-specs (≤7 ✓); rung 3/4 = F1,F2,F3,F4,F6 (≥2 ✓); Workstream C = F5,F6 (≥2 ✓; F3/F4 also lean agent-native); upgrade-to-existing = F1 (optimizer/executor), F6 (trust ramp) (≥1 ✓).

---

## 8. Deep specifications

### Feature F1: Verify-and-Revert Index Lifecycle

**One-line pitch** — Take index tuning fully off the plate: pg_sage builds the index, measures its real effect on the queries it targeted, and reverts within a bounded window if there's no gain or a regression — the Azure/Oracle loop Postgres has never had.

**Problem** — Index management is high-frequency, high-toil, and *mechanically verifiable*, yet every Postgres tool stops at recommending (pganalyze, Dexter, HypoPG) or applies without measuring (pg_sage today verifies via a **global cache-hit ratio**, which reflects whole-database activity, not the change). The people who suffer are whoever owns query latency; today they manually build an index, eyeball a dashboard for a day, and manually drop it if it hurt writes. Azure and Oracle automated exactly this for their engines; Postgres has no equivalent. ([Azure](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql), [Oracle](https://oracle-base.com/articles/19c/automatic-indexing-19c))

**Anti-dashboard justification** — Produces (a) an **action** (CONCURRENTLY build), (b) a **decision artifact** (candidate + HypoPG pre-estimate + affected queryids + expected effect + rollback DDL), and (c) after the window, a **measured effect** with automatic revert. No chart is the deliverable.

**Prior art** — Oracle: invisible/unusable candidate → force real index, compare real execution stats → visible or drop. Azure: apply at low load → validate 30 min–72 h → revert immediately on regression, revert on *no significant gain*. pganalyze: CP-SAT selection of an index *set* balancing read gain vs. write cost. We take Oracle's two-stage (hypothetical→real) gate, Azure's *"revert on no-gain, not only on regression"* and low-load application + bounded window, and pganalyze's deterministic candidate-set arithmetic (so the LLM handles only judgment).

**Actor & trigger** — Analyzer cycle emits a missing/consolidation index finding (existing path). Trigger conditions: query(s) exceed the slow threshold with a scan the candidate would serve; table not write-heavy beyond policy; table ≤ policy size cap for online build; within maintenance window if the tier requires it.

**Inputs** — `pg_stat_statements` (target queryids, calls, mean/total time), `pg_stats`/`pg_statistic` (selectivity), existing indexes (dup/coverage check — already in optimizer), HypoPG (pre-estimate; degrade to EXPLAIN-only if absent), write rate (`pg_stat_user_tables` n_tup_ins/upd/del), table size. Version floor: PG14. HypoPG optional.

**Method** —
1. **Candidate generation (deterministic + LLM split).** Decompose the target queries' WHERE/JOIN/ORDER BY into indexable predicates (deterministic). Enumerate candidate indexes; cost each with the planner (generic plan if no live EXPLAIN). Run a **candidate-set selection** (greedy or CP-SAT-style) that maximizes estimated read gain minus write-amplification cost across the workload — *arithmetic, no LLM*. The **LLM is used only** to (a) judge ambiguous "is this workload write-heavy enough to skip?" calls and (b) synthesize the human-readable rationale. The LLM **never** decides the DDL string that executes.
2. **Pre-estimate gate (rung 2, cheap).** For the selected candidate, HypoPG-create it and re-plan the target queries. Require estimated cost drop ≥ policy threshold (Azure-style two-orders-of-magnitude is a strong signal; make it configurable). Reject if the planner wouldn't use it. This is the "invisible candidate" stage.
3. **Validate LLM output before it can act.** Every candidate DDL passes `ValidateExecutorSQL` (allowlist, single-statement, protected-schema — **including `sage`**, per §6.2 fix) and must be `CREATE INDEX CONCURRENTLY`. LLM self-rated risk is discarded; risk tier is derived from SQL shape.
4. **Apply (rung 3).** Build CONCURRENTLY on a dedicated connection with `lock_timeout` + `statement_timeout` (executor already does this). Only in a low-load window (add: skip if instance CPU/IO above policy ceiling — Azure's "apply at low utilization").
5. **Verify (the fix).** Record `before_state` = per-target-queryid windowed latency from `sage.query_store` (the F1 machinery exists). After a **validation window** sized by query frequency (Azure's 30 min–72 h; default e.g. 2 h, extended for infrequent queries), compute per-query latency delta *for the targeted queries only*, plus a write-amplification check on the table's INSERT/UPDATE latency. **Revert triggers:** (a) any targeted query regressed > threshold, OR (b) **no query improved by the minimum gain** (Azure's no-gain revert), OR (c) write latency on the table rose > threshold, OR (d) the index is INVALID.
6. **Retain or revert.** Retain → mark success, record measured effect. Revert → `DROP INDEX CONCURRENTLY` (the stored rollback), mark `rolled_back`, and set a hysteresis cooldown so the same candidate isn't re-proposed (executor has `CheckHysteresis`).

**Rehearsal** — Rung-2 HypoPG pre-estimate always. When the thin-clone enabler (F2) exists, optionally build the real index *on a clone* and replay captured traffic for a high-confidence pre-prod measurement before touching production — promotes confidence and shortens the prod validation window.

**Action & rollback** — Action: `CREATE INDEX CONCURRENTLY` (online, no ACCESS EXCLUSIVE). Rollback: `DROP INDEX CONCURRENTLY` — cheap, online, time-bounded by `statement_timeout`. Unrollbackable case: none for pure index add/drop (the data is untouched). INCLUDE-upgrade path (drop superseded old index) only after the new one is verified VALID — already handled, keep.

**Autonomy & trust** — Launch rung **3** (apply-with-auto-revert) at `advisory`+ for SAFE index adds; target rung **4** (autonomous within policy) once verify-and-revert has a track record (the trust ramp's own evidence promotes it). Standing-policy params (from F6): max index adds per window, write-heavy ratio cutoff, min-gain threshold, validation-window bounds, table-size cap, low-load ceiling. **Policy absent/ambiguous → fail closed** (no contract → blocked; no policy → observe-only).

**Verification** — Pre-declared, objective: "every targeted queryid's windowed mean latency improved ≥ G% with no targeted query regressed > R% and table write latency not up > W%, measured over the validation window." Auto-revert on breach or on no-gain.

**Failure modes (≥5):**
1. **Bad LLM candidate** (nonexistent column, non-IMMUTABLE expression) → caught by the 8 existing validators + `checkColumnExistence`/`checkExpressionVolatility` before build; rejected, logged.
2. **Stale/missing stats** → HypoPG pre-estimate unreliable → require a fresh ANALYZE first (chainable action) or fall back to a longer prod validation window; if `pg_stat_statements` lacks the target queryids, **do not act** (can't verify).
3. **Concurrent conflicting DDL** (another agent/app builds a similar index) → serialize via F6's DDL lease; `dropInvalidCreateIndexBlockers` already clears failed-CONCURRENTLY debris; coverage-exists check short-circuits.
4. **Partial application** (CONCURRENTLY fails mid-build → INVALID index) → post-check detects INVALID, auto-drops (existing), records durable cleanup-failure if the drop also fails (existing).
5. **Measurement ambiguity** (workload shifted during the window, unrelated latency change) → per-*targeted-query* comparison isolates the change from global noise (the whole point vs. today's global cache-hit); if targeted queries had too few calls in the window to be significant, **extend the window** rather than guess; if still insignificant, revert-to-safe (drop) and re-queue.
6. **Correct but harmful** (index helps reads but tanks a hot write path) → the write-amplification check on table INSERT/UPDATE latency triggers revert even when the read query improved.

**MCP surface** (requires re-introduction, §6.3) — `optimize_query(query_id | query_text, goal: "latency", constraints?: {max_write_impact_pct})` → returns `{decision, candidate_ddl, pre_estimate, expected_effect, rollback_handle, evidence_id}`. Calling-agent cannot request "skip verification" or "apply without revert" — the policy governs, not the caller.

**Evidence artifact** — `sage.action_log` row + Evidence Ledger entry: trigger finding, candidate set considered, HypoPG pre-estimate, applied DDL, before/after per-query latencies, revert decision + reason, rollback availability. Retained per policy (default > action lifetime).

**Acceptance criteria** — (happy) a genuinely useful index is built, targeted queries improve ≥ G%, retained. (adversarial) an index that helps reads but raises write latency > W% is **auto-reverted within the window**; a candidate with no real gain (planner uses it but latency flat) is **auto-reverted (no-gain rule)**; a build that goes INVALID is auto-dropped; concurrent duplicate DDL does not double-build; a table with < min calls on targeted queries never claims a verified improvement. (regression guard) with verify-and-revert disabled in config, the executor refuses to *auto-apply* (apply and verify are coupled — Azure's lesson).

**Dependencies & effort** — Depends on F6 (policy/gate) for rung-4; standalone at rung-3 using existing executor. Reuses optimizer, HypoPG, `query_store`, rollback monitor. **Medium** effort — mostly *replacing the regression check* (global cache-hit → per-target-query) and adding the no-gain/low-load logic; the scaffolding exists. Enabler: HypoPG (present, optional); thin-clone (optional booster via F2).

**Open questions** — Default validation-window length and min-gain threshold (Azure uses frequency-adaptive; needs tuning on real workloads). Whether to adopt full CP-SAT or a greedy set-selection first (start greedy). How aggressively to extend windows for low-frequency queries before giving up.

---

### Feature F2: Rehearse-on-Clone Migration Gate

**One-line pitch** — Turn "will this migration lock my hot table / break prod" from a human judgment call into a rehearsed, lock-safe, auto-reversible operation: lint it, run it against a thin clone with real data and captured traffic, then apply it online with cancel=rollback.

**Problem** — Online DDL and lock-avoidance are the highest-toil, highest-judgment, most outage-prone DBA tasks (ACCESS EXCLUSIVE on a hot table is the canonical self-inflicted outage — Squawk exists solely to prevent it). Long backfills and unsafe `ADD COLUMN NOT NULL`/`ADD UNIQUE` regularly take prod down. Today engineers hand-write expand/contract migrations, guess at lock impact, and test against a tiny dev DB that has different statistics and plans. ([Squawk rules](https://squawkhq.com/docs/rules), [pgroll](https://xata.io/blog/pgroll-internals))

**Anti-dashboard justification** — Produces an **action** (the online migration, applied) and a **decision artifact** (lint result + rehearsal measurement on real-size data + lock analysis + rollback plan). A blocked-in-CI lint alone would be mere telling; this *executes* the safe version.

**Prior art** — Squawk (deterministic lock-hazard rules; prescribes `lock_timeout`); pgroll/Reshape (expand/contract via dual-schema views + batched backfill + cancel=rollback); PlanetScale deploy requests (branch → online DDL → 30-min revert window); Database Lab (thin clones: 1 TiB in ~10 s, real plans). We take Squawk's lint as the *gate*, pgroll's expand/contract as the *execution engine*, Database Lab as the *rehearsal substrate*, and PlanetScale's bounded post-apply revert window as the *safety net*.

**Actor & trigger** — A migration arrives via: an agent's MCP `apply_migration(ddl, intent)` request, a GitHub Actions PR check (roadmap item), or a detected drift-reconciliation need (F6). Trigger: any DDL not on the trivially-safe list (e.g., `ADD COLUMN` nullable no-default is instant; `ADD UNIQUE`, `SET NOT NULL`, type change, `CREATE INDEX` non-concurrent are gated).

**Inputs** — The proposed DDL, target table size + write rate, current locks, a thin-clone target (Database Lab / Neon branch), captured workload sample for replay. Version floor PG14. **Hard dependency: thin-clone enabler** (degrade: if absent, rehearsal falls back to lock-analysis + statistics-based estimate only — lower confidence, cap at rung 2 recommend).

**Method** —
1. **Lint (deterministic gate).** Parse the DDL; apply a Squawk-equivalent rule set: flag ACCESS EXCLUSIVE acquisitions, table-rewrites (`SET NOT NULL` on big table without prior `NOT VALID` CHECK, type changes), non-CONCURRENT index builds, destructive renames/drops that break clients. Each hazard maps to a **safe rewrite** (e.g., `ADD UNIQUE` → `CREATE UNIQUE INDEX CONCURRENTLY` + `ADD CONSTRAINT USING INDEX`; `SET NOT NULL` → add `NOT VALID` CHECK → `VALIDATE` → set not null; add-column-not-null-default → expand/contract).
2. **Plan the expand/contract** (pgroll-style) for anything not natively online: dual-schema views, hidden column + batched backfill, `search_path`-based version selection.
3. **Rehearse on clone (rung 2).** Spin a thin clone; apply the (rewritten) migration; measure: actual lock type/duration held, backfill time at the policy batch size, resulting plans for the top affected queries (real data → real plans), disk delta. Replay a captured traffic sample against the migrated clone to catch app-breaking changes.
4. **Decision.** If rehearsal shows lock ≤ policy ceiling, backfill within window, no plan regression on affected queries → proceed. Else → **park** with the rehearsal evidence (do not apply).
5. **Apply (rung 3).** Execute the online plan on prod with `lock_timeout` (Squawk's rule — cancel if the lock can't be grabbed quickly, so waiters proceed), backfill in throttled batches (pause on replication-lag breach — F4 signal), old schema stays live.
6. **Bounded auto-revert window** (PlanetScale model). For a policy window after apply, monitor affected-query latency + error rates; on breach, **cancel/roll back** (old version still present → cheap). Contract phase (drop deprecated objects) only after the window passes clean.

**Rehearsal** — This feature *is* the rehearsal pattern; clone + traffic replay is the core. Threshold for success: lock ≤ ceiling, backfill ≤ window, zero affected-query plan regressions, replay error rate 0.

**Action & rollback** — Action: the online (expand phase) migration. Rollback during expand/window: cancel the migration — old schema live, so rollback = drop the new additive objects (cheap, online, bounded). **Unrollbackable:** the contract phase (dropping old columns/tables) — gated behind the clean window and a higher trust tier; a contract is never auto-applied on the same cycle as expand.

**Autonomy & trust** — Launch rung **3** for additive/expand migrations under policy; contract phase requires higher tier or a review gate (it's destructive). Never rung-4 for type changes or drops without rehearsal proof. Policy params (F6): lock-duration ceiling, backfill batch size + throttle, max table size for auto-apply, post-apply window. **Ambiguous → park (fail closed).**

**Verification** — Pre-declared: "held lock ≤ L ms, affected queries show no plan regression, replay error rate 0, backfill completed within window." Post-apply window monitors the same on prod; auto-revert on breach.

**Failure modes (≥5):**
1. **Bad/hazardous LLM-or-agent DDL** → lint gate rejects or rewrites; `ValidateExecutorSQL` blocks anything off-allowlist.
2. **Clone diverges from prod** (stale clone, different stats) → clone provisioned from a recent snapshot; if snapshot age > policy, refuse to claim rehearsal validity (fall back to recommend).
3. **Concurrent conflicting migration** → F6 DDL lease serializes; conflict detection parks the later one.
4. **Partial application** (backfill dies mid-way) → hidden-column/expand design means the table is never in a broken state; resume or roll back the additive change.
5. **Lock can't be acquired** → `lock_timeout` cancels the statement, waiters proceed, migration retried in the next window (no outage).
6. **Correct-but-harmful** (migration valid, but a new plan regresses under real traffic) → post-apply window + traffic replay catch it; auto-revert.

**MCP surface** — `apply_migration(ddl | intent, constraints?: {max_lock_ms, window})` → `{decision, rewritten_plan, rehearsal_result, rollback_handle, evidence_id}`. Caller may express intent ("add a unique email constraint"); pg_sage chooses the safe mechanism.

**Evidence artifact** — Migration ledger entry: original DDL, lint findings, safe rewrite, clone rehearsal metrics (lock, backfill, plans, replay), apply result, post-window verdict, rollback availability.

**Acceptance criteria** — (happy) an `ADD UNIQUE` is auto-rewritten to CONCURRENTLY+USING INDEX, rehearsed, applied with no measurable lock. (adversarial) a `SET NOT NULL` on a 500M-row table without a prior validated CHECK is **blocked/rewritten**, never applied as-is; a migration that passes lint but regresses a plan under replayed traffic is **parked** (not applied) or auto-reverted; a stale clone (> policy age) downgrades the result to recommend-only; a contract phase never auto-runs in the same cycle as expand.

**Dependencies & effort** — **Hard dependency: thin-clone enabler** (Database Lab Engine integration or Neon branch API) — the biggest lift. Reuses the migration-risk cases and DDL-safety code pg_sage already has. **High** effort (clone integration + expand/contract engine + traffic replay). Sequence after F6.

**Open questions** — Which clone substrate (Database Lab self-hosted vs. provider branching vs. filesystem snapshots) — likely pluggable. How to capture/replay a representative traffic sample cheaply. Whether to build the expand/contract engine or shell out to pgroll.

---

### Feature F3: Wraparound & Bloat Custodian

**One-line pitch** — Guarantee that no table in the database ever crosses its transaction-ID-freeze, bloat, or dead-tuple deadline — acting with escalating aggression as the deadline approaches, so the wraparound outage that took down Sentry and Mandrill simply cannot happen here.

**Problem** — XID wraparound is one of the few Postgres failures that halts *all writes* and has done so at shops that knew Postgres well: **Sentry (2024)** — autovacuum couldn't keep up freezing old XIDs, hit the wraparound limit, emergency manual vacuum + downtime; **Mandrill/Mailchimp** — autovacuum fell behind on a hot shard, wraparound protection halted writes, ~**40-hour** outage. Bloat and dead-tuple accumulation are the same disease earlier in its course. It's low-frequency but catastrophic, and in a no-human database *nobody is watching `age(relfrozenxid)`*. The deterministic deadline math makes it a perfect guarantee. ([Sentry](https://blog.sentry.io/transaction-id-wraparound-in-postgres), [Mailchimp](https://mailchimp.com/what-we-learned-from-the-recent-mandrill-outage/), [Bytebase](https://www.bytebase.com/blog/postgres-transaction-id-wraparound/))

**Anti-dashboard justification** — Produces a **guarantee** (a continuously enforced invariant: "no table's XID age > deadline, no table's bloat > ceiling") plus the **actions** that maintain it (per-table autovacuum tuning, targeted VACUUM/FREEZE, pg_repack orchestration). The invariant, not a "wraparound risk gauge," is the deliverable. (This is the redesign of the Forecaster, §6.1 — prediction becomes a silent trigger.)

**Prior art** — No autonomous Postgres product enforces this as a guarantee; the space offers monitoring + runbooks (Percona "Overcoming VACUUM WRAPAROUND," AWS "early warning system" — both *alert a human*). Oracle/Azure's automatic-maintenance framing (self-managed maintenance windows) is the closest philosophy: maintenance happens autonomously in low-load windows. We take the *guarantee* framing and the escalating-aggression response.

**Actor & trigger** — Continuous evaluation each collector cycle. Triggers, per table: `age(relfrozenxid)` crossing graduated thresholds toward `autovacuum_freeze_max_age`; dead-tuple ratio > ceiling; bloat estimate > ceiling; a long-running transaction holding back the xmin horizon.

**Inputs** — `pg_class.relfrozenxid` / `age()`, `pg_stat_user_tables` (n_dead_tup, n_live_tup, last_autovacuum), bloat estimate (existing rules), `pg_stat_activity` (oldest xmin holder), current autovacuum settings. Version floor PG14. No extension required (pg_repack optional — degrade to VACUUM if absent/unsupported on managed).

**Method** —
1. **Deadline math (deterministic).** For each table compute distance-to-freeze = `autovacuum_freeze_max_age − age(relfrozenxid)` and a *time-to-deadline* estimate from the XID consumption rate (from snapshots). This yields a per-table urgency, purely arithmetic — **no LLM.**
2. **Graduated response ladder** (escalating aggression as urgency rises): (a) **green** — ensure per-table autovacuum is tuned (scale factors, cost limits) so routine autovacuum keeps up; propose `ALTER TABLE SET (autovacuum_*)` via the verify-and-revert config path. (b) **amber** — issue a targeted `VACUUM (FREEZE)` on the table in the maintenance window, on a dedicated connection (VACUUM can't run in a txn — pg_sage handles this). (c) **red** — deadline near: escalate outside the normal window (the standing policy must permit deadline-override — this is the "degraded-state clock forces action" case from §5), raise cost limits, and if a long-running transaction is holding xmin, surface/act on it (the runaway tracker can cancel it with evidence). (d) **bloat branch** — if the table is bloated beyond ceiling and VACUUM won't reclaim (needs rewrite), **orchestrate pg_repack** (online) rather than `VACUUM FULL` (which takes ACCESS EXCLUSIVE — refused by the allowlist anyway).
3. **LLM split.** Deterministic logic owns *everything that must be correct* (deadline math, which action, escalation). The LLM is used only to (a) explain the action in the evidence artifact and (b) judge genuinely ambiguous autovacuum-tuning trade-offs (e.g., a table that's both write-hot and freeze-urgent) — and even then its output is a *parameter within validated ranges*, never the decision to act.

**Rehearsal** — Autovacuum-setting changes go through F1's verify-and-revert (measure dead-tuple trend + write latency after). VACUUM/FREEZE and pg_repack are low-risk online ops; "rehearsal" is a dry-run size/lock estimate. pg_repack can be rehearsed on a clone (F2) when available.

**Action & rollback** — Actions: `ALTER TABLE SET (autovacuum_*)` (reversible — store prior values), `VACUUM (FREEZE)` (no rollback needed — idempotent maintenance), pg_repack (online rewrite; its own safety/rollback). **Unrollbackable but safe:** VACUUM/FREEZE change no logical data. pg_repack failure leaves the original table intact (its design).

**Autonomy & trust** — This is a **rung-4** candidate at launch for the safe actions (VACUUM/FREEZE, autovacuum tuning) because the deadline is deterministic and the actions are low-risk maintenance — *and because the alternative (waiting for approval) can mean a wraparound outage*. **Critical:** the standing policy (F6) must grant a **deadline-override** — when red, the custodian may act *outside* the normal maintenance window, because a missed XID deadline is worse than an off-window VACUUM. This is the sharpest expression of "fail-safe ≠ fail-closed-into-an-outage": here fail-safe means *act*. pg_repack (rewrite) stays gated at moderate/approval unless policy explicitly permits.

**Verification** — Per-invariant, objective: after action, `age(relfrozenxid)` dropped below the amber threshold / dead-tuple ratio below ceiling / bloat below ceiling. Continuous re-check; if an action didn't move the metric (e.g., xmin held by a live txn), escalate to the blocker.

**Failure modes (≥5):**
1. **VACUUM can't advance the freeze horizon** because a long-running transaction / stale replication slot holds xmin → detect the blocker (`pg_stat_activity` oldest xmin, `pg_replication_slots`), act on *it* (cancel txn w/ evidence via runaway tracker; hand the slot to F4) — vacuuming the table alone is futile and the custodian must recognize that.
2. **Bad autovacuum parameter** (too aggressive → I/O storm) → verify-and-revert path measures write latency and reverts; values are range-validated.
3. **Deadline mis-estimate** (XID rate spikes) → graduated thresholds + continuous re-evaluation give margin; red threshold is set with safety buffer below the hard limit.
4. **pg_repack unavailable/unsupported** (managed provider) → degrade to VACUUM + surface that a rewrite is needed as a parked decision (can't safely online-rewrite without it).
5. **Off-window action needed but policy forbids it** → the deadline-override clock: if urgency crosses red and policy has no override, the custodian **escalates to a forced protective action** (or, if truly forbidden, parks with a *loud* durable artifact and a countdown — this is the "how long can it hold degraded" case; it must not silently let the deadline pass).
6. **Correct-but-harmful** (a huge FREEZE during a low-but-not-idle window causes latency) → cost-limit throttling + apply-at-lowest-load; the freeze is chunked where possible.

**MCP surface** — Mostly autonomous (guarantee), but exposes `set_maintenance_policy(table | schema, {freeze_deadline_buffer, bloat_ceiling, allow_offwindow_on_deadline})` and `get_guarantee_status()` → machine-readable invariant state (which is data for another agent, not a human dashboard).

**Evidence artifact** — Per action: table, urgency (age, time-to-deadline), action taken, before/after XID age + dead-tuple ratio, whether off-window override was used and why. The guarantee's continuous status is itself a durable record ("invariant held since T").

**Acceptance criteria** — (happy) a steadily-aging table gets autovacuum-tuned in green, FREEZE'd in amber, never reaches red. (adversarial) a table whose xmin is pinned by a 6-hour idle-in-transaction session is *not* endlessly VACUUM'd — the custodian identifies and acts on the blocker; a table approaching the hard wraparound limit with `allow_offwindow=false` produces a loud escalating artifact and (per policy) a forced protective action rather than a silent deadline miss; an over-aggressive autovacuum setting that spikes write latency is auto-reverted.

**Dependencies & effort** — Depends on F6 (deadline-override policy is the novel governance bit) and F1's verify-and-revert (for autovacuum tuning). Reuses collector, bloat rules, runaway tracker, executor VACUUM path. **Medium** effort (mostly the deadline ladder + policy override; the actions exist). Enabler: none required (pg_repack optional).

**Open questions** — Exact red-threshold buffer below `autovacuum_freeze_max_age`. Whether deadline-override should ever be fully autonomous or always require a standing policy opt-in (recommend: opt-in, defaulted *on* for the no-human profile — a DB with no human *must* have it or it will eventually wrap).

---

### Feature F4: Replication-Slot & WAL Disk Guardian

**One-line pitch** — Guarantee that no inactive replication slot can silently retain WAL until the disk fills and the primary flips to read-only — detect the abandoned/lagging slot and act before the wall, the classic unattended-2am outage.

**Problem** — An inactive or slow replication slot makes Postgres hoard WAL "because it thinks a replica still needs it"; on a busy system you go from fine to 95% full in under two hours, and when the disk fills **Postgres stops accepting writes** (Azure flips to read-only at 95%). It's "the single most common way CDC pipelines take down production," and it catches teams off guard because the failure is on the DB side, not the CDC side. In a no-human DB there is no disk alert and no pager. ([Gunnar Morling](https://www.morling.dev/blog/insatiable-postgres-replication-slot/), [EDB](https://www.enterprisedb.com/blog/postgresql-13-dont-let-slots-kill-your-primary), the "$2.5M panic" write-up)

**Anti-dashboard justification** — Produces a **guarantee** ("no slot retains WAL beyond policy / no path to disk-full via slots") and the **actions** that enforce it (set `max_slot_wal_keep_size`, drop a provably-abandoned slot, or throttle producers). Not a "WAL usage chart."

**Prior art** — Postgres 13 gave the *knob* (`max_slot_wal_keep_size`) but not the *policy*; EDB and others document it as a manual safeguard. No autonomous product owns slot lifecycle. We take the knob and wrap it in a guarantee + a safe abandoned-slot reaper.

**Actor & trigger** — Continuous. Triggers: a slot's retained WAL (`pg_replication_slots` restart_lsn vs. current LSN → bytes) crossing thresholds; slot `active=false` for longer than policy; retained WAL as a fraction of available disk crossing a ceiling; disk free-space trend projecting exhaustion within the horizon.

**Inputs** — `pg_replication_slots` (active, restart_lsn, wal_status, safe_wal_size on PG13+), current WAL LSN, disk free space (via provider API or a size proxy on managed where filesystem isn't visible), slot age/last-active. Version floor PG13 (for `max_slot_wal_keep_size` / `wal_status`). No extension.

**Method** —
1. **Classify each slot (deterministic).** For every slot compute retained-WAL bytes and disk-fraction; classify: *healthy-active*, *lagging-active* (consumer alive but behind), *inactive-recent* (recently dropped consumer, might return), *abandoned* (inactive > policy age AND retaining > policy bytes). Arithmetic + catalog — no LLM.
2. **Graduated response.** (a) *lagging-active* → the consumer is alive; **throttle producers** if within policy (or hand a backpressure signal to backfills — F2), and set/confirm `max_slot_wal_keep_size` as a backstop so the slot can't kill the primary (the slot gets invalidated instead, a recoverable state, vs. disk-full, an outage). (b) *inactive-recent* → hold, watch. (c) *abandoned* → after positive proof of abandonment over a long window (Azure's long-unused discipline applied to slots), **drop the slot** within policy — but only if it's provably not a configured/known consumer. (d) *disk-trend-red* → if projected exhaustion is near and no safe slot action suffices, escalate to a protective action (set the keep-size backstop, alert as a *side effect*, and — per policy — throttle writes rather than allow a hard read-only stop).
3. **LLM split.** None on the decision path — this is deterministic lifecycle + threshold logic. LLM only narrates the evidence artifact.

**Rehearsal** — Setting `max_slot_wal_keep_size` is reversible and low-risk (rehearse = confirm the value won't immediately invalidate a *healthy* slot). Dropping a slot is destructive to a consumer that returns — the "rehearsal" is the *long abandonment-proof window*, not a clone.

**Action & rollback** — Actions: `ALTER SYSTEM SET max_slot_wal_keep_size` (or provider parameter group on managed) — reversible; `pg_drop_replication_slot(name)` — **the destructive, effectively-unrollbackable one** (a returning consumer must re-snapshot). Rollback for keep-size: restore prior value. Drop has no rollback → gated hardest.

**Autonomy & trust** — Setting the keep-size backstop is **rung 4** (safe, reversible, prevents the outage). **Dropping a slot is the hard case:** default **rung 2-3** (recommend / apply-only-with-strong-proof), never rung-4 without an explicit policy opt-in *and* a long abandonment window *and* confirmation the slot isn't in the known-consumers registry. This is exactly the §5 "refuse the unrollbackable unless proven" posture. **Fail closed:** if abandonment can't be proven, keep the WAL bounded (keep-size) but **don't drop** — bounding beats deleting.

**Verification** — Per-invariant: retained WAL bounded below ceiling; disk-exhaustion horizon beyond policy. After a drop, confirm WAL began releasing. After keep-size set, confirm the healthy slots weren't invalidated.

**Failure modes (≥5):**
1. **Drop a slot a consumer still needs** → prevented by the long abandonment window + known-consumers registry + "bound don't delete" default; the keep-size backstop makes dropping rarely necessary.
2. **Managed provider hides the filesystem** → use provider disk metrics / `safe_wal_size` proxy; if truly unavailable, fall back to WAL-bytes-relative-to-configured-max and act on the knob, not on disk %.
3. **Keep-size set too low** → could invalidate a legitimately-lagging slot → set it relative to available disk with margin; invalidating a lagging slot (recoverable) is *by design* preferable to disk-full (outage), and is logged loudly.
4. **Producer throttling harms the app** → throttling is policy-bounded and preferred only over an imminent hard stop; it's a red-state action, not routine.
5. **Disk fills from a non-slot cause** (bloat, temp files, base backup) → the guardian owns *slot-driven* WAL; disk-full from other causes hands off to F3 (bloat) / retention (F5) — the guardian must not falsely attribute.
6. **Rapid slot flapping** (consumer reconnecting intermittently) → anti-oscillation: don't drop/act on a slot that's been active within the window; treat as lagging-active.

**MCP surface** — `register_consumer(slot_name, owner)` (so agents declare their slots → moves them out of "abandoned" candidacy — but a claim only *protects*, it never *authorizes* a drop), `set_wal_retention_policy({max_bytes, abandon_after, allow_drop})`, `get_guarantee_status()`.

**Evidence artifact** — Per action: slot name, classification, retained bytes, disk fraction, abandonment proof (inactive duration, not-in-registry), action taken, WAL-release confirmation. The bounded-WAL invariant status as a durable record.

**Acceptance criteria** — (happy) a slot whose consumer died is bounded by keep-size immediately and dropped only after the abandonment window with registry confirmation. (adversarial) a slot that flaps (reconnects every few minutes) is **never dropped**; a slot registered by a known agent-consumer is never auto-dropped even if briefly inactive; when disk-exhaustion is imminent and no safe drop is provable, the guardian **bounds WAL (keep-size) and throttles per policy** rather than letting the primary hit read-only; on a managed provider with no filesystem view, it still enforces WAL-bytes bounds.

**Dependencies & effort** — Depends on F6 (the drop-authorization policy + known-consumers registry). Reuses collector + executor (ALTER SYSTEM/parameter-group path) + provider adapters. **Low-Medium** effort. Enabler: none (provider disk metrics helpful, not required).

**Open questions** — How to obtain disk free-space uniformly across self-managed vs. RDS/Aurora/Cloud SQL/AlloyDB (provider APIs differ). Default abandonment window (hours vs. days) — likely per-profile (aggressive for ephemeral agent DBs).

---

### Feature F5: Agent-Native Schema Custodian

**One-line pitch** — Continuously enforce that an agent-authored schema stays healthy — every FK has a supporting index, no table grows unbounded without a retention policy, no obvious anti-pattern persists — remediating what it safely can itself and parking what it can't.

**Problem** — When agents author DDL, nobody does design-time review, and agents produce recognizable pathologies: FKs with no supporting index (every parent delete/update scans the child), everything `text`, random-UUID PKs wrecking index locality, unbounded append-only tables with no retention (the silent disk-filler), missing constraints the app assumes. These accumulate silently and surface later as performance cliffs, disk exhaustion, and lock storms. No product targets *this* class of problem — it's green field (§4.3). ([Workstream A/C evidence])

**Anti-dashboard justification** — Produces **guarantees** (enforced invariants: "every FK has a supporting index," "every table has a retention policy or an explicit exemption," "no table exceeds N rows unpartitioned") and the **actions** to maintain them (build FK-supporting indexes CONCURRENTLY, enforce retention, validate constraints). A "schema health grade" would fail the filter — the invariant, enforced, is the deliverable.

**Prior art** — Schema linters (Squawk, migration linters) catch *proposed* bad DDL in CI; none continuously *remediate live* schemas, and none target agent-specific pathologies. Bytebase/Atlas do schema-as-code drift detection (feed into F6). We extend from "lint the migration" to "enforce the invariant on the living schema, and fix it."

**Actor & trigger** — Continuous schema scan (a slow cycle, e.g., hourly/daily) + on-DDL trigger (via F6's change ledger — when an agent adds a table/FK, check it immediately). Triggers: an FK without a matching leading-column index; a table crossing a row/size threshold with no partition or retention policy; a column pattern matching a known anti-pattern; a table with no PK or a random-UUID PK on a high-write table.

**Inputs** — `pg_constraint` (FKs), `pg_index`, `pg_attribute`/`pg_type` (column types), `pg_stat_user_tables` (row counts, write rate, growth), `pg_class` (size), the intended-schema baseline + exemptions registry (from F6). Version floor PG14. HypoPG helps validate proposed indexes (optional).

**Method** —
1. **Invariant catalog (deterministic detection).** A rule set, each rule = (detector, severity, remediation-class, auto-remediable?): (a) *FK without supporting index* → detector joins `pg_constraint` to `pg_index`; remediation = `CREATE INDEX CONCURRENTLY` on the FK columns; **auto-remediable** (additive, verifiable via F1). (b) *unbounded append table* → high insert rate, monotonic growth, no delete/retention; remediation = propose+enforce a retention policy (partition-by-time + drop-old, or a scheduled delete); **auto-remediable with policy consent**. (c) *missing NOT NULL/CHECK the app relies on* → hard to prove intent → **recommend/park**. (d) *everything-text* → propose domain/type tightening → **recommend** (type change needs rehearsal — F2). (e) *random-UUID PK on write-hot table* → propose UUIDv7/identity migration → **recommend + rehearse** (rewrites the table; never auto). (f) *no PK* → recommend. (g) *index-on-everything / redundant* → hand to F1 (drop redundant, verified).
2. **Remediate what's safe (rung 2-3).** FK-supporting indexes and redundant-index cleanup go through F1's verify-and-revert (they're index ops). Retention enforcement is additive (partition + scheduled drop) — apply under policy with the retention window declared.
3. **Park what's not.** Type changes, PK migrations, constraint additions where app-intent is unprovable → durable parked decision with the evidence and the proposed fix, awaiting a policy grant or a reviewer (who may never come — that's fine, the DB is safe meanwhile).
4. **LLM split.** Deterministic detection owns the invariants (all catalog arithmetic). The LLM is used for *judgment*: inferring likely intent ("this `status text` column has 4 distinct values — propose an enum/CHECK"), synthesizing the remediation rationale, and prioritizing which parked items matter — never to decide an auto-applied structural change.

**Rehearsal** — Additive fixes (indexes, retention partitions) → F1 verify-and-revert / low-risk. Structural fixes (type/PK changes) → **must** rehearse on a clone (F2) and are recommend-only until then.

**Action & rollback** — Auto actions: `CREATE INDEX CONCURRENTLY` (FK support; rollback = drop), partition creation + retention schedule (rollback = detach/stop schedule). Parked/recommended: type changes, PK migrations, constraint additions (via F2 when executed). **Unrollbackable:** dropping data via retention — gated hard; first retention run is *dry-run* (report what *would* be deleted) before enforcement, and retention deletes are bounded/batched.

**Autonomy & trust** — FK-index and redundant-index enforcement: **rung 3-4** (safe, verifiable). Retention enforcement: **rung 3** with mandatory dry-run first + policy-declared window (deleting data is serious). Structural changes: **rung 1-2** (recommend/rehearse), never auto. **Fail closed:** unprovable intent → park, never guess-and-apply.

**Verification** — Per-invariant: after a fix, the invariant holds (FK now has an index; table now has an enforced retention policy; redundant index gone with no read regression). Continuous re-audit (the §5 self-audit): do past fixes still hold (is the FK index still used, is retention still running)?

**Failure modes (≥5):**
1. **Bad inferred intent** (LLM proposes a CHECK the app violates) → structural/constraint changes are recommend/rehearse-only; a `NOT VALID`→`VALIDATE` path surfaces existing violations *before* enforcing.
2. **FK index that's genuinely unneeded** (tiny lookup table, never a bulk parent delete) → the invariant is "FK has supporting index," but F1's verify step catches a pure write-cost-no-read-gain index and reverts; or policy exempts small tables.
3. **Retention deletes wanted data** → mandatory dry-run + explicit retention-window consent + batched, bounded deletes; never a first-run mass delete.
4. **Concurrent agent re-introduces the pathology** (drops the FK index again) → anti-oscillation (executor already guards this) + park after N reversions with a "the schema keeps reverting externally" artifact (matches the existing oscillation guard).
5. **Partial application** (index build fails) → F1's INVALID-index cleanup handles it.
6. **Correct-but-harmful** (tightening a type breaks a writer) → type changes are rehearsed on a clone with traffic replay (F2) and never auto-applied.

**MCP surface** — `declare_table_contract(table, {append_only?, retention?, expected_pk?, exemptions?})` (an agent declares intent so the custodian enforces the *right* invariants — but a declaration constrains, it doesn't authorize destructive action), `ensure_fk_indexes(schema)`, `get_schema_invariant_status()`.

**Evidence artifact** — Per invariant/fix: pathology detected, evidence, remediation class, action taken or parked, verification. Parked items form a durable "schema debt" ledger for a reviewer who may never arrive.

**Acceptance criteria** — (happy) a newly-created FK gets a supporting index built and verified within a cycle; a declared append-only table gets an enforced retention policy after a dry-run. (adversarial) a proposed type tightening is **never auto-applied** (recommend/rehearse only); a first retention run **dry-runs** and does not delete; a small lookup table's FK index that shows write-cost-no-gain is reverted or exempted; a pathology an external agent keeps re-introducing is parked after N reversions, not fought forever; retention never mass-deletes on first enforcement.

**Dependencies & effort** — Depends on F6 (intended-schema baseline, exemptions registry, retention policy consent) and reuses F1 (index verify) + F2 (clone rehearsal for structural fixes). **Medium-High** (the invariant catalog is broad). Enabler: F2's clone for structural fixes; HypoPG optional.

**Open questions** — How to establish the "intended schema baseline" for a DB nobody declared (infer from current state + agent declarations?). Where to draw the auto-remediate / recommend line for each pathology (proposed split above is a starting point). Retention default posture (opt-in vs. enforced-with-consent).

---

### Feature F6: Standing-Policy & Change-Serialization Control Plane *(foundation)*

**One-line pitch** — Replace "a human approves changes" with a declarative policy object the agent enforces itself — allowed change classes, windows, lock/blast-radius/spend budgets, fail-closed refusal of the unrollbackable — plus DDL serialization and drift reconciliation for databases with many agent-writers and no owner.

**Problem** — Every rung-3/4 feature above assumes *someone eventually reviews*. In the no-human database there is no reviewer, no tribal knowledge, no escalation target. The current trust ramp is a global level + a few booleans — it has no notion of per-change-class budgets, lock ceilings, blast-radius limits, spend caps, or a fail-closed refusal set, and (audit) it has a dead/divergent second gate, a maintenance-window parser that ignores day-of-week, and "approval required" guardrails that aren't enforced. Multi-writer DBs have no arbiter for conflicting DDL. This feature is the governance substrate everything else needs. ([§5, §6.2])

**Anti-dashboard justification** — Produces the **decision-governance layer** (a policy object + enforcement point) and the **Evidence Ledger** (durable decision artifacts). It's pure "decide and do / refuse," never "display."

**Prior art** — Azure/Oracle "maintenance windows + self-disable to protect the workload" (autonomous ops bounded by low-load windows and safety self-governors). Schema-as-code drift (Atlas/Bytebase) for the intended-state baseline. PlanetScale "safe migrations" branches (only deploy-request-able) as the serialization model. We synthesize these into a *policy object* + *DDL lease* + *fail-closed parking* for the no-human case, which none of them fully specify.

**Actor & trigger** — Consulted by *every* executor action (the enforcement point) and by the schema custodian (F5) and migration gate (F2) for serialization. Trigger: any proposed change; any incoming DDL (for serialization); periodic drift reconciliation.

**Inputs** — The policy object (below), the intended-schema baseline, the in-flight/queued change ledger, current resource state (locks, load, disk, spend/token budgets), the action contract (existing typed contracts + risk tiers). Version floor PG14 (advisory locks for leases). No extension.

**Method** —
1. **The policy object (declarative).** Per instance (with fleet inheritance — F7): `{ allowed_change_classes: [...], maintenance_windows: [...], lock_duration_ceiling_ms, blast_radius: {max_rows_rewritten, max_tables_per_window}, budgets: {storage_bytes, spend_or_tokens_daily}, rate_limits: {max_self_initiated_changes_per_window}, deadline_overrides: {xid, disk}, refusal_set: [unrollbackable classes...], unknown_classification: "fail_closed" }`. It is the *only* source of authority — a calling agent's claims never grant permission (§5).
2. **Enforcement point (single gate).** Unify the divergent gates (delete the dead `ShouldExecute`; `EvaluateActionPolicy` is the one path). Every action is evaluated against: contract risk tier × trust level × policy budgets/windows/blast-radius × emergency-stop × replica. **Fix the maintenance-window parser** (honor day-of-week/day-of-month) and **make guardrail strings enforced**, not decorative. Ambiguous/unknown classification → **fail closed** (already the posture — codify it in the policy).
3. **DDL serialization (multi-writer).** A **change lease**: before any DDL, acquire a per-object advisory lock + write a `sage.schema_change` ledger row (intent, actor, target objects). Conflicting concurrent DDL on overlapping objects is queued or parked. This gives the "arbiter" a no-owner DB lacks.
4. **Drift reconciliation.** Periodically diff live schema vs. the intended baseline. Additive/safe drift → the custodian (F5) reconciles. Destructive/ambiguous drift → **park** with evidence.
5. **Fail-closed / no-reviewer posture (§5).** Refusal set = classes the agent will *never* auto-do (RLS/grant expansion, major upgrade, non-duplicate object drop, anything unrollbackable) → parked, never applied. **Degraded-state clock:** a parked decision blocking a *deadline* (XID/disk) triggers the deadline-override protective action (F3/F4) rather than passing the deadline. A parked decision *not* blocking a deadline can hold indefinitely (safe-but-suboptimal) with a durable artifact.
6. **Evidence Ledger.** Formalize `sage.action_log`+Cases+Shadow into a durable, self-contained record per decision (trigger, evidence, alternatives, action, rehearsal, measured effect, rollback availability), retained per policy. This is the web-UI's proper job (review decisions, not telemetry).
7. **LLM split.** The policy engine is **fully deterministic** (it's the thing that must be correct). The LLM never sets policy, never grants authority, never overrides the gate. It may only *draft* a proposed policy for a human/operator to ratify, and *narrate* ledger entries.

**Rehearsal** — Policy changes themselves are config; a policy change goes through a validation pass (no self-contradiction, no budget=0-means-unlimited traps — an audit-class bug) and a dry-run showing which currently-pending actions it would now allow/block before it takes effect.

**Action & rollback** — "Actions" here = gate decisions + lease grants + parks + reconciliations. Policy changes are versioned and reversible (restore prior policy). A change lease is released on completion/abort. No unrollbackable action originates here.

**Autonomy & trust** — This is the substrate that *defines* rungs 3-4 for everything else. It runs at all trust levels (it's the governor). Its own changes (editing the policy) are **operator/admin-gated** (authenticated, role-checked — reuse the RBAC added in v0.8.4). **Fail closed** is its default and its whole point.

**Verification** — Objective: every executed action is traceable to a policy grant; no action outside policy executed; every parked item has a durable artifact; drift reconciled or parked; the gate is provably the single path (no bypass). Continuous self-audit that the ledger is complete (every action has an evidence entry).

**Failure modes (≥5):**
1. **Policy misconfiguration opens too much** (e.g., budget 0 read as "unlimited" — a real audit-class bug) → validation pass rejects ambiguous/zero-means-unlimited configs; dry-run surfaces the blast radius before apply.
2. **Two agents race conflicting DDL** → the change lease serializes; the loser queues or parks; no interleaved half-migrations.
3. **Gate bypass** (an action path that skips `EvaluateActionPolicy`) → single-gate invariant enforced by construction (delete the dead second gate; every Exec routes through it); a self-audit checks for un-gated action_log entries.
4. **Deadlock/starvation on leases** (an agent holds a lease and dies) → lease TTL + reclamation (advisory locks auto-release on connection death; ledger rows time out).
5. **Parked-forever on a deadline** → the degraded-state clock forces the protective action (F3/F4) rather than silently missing an XID/disk deadline.
6. **Untrusted caller escalation** (agent claims "autonomous approved") → claims are input, not authority; the gate consults only the policy; the claim is logged as a *request*, not honored.

**MCP surface** — `get_policy()`, `propose_policy_change(delta)` (returns a dry-run impact; ratification is operator-gated, not caller-granted), `request_change(intent)` → routed through the gate → `{granted | queued | parked, evidence_id}`, `get_ledger(filter)` (machine-readable decision history). No MCP tool can widen authority.

**Evidence artifact** — The Evidence Ledger *is* this feature's output: every gate decision, lease, park, reconciliation, and policy change, durable and self-contained.

**Acceptance criteria** — (happy) an in-policy action executes and is fully traced in the ledger; an out-of-window moderate DDL is blocked until the (correctly-parsed) window; a Sunday-only maintenance window is honored on Sunday and *only* Sunday (the audit bug fixed). (adversarial) two conflicting concurrent migrations are serialized, not interleaved; a caller asserting elevated authority is refused and its claim logged as a mere request; a zero/ambiguous budget is rejected at validation, not silently treated as unlimited; a parked decision blocking an XID deadline triggers the protective override; every executed action has a matching ledger entry (no un-gated actions).

**Dependencies & effort** — None upstream (it's the foundation); fixes the three trust-ramp audit defects along the way. Everything else (F1-F5, F7) depends on it. **High** effort (it's the governance core + the ledger), but high-leverage. Enabler: none.

**Open questions** — Policy expression format (YAML-native like the rest of pg_sage config, vs. a richer DSL). How much of the policy should be operator-authored vs. agent-proposed-then-ratified. Default profiles ("staffed" vs. "no-human/ephemeral" — the latter defaults deadline-overrides on and windows wide).

---

## 9. Sequencing and roadmap

**Dependency DAG (build order):**

```
F6 (Policy + fixed gate + Evidence Ledger)   ← FOUNDATION, build first
      │
      ├──> F1 (Verify-and-Revert Index)       ← mostly present; earliest differentiation
      │        │  (fixes rollback-monitor weak spot; reusable verify loop)
      │        ├──> feeds F3 (autovacuum tuning verify)
      │        └──> feeds F5 (index/retention enforcement)
      │
      ├──> F3 (Wraparound/Bloat Custodian)     ← guarantee; needs F6 deadline-override; no clone needed
      ├──> F4 (Slot/WAL Guardian)              ← guarantee; needs F6 drop-policy; no clone needed
      │
      └──> [THIN-CLONE ENABLER integration]    ← the big infra lift
               ├──> F2 (Rehearse-on-Clone Migration)   ← needs clone
               ├──> deepens F1 (pre-prod verification)
               └──> F5 (Schema Custodian, structural fixes)  ← needs F2's clone + F6 serialization

F7 (Fleet staged rollout / cross-instance learning)  ← LAST; needs F6 + a stock of validated F1/F3/F4
```

**Phased roadmap:**

- **Phase 0 — Foundation & audit fixes (F6 core).** The policy object, single unified gate, the three trust-ramp fixes (delete dead gate, fix cron window parser, enforce guardrails), the Evidence Ledger. *Also fix the audit's High-severity SSRF and fleet-mode dead features first — they undercut trust in the whole system.* Nothing rung-4 ships before this.
- **Phase 1 — First autonomy wins (F1, F3, F4).** Verify-and-revert index lifecycle (the headline; mostly reuses existing scaffold, so cheapest high-value), and the two custodians/guarantees that need no clone. These prove the "act → verify → revert / guarantee" model on production with contained blast radius.
- **Phase 2 — Rehearsal substrate (thin-clone enabler + F2, deeper F1).** The infrastructure lift. Unlocks rehearsed migrations and pre-prod verification. Highest-value but highest-cost; sequenced after the cheaper wins bank credibility.
- **Phase 3 — Agent-native schema (F5) + MCP re-introduction.** Schema custodian (needs F2's clone for structural fixes and F6's serialization) and — the strategic call — an **intent-level MCP surface** so agent-to-agent delegation has a home. Without MCP, Question 2's differentiation is unreachable.
- **Phase 4 — Fleet (F7).** Staged rollout + cross-instance learning across the fleet, once there's a stock of per-instance validated changes worth propagating and F6's policy inheritance is proven.

**Enabler gating (explicit):** thin clones gate F2 and the strongest form of F1/F5 (rungs stall at 2 without them — the OtterTune lesson: unverifiable = untrusted). HypoPG (present) enables F1's cheap pre-estimate. MCP re-introduction gates the entire agent-to-agent lane. Provider disk/parameter-group APIs gate F4/F3 on managed platforms.

---

## 10. Risks and open questions

- **The §2 tension is real and strategic.** The roadmap drifts toward observability (trend analysis, plan-diff alerts, cost-per-query dashboard) and the product *removed MCP for a web dashboard*. If the no-human lane is the differentiator, this is backwards: the machine interface (MCP, intent-level) is the product surface, and the web UI should be the *audit/approval* view of decisions, not telemetry. **This is a positioning decision, not a feature decision** — flag it to leadership.
- **Postgres has no plan pinning.** Any plan-stability feature caps at rung 2-3 (hints via `pg_hint_plan`, forced re-plan, stats manipulation) — be honest; don't sell a FORCE_LAST_GOOD_PLAN equivalent that Postgres can't back. Kept out of the deep-spec set for this reason.
- **Thin clones are the pivotal, expensive dependency.** Much of the high-autonomy value (rehearsal-backed verification) is gated on it. The build-vs-integrate (Database Lab self-host vs. provider branching) decision materially shapes the roadmap.
- **Verify-and-revert coupling (Azure's lesson).** Autonomy and verification must be *coupled* — pg_sage should refuse to auto-apply a change it can't verify (Azure disables verify-and-revert for manual T-SQL; the inverse must hold here). This constrains the executor design.
- **Where the anti-dashboard filter fights user need.** Some operators genuinely want the trend charts and cost views. The resolution is not to build them as pg_sage deliverables but to *emit the signals* and let a generic observability stack (Grafana over the existing Prometheus endpoint) render them — pg_sage acts, the dashboard the user already has displays. State this explicitly to avoid a filter-vs-user standoff.
- **Deadline-override is the riskiest new autonomy.** Letting the custodian act *outside* the maintenance window on an XID/disk deadline is powerful and correct, but it's the one place the agent overrides the operator's stated window. It must be policy-opt-in, loudly evidenced, and bounded. Get this wrong and it's a trust-destroyer; get it right and it's the clearest "the DB kept itself alive with nobody watching" story.
- **Open: how to bootstrap "intended state" for a DB nobody declared.** F5/F6 lean on an intended-schema baseline; for a truly ownerless DB, infer it from current state + agent declarations, and treat the baseline itself as evolving evidence.

---

## 11. Sources (retrieved 2026-07-22)

- Azure SQL Automatic Tuning overview — https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql
- SQL Server Automatic Tuning / Automatic Plan Correction — https://learn.microsoft.com/en-us/sql/relational-databases/automatic-tuning/automatic-tuning?view=sql-server-ver17
- Oracle DBMS_AUTO_INDEX (19c) — https://docs.oracle.com/en/database/oracle/oracle-database/19/arpls/DBMS_AUTO_INDEX.html
- Oracle Automatic Indexing (ORACLE-BASE) — https://oracle-base.com/articles/19c/automatic-indexing-19c
- OtterTune shutdown (dbtune analysis) — https://www.dbtune.com/blog/ottertune ; (dang.ai) — https://dang.ai/tool/ai-database-optimizer-ottertune ; VLDB OtterTune paper — https://www.vldb.org/pvldb/vol11/p1910-zhang.pdf
- AWS DevOps Guru for RDS — recommendations — https://docs.aws.amazon.com/devops-guru/latest/userguide/working-with-rds.analyzing.recommend.html
- pganalyze Indexing Engine (CP model) — https://pganalyze.com/docs/indexing-engine/cp-model ; Index Advisor v3 — https://pganalyze.com/blog/index-advisor-v3
- Postgres.ai Database Lab Engine — https://github.com/postgres-ai/database-lab-engine/blob/master/README.md ; migration testing — https://postgres.ai/products/database-migration-testing
- pgroll internals (Xata) — https://xata.io/blog/pgroll-internals ; expand/contract — https://xata.io/blog/pgroll-expand-contract
- PlanetScale deploy requests — https://planetscale.com/docs/vitess/schema-changes/deploy-requests ; instant deploy requests — https://planetscale.com/blog/instant-deploy-requests
- Squawk rules — https://squawkhq.com/docs/rules ; safe migrations — https://squawkhq.com/docs/safe_migrations
- HypoPG docs — https://hypopg.readthedocs.io/en/rel1_stable/hypothetical_indexes.html
- Replication slot WAL disk-full: Gunnar Morling — https://www.morling.dev/blog/insatiable-postgres-replication-slot/ ; EDB "don't let slots kill your primary" — https://www.enterprisedb.com/blog/postgresql-13-dont-let-slots-kill-your-primary
- XID wraparound: Sentry — https://blog.sentry.io/transaction-id-wraparound-in-postgres ; Mailchimp/Mandrill — https://mailchimp.com/what-we-learned-from-the-recent-mandrill-outage/ ; Bytebase — https://www.bytebase.com/blog/postgres-transaction-id-wraparound/
- pg_sage repository (read directly): `internal/executor/*`, `internal/optimizer/*`, `internal/config/config.go` (MCP-retired error), `roadmap.md`, `README.md`.
