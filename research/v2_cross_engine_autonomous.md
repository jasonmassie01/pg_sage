# Cross-Engine Autonomous DBA Mechanics — Auto-Apply, Verify, Revert

> Research date: 2026-06-10
> Author: pg_sage research
> Companion to: `v1_cross_db_ai_landscape.md` (Snowflake, Databricks, BigQuery, Mongo Atlas advisory surface, Cockroach MCP, PlanetScale deploy-requests, Neon, Supabase)
> Ground truth: `docs/REVERSE_SPEC.md` (pg_sage as-built)
> Purpose: Extract the **closed-loop auto-apply safety mechanics** from non-Postgres engines — the verify-and-revert IP — and map each onto pg_sage's sidecar+executor without a Postgres extension.

`v1_cross_db_ai_landscape.md` already covered the *advisory* surface of Snowflake, BigQuery, Mongo Atlas, Cockroach, PlanetScale, Neon, Supabase, Crunchy, ClickHouse, and the deploy-request/30-min-revert UX patterns. This file does **not** re-cover those. It goes one level deeper on the systems that actually **auto-execute and auto-revert** physical changes: Oracle automatic indexing + SPM, Azure SQL automatic tuning + FORCE LAST GOOD PLAN, SQL Server Automatic Plan Correction, AWS Aurora/RDS, AlloyDB/Cloud SQL, CockroachDB admission control, TiDB, Mongo Autopilot's rollout/rollback specifics, and the Redshift/Vertica auto-optimizers. For each: what is automated, the autonomy level, **the safety mechanism** (the transferable IP), and a Postgres implementation sketch for pg_sage.

A framing correction worth verifying up front: pg_sage's REVERSE_SPEC describes the executor's auto-rollback as reacting to *coarse global metrics* after a `RollbackWindowMinutes` window, with no per-statement performance attribution and no pre-apply verification. Every system below that auto-applies physical changes does the opposite — it ties the revert decision to the **specific SQL statements** the change was supposed to help, and most verify *before* exposing the change to the live optimizer. That gap is the single most important finding here.

---

## 1. Oracle Automatic Indexing — The Gold Standard Closed Loop

Oracle's automatic indexing (`DBMS_AUTO_INDEX`, GA in 19c, hardened through 23ai/26ai) is the most complete auto-create → verify → auto-drop loop shipping anywhere. It is worth studying as a state machine because pg_sage's optimizer produces the same *input* (candidate index recommendations) but stops at "emit a finding."

### The lifecycle (verified against ORACLE-BASE and Richard Foote's teardown)

A background task runs **every 15 minutes** and walks a five-stage pipeline ([oracle-base — Automatic Indexing 19c](https://oracle-base.com/articles/19c/automatic-indexing-19c), [Richard Foote — Automatic Indexing methodology](https://richardfoote.wordpress.com/2019/07/24/oracle-19c-automatic-indexing-methodology-introduction-after-today/)):

1. **Capture.** Identify candidate indexes from column usage in the last interval's SQL workload.
2. **Create invisible/unusable.** Candidates are first created as **INVISIBLE and UNUSABLE** metadata-only objects, then hard-parsed against the captured SQL to check whether the cost-based optimizer would even *consider* them. Candidates the CBO would ignore are discarded before any physical build — this is a cheap pre-filter that avoids building useless indexes.
3. **Build + verify.** Surviving candidates are physically built as **INVISIBLE/USABLE** (so they exist and are maintained, but the *general* workload cannot see them). Each is verified via **SQL Performance Analyzer** against a **SQL Tuning Set** of the captured statements — Oracle literally re-runs the captured SQL with and without the candidate and compares plans/cost ([Oracle blog — Automatic Indexing cheat sheet](https://blogs.oracle.com/datawarehousing/post/automatic-indexing-cheat-sheet)).
4. **Decision per statement.** If *all* captured statements improve, the index is made **VISIBLE** for the whole database. If *some* statements regress, the index is made visible **only** for the statements that improved (via SQL plan directives) and the regressing statements are **blocklisted** so they are never re-considered for that index. If *nothing* improves, the index stays invisible and is marked unusable.
5. **Auto-drop.** Indexes that delivered no benefit are made unusable and dropped. Indexes that were used but later go cold are retained for `AUTO_INDEX_RETENTION_FOR_AUTO` = **373 days** (53 weeks) by default, then dropped. Manually-created indexes have a separate, longer retention.

Key config: `AUTO_INDEX_MODE` (`IMPLEMENT` = auto-apply, `REPORT ONLY` = advisory, `OFF`), `AUTO_INDEX_SPACE_BUDGET` (% of tablespace it may consume), `AUTO_INDEX_SCHEMA` (allow/deny lists). The **`REPORT ONLY` vs `IMPLEMENT` split is exactly pg_sage's advisory-vs-autonomous trust ramp**, but Oracle ties it to a single per-database knob rather than a time-based ramp.

### Autonomy level

**Fully autonomous, auto-apply by default** in Autonomous Database; opt-in (`IMPLEMENT`) on-prem. No human in the loop.

### The transferable IP

The genius is **invisible-but-maintained verification**. The index physically exists and is kept current by DML, so the verification measures *real* execution against *real* current data — not an estimate — yet the broad workload is shielded because the index is invisible. The decision is **per-statement**, not global, and failures are **blocklisted** so the system doesn't thrash re-proposing the same losing index. Drop is driven by *measured non-use over a long window*, not a guess.

### pg_sage implementation sketch

Postgres has the primitives to replicate ~80% of this without an extension:

- **Invisible-but-maintained** maps to Postgres in two ways. (a) HypoPG gives a *hypothetical* index for the cheap pre-filter (stage 2) — does the planner even pick it? pg_sage already uses HypoPG in the optimizer. (b) For the *real* verification (stage 3), build the index with `CREATE INDEX CONCURRENTLY`, then immediately set it invisible-equivalent. Postgres lacks Oracle's true `INVISIBLE` flag, but `ALTER INDEX ... SET (enable = ...)` doesn't exist either; the closest is **per-session `SET enable_indexscan`** during a controlled A/B, OR build the index and gate visibility by attaching it under a feature flag. The honest gap: Postgres can't hide a built index from *other* sessions, so true invisible verification requires the index to be live for everyone during the test window.
- **The pragmatic substitute** is what Azure does (§2): build it for real, then run a verification window measuring the *captured statements* (from `pg_stat_statements` query IDs) before/after, and **`DROP INDEX CONCURRENTLY` if they didn't improve.** pg_sage already has a `RollbackWindowMinutes` mechanism — the fix is to attribute the verdict to the specific `queryid`s the optimizer cited, not global metrics.
- **Blocklist** is trivial and high-value: persist `(queryid, index_definition) → rejected` in a `sage.*` table so the optimizer never re-proposes a losing index. pg_sage's finding dedup keys on `(Category, ObjectIdentifier)` — extend that with a rejection ledger.
- **373-day retention drop** maps directly to pg_sage's existing unused-index rule; gate auto-drop on a long, configurable idle window read from `pg_stat_user_indexes.idx_scan`.

---

## 2. Azure SQL Database Automatic Tuning — The Auto-Apply/Revert Template

Azure SQL is the most directly portable design because, unlike Oracle, it runs at fleet scale over *millions* of databases and its verify/revert loop is built around the same statistics surface Postgres exposes (Query Store ≈ `pg_stat_statements` + plan history). Microsoft published the underlying research ([MSR — Automatically Indexing Millions of Databases](https://www.microsoft.com/research/uploads/prod/2019/02/autoindexing_azuredb.pdf)).

### What's automated (three options)

From the Azure docs ([Automatic tuning overview](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql)):

| Option | Behavior | Default |
|---|---|---|
| **CREATE INDEX** | Identifies + creates indexes, **auto-verifies** query perf improved. Skips if it would push space >90% of max, or if clustered index/heap > 10 GB. | **Off** |
| **DROP INDEX** | Drops indexes unused over **90 days** + duplicate indexes. Never drops unique/PK indexes. Auto-disables if index hints or partition switching are present. | **Off** |
| **FORCE LAST GOOD PLAN** | Forces the prior good plan when a regression is detected (see §3). | **On** |

### The verify-and-revert mechanics (the IP)

This is the design template. When automatic tuning applies a recommendation autonomously:

1. **Apply only during low utilization.** Index creation is scheduled for periods of **low CPU, Data IO, and Log IO**. The system gives user workload the **highest resource priority** and can temporarily **"Disabled by the system"** to protect a hot workload ([Automatic tuning overview](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql)). Once an index build starts during a low window, it will *not* pause/cancel even if load spikes (cancelling mid-build is worse than finishing).
2. **Validate against the specific workload.** After applying, it measures query performance and compares to the pre-change baseline. **The validation window is 30 minutes to 72 hours**, longer for infrequently-executing queries (it needs enough executions to be statistically confident).
3. **Auto-revert on regression.** "If there's no improvement, or in the unlikely case performance regresses, changes are promptly reverted." **If at any point during validation a regression is detected, changes are reverted immediately** — it does not wait for the window to close.
4. **Critical limitation worth stealing the inverse of:** if you apply the *same* recommendation via raw T-SQL instead of the autonomous path, **the verification and auto-revert do not run.** The safety is a property of the *managed apply path*, not the DDL. → For pg_sage this means: the executor's apply path must *own* the verification; an index a human creates by hand gets no auto-revert, and that's the correct boundary.

History is retained 21 days and every action emits a reversible T-SQL script.

### Autonomy level

**Auto-apply, opt-in per option,** configurable at server level with database inheritance. FORCE LAST GOOD PLAN is on by default; the two physical-DDL options are off by default — Microsoft's own risk ranking: plan forcing is safe enough to default-on, index DDL is not.

### pg_sage implementation sketch

This is almost a spec for pg_sage's next executor iteration:

- **Low-utilization scheduling.** pg_sage's collector already snapshots CPU/IO proxies. Gate MODERATE+ actions on a rolling "is the database quiet now?" check (e.g., recent `pg_stat_statements` call-rate and connection count below a threshold), and expose a maintenance window — REVERSE_SPEC notes the current window parser only understands cron+`always`, so range windows need finishing anyway.
- **Per-queryid validation window.** Replace the coarse global-metric rollback with: snapshot `mean_exec_time` (or `total_exec_time/calls`) for the **exact `queryid`s** the optimizer cited, apply the index, then sample those queryids over a 30-min-to-72-hr window scaled by their call frequency. Revert (`DROP INDEX CONCURRENTLY`) if the cited queries didn't improve by a configured margin OR if any regressed. This directly fixes the REVERSE_SPEC gap ("regression detection uses coarse global metrics, masks per-table regressions").
- **Immediate revert on regression.** Don't wait for the window to expire — if a cited query's mean time crosses a regression threshold mid-window, revert now.
- **Space guard.** Port the ">90% of max size → skip" and ">10 GB heap → skip" guards as configurable pre-flight checks; cheap and prevents the worst-case disk-full incident.
- **Apply-path ownership.** Make the verify/revert a property of the executor, not the DDL — consistent with REVERSE_SPEC's note that the live executor and the richer `cases`/`ActionContract` model are disconnected.

---

## 3. SQL Server Query Store + Automatic Plan Correction (FORCE LAST GOOD PLAN internals)

This is the engine *under* Azure's FORCE LAST GOOD PLAN, and the second core design template. Postgres has no native plan store, so this is the hardest to replicate — but the *detection logic* is portable and the feature is the single highest-value thing Postgres lacks.

### What it does

`ALTER DATABASE [db] SET AUTOMATIC_TUNING (FORCE_LAST_GOOD_PLAN = ON);` ([Automatic tuning — SQL Server](https://learn.microsoft.com/en-us/sql/relational-databases/automatic-tuning/automatic-tuning?view=sql-server-ver17)). Query Store records every plan a query has used plus per-plan execution stats. When a query's *current* plan regresses versus a *prior known-good* plan, the engine forces the old plan automatically, then keeps monitoring to confirm the forced plan is actually better — and **un-forces** if it isn't ([dbanuggets — How automatic plan correction works](https://dbanuggets.com/2022/01/05/query-store-fundamentals-how-automatic-plan-correction-works/)).

### The detection mechanics (the IP)

- **Regression test.** Before SQL Server 2022 CU4, APC used a **sigma test**: it compared mean CPU time of the current plan vs the last known-good plan and flagged a regression when the difference exceeded **three standard deviations** ([SQLYARD — APC regression detection](https://sqlyard.com/2026/05/18/automatic-plan-correction-how-sql-server-detects-and-fixes-query-regressions/)). 2022 CU4+ improved the statistical model. The point: regression is a **statistical claim about a distribution**, not a single-sample spike.
- **Forcing criteria.** It only auto-forces when **estimated CPU gain > 10 seconds**, or the new plan has *more errors* than the candidate plan — i.e., a materiality threshold so it doesn't churn on noise ([dbanuggets](https://dbanuggets.com/2022/01/05/query-store-fundamentals-how-automatic-plan-correction-works/)).
- **Verify-after-force.** After forcing, it continues to monitor via Query Store; if the forced plan stops winning, it releases the force. The loop is self-correcting in *both* directions.

### Autonomy level

**Auto-apply** (when enabled). Default-on in Azure SQL Database; opt-in in box SQL Server. Microsoft treats plan-forcing as lower-risk than index DDL because forcing a plan is instantly reversible (un-force) and touches no physical structure.

### pg_sage implementation sketch

Postgres has no Query Store, so this is a *build*, not a *wire-up* — but it's the marquee differentiator:

- **Plan history table.** Capture `EXPLAIN (FORMAT JSON)` for the top-N queries by `total_exec_time` on a schedule, keyed by `pg_stat_statements.queryid`, into a `sage.plan_history` table with per-snapshot `mean_exec_time`. pg_sage already has `explain_cache`/`explain_results` tables (REVERSE_SPEC §5) — this is an extension of existing infrastructure.
- **Regression detection.** Port APC's logic directly: for each queryid, maintain a rolling distribution of `mean_exec_time` and flag a regression when the latest exceeds the prior-good mean by **>3σ** AND the absolute gain would exceed a materiality threshold (the "10 CPU-seconds" analog). This is a deterministic Tier-1 rule — no LLM needed.
- **The "force" problem.** Postgres can't force an arbitrary stored plan natively. The honest options: (a) ship guidance via `pg_hint_plan` if the extension is present (degrade gracefully if not — pg_sage's no-extension stance means this is *advisory* when absent); (b) detect the *cause* of the regression (stale stats → run `ANALYZE`; bad plan after a new index → consider dropping it; parameter-sniffing-style variance) and remediate the cause rather than pin the plan. (c) Surface "your plan regressed on $queryid at $time, here's the before/after plan diff" as a finding even when no auto-fix is possible — this alone beats every Postgres advisor, none of which track plan regression over time.
- **Materiality + statistical thresholds** keep it from flapping — directly addresses the REVERSE_SPEC concern about action-induced flapping (the 2-min grace window is the crude version of this).

---

## 4. Oracle SQL Plan Management (SPM) — Capture/Evolve/Verify Baselines

SPM is Oracle's *other* plan-stability mechanism and the conceptual ancestor of APC. It's worth a separate note because its **evolve-then-accept** workflow is a cleaner template than APC for a system (like pg_sage) that can run controlled comparisons.

### How it works

`OPTIMIZER_CAPTURE_SQL_PLAN_BASELINES = TRUE` auto-captures baselines for repeatable statements ([oracle-base — SPM 11gR1](https://oracle-base.com/articles/11g/sql-plan-management-11gr1)). The lifecycle:

1. **Capture.** First execution logs a signature. Second execution creates an **accepted** baseline. Subsequent executions whose plan differs are added as **non-accepted** plans — the optimizer keeps using the accepted plan, so a new (possibly worse) plan can never silently take over.
2. **Evolve.** The **SPM Evolve Advisor** *executes* the non-accepted alternative plans and compares their measured performance to the accepted baseline. Only plans that demonstrably perform **at least as well** get promoted to accepted ([Oracle docs 19c — Managing SQL Plan Baselines](https://docs.oracle.com/en/database/oracle/oracle-database/19/tgsql/managing-sql-plan-baselines.html)).

### The transferable IP

**Conservative by construction:** a new plan is *guilty until proven innocent*. The system never adopts a new plan on faith — it runs it, measures it, and promotes only on proof. This is the inverse of Postgres's behavior, where the planner adopts whatever the cost model picks *right now* with no memory of whether last week's plan was faster.

### pg_sage implementation sketch

pg_sage's executor can run controlled comparisons that Postgres's planner can't:

- For a recommended index, the "evolve" step is exactly Azure's per-queryid A/B: measure the cited queries with and without, promote (keep) only on proof.
- A `sage.accepted_plans` table mirroring baselines gives the regression detector (§3) a "known good" reference. The collector already captures the data; the missing piece is *durable per-query plan/latency memory* and a promotion gate.
- Frames pg_sage's whole executor philosophy correctly: **default to the known-good state, require measured proof to change it, retain the ability to revert.**

---

## 5. AWS Aurora / RDS — Observability-First, Conservative Auto-Apply

AWS deliberately stops short of auto-applying schema changes. Its autonomy is in *capacity* and *storage*, not *DDL* — a revealing risk posture from the largest managed-Postgres operator.

### What ships

- **Performance Insights + DevOps Guru for RDS.** DevOps Guru is ML over Performance Insights data; it produces **proactive insights** (warn before a problem) and **reactive anomaly detection** for Aurora PostgreSQL/MySQL, surfacing things like connection storms and sustained high `DBLoad`/CPU with recommendations ([AWS docs — DevOps Guru for RDS](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/devops-guru-for-rds.html), [AWS — Proactive Insights](https://aws.amazon.com/blogs/devops/proactive-insights-with-amazon-devops-guru-for-rds/)). **Advisory only** — it never auto-fixes.
- **Aurora Serverless v2 autoscaling.** Scales capacity in fine-grained **ACU** increments by continuously monitoring load. **Autonomous**, but it's a capacity dial, not a database change.
- **Aurora Optimized Reads.** A **tiered cache** that spills buffer-pool pages about to be evicted onto local NVMe, extending cache up to 5× instance memory for up to 8× latency improvement on I/O-bound reads ([AWS — Aurora Optimized Reads for PostgreSQL](https://aws.amazon.com/blogs/database/new-amazon-aurora-optimized-reads-for-aurora-postgresql-with-up-to-8x-query-latency-improvement-for-i-o-intensive-applications/)). Autonomous once enabled; it's a *substrate* feature.
- **RDS Optimized Writes.** **Torn Write Prevention** — guarantees 16 KiB writes aren't torn on crash/power-loss using the Nitro system, letting MySQL skip the doublewrite buffer ([InfoQ — RDS Optimized Reads/Writes](https://www.infoq.com/news/2023/01/aws-rds-optimized-reads-writes/)). Hardware-level, not a brain feature.

### Autonomy level

**Capacity = autonomous; physical/DDL = advisory.** AWS, with the most production telemetry of anyone, chose *not* to auto-create indexes or auto-tune config. That validates pg_sage's trust ramp as a *differentiator*, not table stakes — and signals where the bar for "safe enough to auto-apply" sits.

### Transferable IP / pg_sage sketch

- **DevOps Guru's proactive-vs-reactive split** is a finding-severity taxonomy pg_sage should adopt: "this *will* become a problem" (sequence exhaustion forecast, bloat trend) vs "this *is* anomalous now." pg_sage has forecasters; the proactive framing should drive the UX.
- **Optimized Reads/Writes are explicitly out of scope** (substrate, per `v1` synthesis §C) — but pg_sage should *advise on them*: "this RDS instance is I/O-bound; enabling Optimized Reads (db.r6gd) would help" is a legitimate finding, since pg_sage can detect the I/O-bound symptom that the feature addresses.
- **The conservative posture is the lesson:** AWS auto-applies only changes that are instantly and safely reversible (scale ACUs down) and never touches schema. pg_sage's edge is doing the schema part — but only with §2's verify/revert rigor.

---

## 6. Google AlloyDB + Cloud SQL — Auto-Columnarization (Auto), Index Advisor (Advisory)

AlloyDB is the most architecturally relevant because it *is* Postgres, so its choices about what to automate are a direct signal.

### What ships

- **Index Advisor.** Runs **every 24 hours** by default, analyzing the workload; results land in the `google_db_advisor_recommended_indexes` view with **per-index storage estimates and affected-query counts**. `google_db_advisor_recommend_indexes()` triggers on-demand analysis of the top-100 queries. Integrated into Query Insights for one-click create ([AlloyDB — Use the index advisor](https://cloud.google.com/alloydb/docs/use-index-advisor), [Google Cloud blog — How the AlloyDB Index Advisor works](https://cloud.google.com/blog/products/databases/how-the-alloydb-index-advisor-helps-make-smart-indexes)). **Advisory** — AlloyDB does *not* auto-create indexes. Notably, even Google-on-Postgres stops at advisory for index DDL.
- **Auto-columnarization.** The columnar engine **automatically manages column-store content** — analyzes query patterns hourly and populates the column store with the optimal columns; **enabled by default on new instances** ([AlloyDB — Manage column store with auto-columnarization](https://cloud.google.com/alloydb/docs/columnar-engine/manage-content-recommendations)). This is **fully autonomous** — but it's a *cache-population* decision (instantly reversible, no DDL, no plan impact on the row store), which is *why* Google was comfortable defaulting it on. Same risk logic as Azure (plan-force default-on) and AWS (ACU autoscale default-on): **auto-apply only what's instantly reversible and side-effect-free.**

### Transferable IP / pg_sage sketch

- The **storage estimate + affected-query count** in the recommendation view is the impact-ranking pg_sage's findings need (echoes v1's Mongo lesson). pg_sage's optimizer has the affected-query data; surface it as a numeric score.
- **Auto-columnarization's "analyze patterns, populate the reversible cache, default-on" pattern** maps to pg_sage's *config* tier, not DDL: things like prewarming hot tables (`pg_prewarm` if available) or recommending `shared_buffers` adjustments are the reversible analog. The lesson is the **risk gradient**: cache/capacity = auto; plan-force = auto; index DDL = advisory→auto-with-verify; destructive DDL = always gated.

---

## 7. CockroachDB, TiDB — NewSQL Autonomous Primitives

These engines automate at a different layer (storage/admission), but two ideas port.

### CockroachDB Admission Control

A built-in scheduler that **prioritizes work during overload** — critical foreground SQL runs unimpeded while background/elastic work (backups, stats, rebalancing) is throttled. v25.1 added **replication admission control** that paces Raft entries to the slowest follower and isolates regular vs elastic traffic ([Cockroach — Admission Control](https://www.cockroachlabs.com/docs/stable/admission-control), [Cockroach — How admission control protects against overload](https://www.cockroachlabs.com/blog/admission-control-unexpected-overload/)). **Auto-stats:** on by default, ANALYZE-equivalent runs automatically.

**Transferable IP:** *self-throttling under load* is a safety mechanism pg_sage should adopt for its *own* maintenance actions. When the target DB is hot, pg_sage's `CREATE INDEX CONCURRENTLY`/`VACUUM`/`REINDEX` are exactly the "elastic" work that should be paced or deferred — this is the generalization of Azure's "apply only during low utilization" (§2) into a continuous back-pressure signal. pg_sage already has `ddlSem=3` and an ANALYZE semaphore (REVERSE_SPEC §4); the missing piece is *load-aware* admission: read live `DBLoad`/active-backend count and shed/defer maintenance when the DB is busy. Combined with REVERSE_SPEC's noted HA gap (replica gating wired to nothing), this is a coherent "don't add load to a struggling primary" story.

### TiDB Auto-Analyze

TiDB **auto-schedules ANALYZE** when the modified-row ratio exceeds `tidb_auto_analyze_ratio`, but **only within `tidb_auto_analyze_start_time`–`tidb_auto_analyze_end_time`** ([TiDB — Introduction to Statistics](https://docs.pingcap.com/tidb/stable/statistics/)). **Auto-apply, change-driven, window-gated.**

**Transferable IP:** TiDB triggers maintenance on **change volume**, not a fixed clock, and confines it to a window. pg_sage's vacuum/analyze advising should be driven by `n_mod_since_analyze`/dead-tuple ratio (change-driven, which it partly is) **and** confined to a maintenance window (which REVERSE_SPEC says is currently broken for range strings). Fixing the window parser plus change-driven triggers ≈ TiDB's model.

---

## 8. MongoDB Atlas Autopilot — Auto-Index Rollout + Drop-Recommendation Loop

v1 covered Autopilot's existence; here is the rollout/safety detail. (Note: Atlas Serverless was retired Jan 22 2026 and instances migrated to Flex/Dedicated, but the Autopilot mechanics moved to broader tiers.)

### Mechanics

Autopilot analyzes recent workload, identifies **slow queries (>100 ms)**, and auto-creates indexes that would accelerate them, **building in the background in a performant manner** ([MongoDB — Database Automation: Automated Indexes](https://www.mongodb.com/developer/products/atlas/mongodb-automation-index-autopilot/), [MongoDB — Auto-Index for Serverless](https://www.mongodb.com/blog/post/introducing-auto-index-creation-atlas-serverless-instances)). Two safety governors:

- **Write-amplification cap:** at most **four indexes per collection**, to bound the DML/write overhead an over-eager auto-indexer would impose.
- **Closed-loop drop:** the Performance Advisor flags an index as **unused if it hasn't supported a query in ≥7 days** after creation/restart and recommends dropping it ([MongoDB — Review Drop Index Recommendations](https://www.mongodb.com/docs/atlas/performance-advisor/drop-indexes/)). So the create-loop and drop-loop together form a full lifecycle: create on demonstrated need, drop on demonstrated non-use.

### Transferable IP / pg_sage sketch

- **The per-table index cap** is a safety knob pg_sage lacks and should add: refuse to auto-create an Nth index on a table (configurable, default ~4–5) because each index taxes every write. This is a *write-side* safety counterpart to the read-side verification — and prevents the "auto-indexer death-by-a-thousand-indexes" failure mode.
- **The 7-day-unused drop loop** is the cheap, measurable auto-drop pg_sage can ship now from `pg_stat_user_indexes.idx_scan` — no Oracle-style verification needed, just measured non-use over a window. This is strictly easier than the create path and should ship first.
- **The >100ms slow-query trigger** mirrors pg_sage's existing slow-query rule; aligning the auto-index trigger to it closes the loop.

---

## 9. Snowflake / BigQuery / Redshift / Vertica — Warehouse Auto-Optimizers

Mostly covered in v1; the **physical-layout auto-optimizers** add one safety idea worth recording.

- **Redshift Automatic Table Optimization (ATO).** Auto-applies **sort keys and distribution keys** by observing query interaction, **altering tables within hours** of creation with **minimal impact**, and continuously updating recommendations as the workload shifts. Recent ML models let it recommend *sooner*, before observing the full workload ([AWS — Redshift Advisor sort/dist key recommendations](https://aws.amazon.com/about-aws/whats-new/2023/12/amazon-redshift-advisor-sort-distribution-key-recommendations/), [AWS — Automate Redshift tuning with ATO](https://aws.amazon.com/blogs/big-data/automate-your-amazon-redshift-performance-tuning-with-automatic-table-optimization/)). **Auto-apply**, but the changes are background reorganizations on a column store — low-blast-radius by architecture.
- **Vertica Database Designer / Snowflake Automatic Clustering / BigQuery recommenders** — covered in v1. Snowflake auto-clusters (autonomous, reversible reorg); BigQuery stops at advisory recommenders for partition/cluster (validating advisory-for-layout); Vertica's Database Designer is a one-shot advisor, not a continuous loop.

**Transferable IP:** ATO's "apply incrementally in the background, keep monitoring, keep updating as workload shifts" is the **continuous-reconciliation** pattern — the change is never "done," it's a control loop that re-evaluates. pg_sage's analyzer is already a loop; the lesson is that *executor actions should also be continuously re-evaluated*, not fire-and-forget (which is what the current rollback-window-then-stop design is).

---

## Synthesis: The Universal Auto-Apply Safety Pattern

Every system that auto-applies physical changes shares the same five-part skeleton. pg_sage implements parts of 1 and 5; the gold is in 2–4.

1. **Materiality + statistical gate before acting.** APC: >3σ regression AND >10 CPU-sec gain. Azure: skip if space >90%, heap >10 GB. Mongo: >100ms, ≤4 indexes/collection. → *Don't act on noise; bound blast radius.*
2. **Apply during low utilization / under admission control.** Azure low-IO windows; Cockroach admission control; TiDB time windows. → *Never add load to a hot database.*
3. **Verify against the specific statements the change targets, over a frequency-scaled window.** Oracle SQL Tuning Sets; Azure 30min–72hr per-query validation; SPM evolve. → *This is the part pg_sage is missing — its rollback is global, not per-queryid.*
4. **Auto-revert immediately on regression; keep the known-good as the default.** Azure immediate revert; APC un-force; SPM guilty-until-proven. → *Default to known-good, require proof to change, retain instant revert.*
5. **Auto-drop on measured non-use over a long window; blocklist proven losers.** Oracle 373-day retention + blocklist; Azure 90-day; Mongo 7-day. → *pg_sage has the unused-index rule; add the blocklist.*

The risk gradient every vendor independently converged on, from most-auto to most-gated: **reversible cache/capacity (auto, often default-on)** → **plan forcing (auto, reversible)** → **index creation (auto only with per-query verify+revert)** → **index/table drop (auto only on long measured non-use)** → **destructive DDL (always human-gated)**. pg_sage's trust ramp is the right *transition* mechanism; this gradient is the right *destination per action class*.

---

## Top 5 Ideas to Steal (with Postgres implementation sketch)

1. **Per-queryid verify-and-revert window (Azure §2 + Oracle §1).** Replace the executor's coarse global-metric rollback with statement-level attribution: snapshot `pg_stat_statements.mean_exec_time` for the **exact `queryid`s the optimizer cited**, apply the `CREATE INDEX CONCURRENTLY`, sample those queryids over a 30-min-to-72-hr window scaled by call frequency, and `DROP INDEX CONCURRENTLY` if the cited queries didn't improve by a margin OR any regressed — reverting *immediately* on mid-window regression. This directly closes the REVERSE_SPEC §4 gap ("regression detection uses coarse global metrics, masks per-table regressions") and is the single highest-value change. *Effort: medium. The executor, rollback goroutine, and pg_stat_statements collection already exist; the work is wiring the verdict to cited queryids and a per-query baseline table.*

2. **Plan-regression detector via a Postgres "mini Query Store" (SQL Server APC §3 + SPM §4).** Build `sage.plan_history` (extend existing `explain_cache`/`explain_results`): periodically capture `EXPLAIN (FORMAT JSON)` + `mean_exec_time` per top-N `queryid`, maintain a rolling distribution, and fire a Tier-1 finding when latest exceeds prior-good by **>3σ AND** the absolute gain clears a materiality threshold. Auto-remediate the *cause* (stale stats → `ANALYZE`; regressed-after-index → flag/revert the index) since Postgres can't force a stored plan without `pg_hint_plan`; degrade to "here's the before/after plan diff" finding when no auto-fix exists. *No Postgres advisor tracks plan regression over time — instant differentiator. Effort: high (it's a build), but reuses the explain infrastructure.*

3. **Load-aware admission control for pg_sage's own maintenance (Cockroach §7 + Azure low-util §2).** Before any MODERATE+ action, read a live load signal (active backends, `pg_stat_statements` call-rate, replication lag) and **defer/pace** `CREATE INDEX`/`VACUUM`/`REINDEX` when the primary is hot — generalizing Azure's "low-utilization window" into continuous back-pressure. Pairs with finishing the broken maintenance-window range parser (REVERSE_SPEC §3) and wiring the dormant `ha.InSafeMode()`/replica gating. *Effort: low-medium; `ddlSem` and the ANALYZE semaphore are already there — add a load gate in `ShouldExecute`.*

4. **Rejection blocklist + per-table index cap (Oracle §1 + Mongo §8).** Two cheap write-side governors: (a) a `sage.rejected_recommendations` ledger keyed on `(queryid, index_definition)` so the optimizer never re-proposes an index that already failed verification (stops thrashing); (b) a configurable per-table index ceiling (default ~5) so the auto-indexer can't death-by-a-thousand-indexes a write-heavy table. *Effort: low; both are small tables/checks bolted onto the existing optimizer + finding dedup.*

5. **Measured-non-use auto-drop loop (Mongo 7-day §8 + Oracle 373-day §1 + Azure 90-day §2).** Ship the *drop* side of the index lifecycle first (it's strictly easier than create): auto-`DROP INDEX CONCURRENTLY` for non-unique, non-constraint indexes with `idx_scan=0` over a long configurable window, with the same verify/revert safety as create (it's reversible — recreate if a regression appears). REVERSE_SPEC notes the unused-index rule exists but index-bloat/REINDEX advising is unwired; this completes the lifecycle. *Effort: low-medium; the detection rule already exists, the executor + rollback path already exist.*

---

## Open Questions

1. **Can pg_sage verify an index without exposing it to the whole workload?** Oracle's INVISIBLE-but-maintained index is the crux of safe verification, and Postgres has no equivalent — a built index is live for every session. Is HypoPG's hypothetical-only check (cheap, no real data) "good enough" as the pre-filter, accepting that the *real* verification must be the post-apply A/B (Azure's approach)? Or is there a per-session trick (`enable_indexscan` toggling in a controlled comparison harness) that approximates invisibility?

2. **What's the minimum viable plan store without an extension?** APC/SPM depend on engine-level plan capture. Capturing `EXPLAIN` for top-N queryids on a schedule is feasible over the wire, but (a) how much overhead does periodic `EXPLAIN` add, (b) `EXPLAIN` without `ANALYZE` gives estimates not actuals — is estimate-drift a good enough regression signal, and (c) does `EXPLAIN (ANALYZE)` on a production query to get actuals risk side effects on non-`SELECT` statements?

3. **Where exactly is the per-queryid verdict statistically valid?** Azure's 30-min-to-72-hr window scales with execution frequency. For a query that runs 10×/day, how long must pg_sage wait before a "didn't improve" verdict is trustworthy rather than noise? Need a concrete min-executions/confidence rule, or the auto-revert will itself become a source of flapping.

4. **Should plan-forcing be in scope at all given the no-extension constraint?** FORCE LAST GOOD PLAN is the highest-value Postgres gap, but forcing requires `pg_hint_plan`. Does pg_sage relax its no-extension stance for an *optional* extension (advisory when absent, executor-capable when present), the way it already conditionally uses HypoPG? What's the product story when the extension isn't installed?

5. **How does load-aware admission interact with the trust ramp and emergency stop?** If pg_sage defers maintenance under load, a chronically-busy database might *never* hit a low-load window — silently starving needed vacuums. Does that need an escalation ("I've been unable to vacuum table X for N days due to sustained load") so deferral doesn't become silent neglect — the inverse of the safety the deferral was meant to provide?

6. **What's the right default per action class — and does that obsolete the time-based ramp?** Every vendor defaults-on the reversible classes (cache, plan-force, capacity) and gates the rest by *reversibility*, not by *elapsed time*. Should pg_sage's destination be "SAFE+reversible actions auto-apply from day 0, verified" rather than "everything waits 31 days"? The trust ramp may be conflating *operator confidence over time* with *intrinsic action risk* — which are orthogonal.

---

## Sources (new, not in v1 or competitive_landscape.md)

### Oracle Automatic Indexing & SPM
- [oracle-base — Automatic Indexing in Oracle Database 19c](https://oracle-base.com/articles/19c/automatic-indexing-19c)
- [Richard Foote — Oracle 19c Automatic Indexing: Methodology](https://richardfoote.wordpress.com/2019/07/24/oracle-19c-automatic-indexing-methodology-introduction-after-today/)
- [Oracle blog — Automatic Indexing Cheat Sheet](https://blogs.oracle.com/datawarehousing/post/automatic-indexing-cheat-sheet)
- [oracle-base — SQL Plan Management 11gR1](https://oracle-base.com/articles/11g/sql-plan-management-11gr1)
- [Oracle docs 19c — Managing SQL Plan Baselines](https://docs.oracle.com/en/database/oracle/oracle-database/19/tgsql/managing-sql-plan-baselines.html)

### Azure SQL & SQL Server Automatic Tuning / APC
- [Microsoft Learn — Automatic Tuning Overview (Azure SQL)](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql)
- [Microsoft Learn — Automatic Tuning (SQL Server)](https://learn.microsoft.com/en-us/sql/relational-databases/automatic-tuning/automatic-tuning?view=sql-server-ver17)
- [dbanuggets — How Automatic Plan Correction works](https://dbanuggets.com/2022/01/05/query-store-fundamentals-how-automatic-plan-correction-works/)
- [SQLYARD — APC: How SQL Server detects and fixes query regressions](https://sqlyard.com/2026/05/18/automatic-plan-correction-how-sql-server-detects-and-fixes-query-regressions/)
- [Microsoft Research — Automatically Indexing Millions of Databases in Azure SQL DB (PDF)](https://www.microsoft.com/research/uploads/prod/2019/02/autoindexing_azuredb.pdf)

### AWS Aurora / RDS
- [AWS docs — DevOps Guru for RDS (Aurora)](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/devops-guru-for-rds.html)
- [AWS blog — Proactive Insights with DevOps Guru for RDS](https://aws.amazon.com/blogs/devops/proactive-insights-with-amazon-devops-guru-for-rds/)
- [AWS blog — Aurora Optimized Reads for Aurora PostgreSQL](https://aws.amazon.com/blogs/database/new-amazon-aurora-optimized-reads-for-aurora-postgresql-with-up-to-8x-query-latency-improvement-for-i-o-intensive-applications/)
- [InfoQ — RDS Optimized Reads/Writes (Torn Write Prevention)](https://www.infoq.com/news/2023/01/aws-rds-optimized-reads-writes/)

### Google AlloyDB / Cloud SQL
- [AlloyDB docs — Use the index advisor](https://cloud.google.com/alloydb/docs/use-index-advisor)
- [Google Cloud blog — How the AlloyDB Index Advisor works](https://cloud.google.com/blog/products/databases/how-the-alloydb-index-advisor-helps-make-smart-indexes)
- [AlloyDB docs — Auto-columnarization / column store content](https://cloud.google.com/alloydb/docs/columnar-engine/manage-content-recommendations)

### CockroachDB / TiDB
- [Cockroach docs — Admission Control](https://www.cockroachlabs.com/docs/stable/admission-control)
- [Cockroach blog — How admission control protects against overload](https://www.cockroachlabs.com/blog/admission-control-unexpected-overload/)
- [TiDB docs — Introduction to Statistics (auto-analyze)](https://docs.pingcap.com/tidb/stable/statistics/)

### MongoDB Atlas Autopilot
- [MongoDB — Database Automation: Automated Indexes (Autopilot)](https://www.mongodb.com/developer/products/atlas/mongodb-automation-index-autopilot/)
- [MongoDB blog — Auto-Index Creation for Atlas Serverless](https://www.mongodb.com/blog/post/introducing-auto-index-creation-atlas-serverless-instances)
- [MongoDB docs — Review Drop Index Recommendations](https://www.mongodb.com/docs/atlas/performance-advisor/drop-indexes/)

### Redshift
- [AWS — Redshift Advisor sort/distribution key recommendations](https://aws.amazon.com/about-aws/whats-new/2023/12/amazon-redshift-advisor-sort-distribution-key-recommendations/)
- [AWS blog — Automate Redshift tuning with Automatic Table Optimization](https://aws.amazon.com/blogs/big-data/automate-your-amazon-redshift-performance-tuning-with-automatic-table-optimization/)
