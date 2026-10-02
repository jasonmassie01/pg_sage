# Landscape: AI-driven database operations, late 2026

Scope: AI-driven ops for PostgreSQL as of 2026-10-02, plus non-Postgres bar-setters. Updates the
June research (`research/v2_competitive_postgres_autonomy.md`). Vendor claims are paraphrased,
not independently verified. pg_sage claims were checked against package names, the MCP tool
registry (`sidecar/internal/mcp/*.go`) and `CHANGELOG.md` v1.8.1, not a deep code read.

## What changed since June

1. **Autonomy went mainstream in marketing but not in practice.** EDB launched an "agentic database"
   (2026-06-23) that auto-applies indexes, scaling and minor patches behind an approval dial. But
   that runs only on its Hybrid Manager. Every hyperscaler AI agent is still diagnose-only: AWS
   DevOps Agent (GA 2026-03-31), Google's Database Observability Agent (Next '26, 2026-08-04) and
   Datadog Bits.
2. **A real benchmark now exists, and it is brutal.** DBA-Bench (arXiv 2607.22165, July 2026) has
   106 live-workload PostgreSQL scenarios. The best model reaches **17.9% "Safe Pass"** against
   **93.4%** for a human DBA. 36.7% of runs that fixed the problem still broke a safety rule, mostly
   through unscoped interventions.
3. **MCP is table stakes and mostly read-only.** pganalyze, Supabase, Neon, PlanetScale, AWS
   (Aurora), Google (MCP Toolbox 1.0 GA plus managed MCP servers), pgEdge, Postgres MCP Pro and
   Tiger pg-aiguide all ship one. Almost all are read-only or execute raw SQL. Very few offer a
   *governed write path*.
4. **Read-only chat agents are commoditized.** Xata Agent, the best-known OSS Postgres AI SRE, was
   **archived on 2026-06-15**. pgEdge shipped a similar read-only design (AI DBA Workbench, "Ellie")
   in April.
5. **The agent-created-database wave is real.** pganalyze cites over 80% of Neon databases as
   agent-created. Tiger Ghost (GA 2026-06-09) and AlloyDB "PostgreSQL for agents" (preview
   2026-09-24) both target agent-spawned databases. They provision and scale those databases;
   none of them *operates* them.

## 1. Landscape table

| Product / project | What it automates | Autonomy (L0 observe … L3 auto) | How it earns trust | Pricing / positioning | Source |
|---|---|---|---|---|---|
| **pganalyze** (Index Advisor 3, Query Advisor, VACUUM Advisor, MCP) | Workload-aware index selection, plan-pattern insights (inefficient index use, work_mem, OR→UNION), checkups. MCP is read-only: query stats, EXPLAINs, index selection | **L1 advise-only, by policy.** The "Dilemma of the AI DBA" post argues AI should not own outcomes | Deterministic analysis, not generative. PII redaction. MCP needs no DB access and is capped at 100 calls/h per server | Paid SaaS and Enterprise Server. Positioned as "AI helps engineers", not "AI DBA" | [blog](https://pganalyze.com/blog), [MCP](https://pganalyze.com/blog/mcp-server-public-preview), [dilemma](https://pganalyze.com/blog/the-ai-dba-dilemma), [IA3](https://pganalyze.com/blog/index-advisor-v3) |
| **PostgresAI** (postgres_ai, DBLab, "Self-Driving Postgres") | Health checks, AI-prepared PRs, recommendations validated on DBLab thin clones | **L1 today.** Roadmap: safe ops automatic in 2026 ("Autopilot"), full self-driving 2027+ | Clone-tested changes, human expert validation, PR review | $512/month per cluster (AI layer plus expert DBAs). Monitoring is Apache-2.0 | [roadmap](https://v2.postgres.ai/docs/roadmap), [SDP](https://postgres.ai/blog/20250725-self-driving-postgres) |
| **DBtune** | Closed-loop knob tuning (work_mem, max_wal_size, random_page_cost …) measured on TPS and latency. Patroni- and CloudNativePG-aware (2026) | **L3 for knobs** (auto mode) or L1 (recommend mode) | Reload-only knobs, regression and memory guards, one-click rollback | Free for ≤3 instances, then per-core/year. On AWS and Azure marketplaces | [products](https://www.dbtune.com/products), [Patroni](https://dbtune.com/blog/autonomous-tuning-patroni-ha), [CNPG](https://www.dbtune.com/blog/tuning-cloudnativepg-without-the-guesswork) |
| **OtterTune** (legacy) | ML knob tuning for RDS/Aurora | L3 knobs | Ran as a SaaS that needed cloud access | **Dead (2024).** The lesson: knob tuning alone was not sticky | [ottertune.com](https://ottertune.com/), [DBtune on it](https://www.dbtune.com/blog/ottertune) |
| **EDB Postgres AI "agentic database"** | Auto-applies the highest-value pending index, CPU/memory/storage autoscale, minor-version patching. Advisory exposed over MCP | **L3 with dial:** no-approval / approval-required / maintenance-window | Task Manager audit trail, RBAC approvers, written justification. Rollback not documented. Agent governance (owners, drift) promised for H2 | Enterprise. Hybrid Manager only. Sovereign/on-prem pitch | [Q2 release](https://www.enterprisedb.com/blog/edb-postgres-ai-q2-2026-release), [internals](https://www.enterprisedb.com/blog/inside-agentic-database-how-edb-turned-postgres-self-managing-system), [launch](https://www.opensourceforu.com/2026/06/edb-postgres-ai-launch/) |
| **Azure DB for PostgreSQL "autonomous tuning"** | Create/drop/reindex indexes, ANALYZE, VACUUM recommendations from Query Store plus HypoPG | **L1.** `index_tuning.mode` is only OFF or REPORT; it has no auto-apply despite the name | Cost-based improvement and regression factors, size caps, 35-day unused window | Free with the service (≥4 vCores) | [docs](https://learn.microsoft.com/en-us/azure/postgresql/monitor/concepts-autonomous-tuning) |
| **Azure SQL / SQL Server automatic tuning** (bar-setter) | CREATE INDEX, DROP INDEX, FORCE_LAST_GOOD_PLAN | **L3**, per option | Verifies every change on Query Store per-query metrics and reverts on regression | Included | [docs](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql) |
| **Oracle Autonomous AI DB 26ai** (bar-setter) | Automatic indexing and partitioning. Select AI Agent with MCP | **L3** | Candidate indexes are built *invisible*, tested against the workload, and made visible only if they help. Regressed SQL gets SPM baselines | Proprietary, OCI | [ORACLE-BASE](https://oracle-base.com/articles/19c/automatic-indexing-19c), [26ai](https://blogs.oracle.com/database/oracle-announces-oracle-ai-database-26ai) |
| **Google Cloud: Database Observability Agent, Gemini, MCP Toolbox** | RCA across Database Insights, logs and traces. Recommends pooling and indexes. AlloyDB auto-maintains ScaNN vector indexes | **L1–L2.** Executes "validated actions with user approval" | Rationale plus expected impact. Approval gate | Preview. Toolbox 1.0 GA (OSS, 40+ DBs). Managed MCP GA | [agents](https://cloud.google.com/blog/products/databases/deep-dive-on-new-ai-powered-database-agents), [Next '26](https://cloud.google.com/blog/products/databases/whats-new-for-google-cloud-databases-at-next26) |
| **AWS DevOps Agent, CloudWatch Database Insights** | Autonomous incident investigation, including RDS/Aurora PG logs and Performance Insights data | **L0–L1.** No write access by design | Investigation trace, mitigation plan | $0.0083 per agent-second. Performance Insights console EOL 2026-07-31 | [GA](https://aws.amazon.com/blogs/mt/announcing-general-availability-of-aws-devops-agent/), [diagnose-not-act](https://bex.co/blog/2026/08/06/aws-devops-agent-diagnose-not-act-line), [agentic DB](https://aws.amazon.com/products/databases/agentic-ai/) |
| **Datadog Bits AI SRE, DBM** | Alert investigation (3–4 min) over metrics, APM, logs, DBM plans. DBM missing-index recommendations | **L1.** Triage actions (tickets, Slack), no DB writes | Agent Trace view of every tool call | Add-on to Datadog. DBM recommendations included | [Bits](https://www.datadoghq.com/blog/bits-ai-sre-deeper-reasoning/), [comparison](https://bigdataboutique.com/blog/postgresql-monitoring-tools) |
| **PagerDuty SRE Agent** | Triage, diagnostics, remediations, self-updating runbooks. Learns from incident memory | **L1–L2.** "Act or wait for approval". Fully autonomous responder in EA for H2 2026 | Permissions and behavior guidance, memory | Platform add-on | [Spring '26](https://www.pagerduty.com/blog/product/the-path-to-autonomous-operations-pagerduty-spring-26-release/) |
| **Resolve AI / Traversal / Cleric** (generic AI SRE) | Multi-hop causal RCA across the stack | Mostly L1. Resolve "pushing to" high autonomy behind guardrails | Evidence-backed RCA. Traversal claims 82% RCA accuracy | Resolve sells consumption credits ($1B valuation). Enterprise | [Traversal data](https://www.traversal.com/blog/ai-in-incident-response-state-of-the-field-2026-sre), [Resolve](https://resolve.ai/product/ai-sre), [overview](https://wetheflywheel.com/en/guides/best-ai-sre-tools-2026/) |
| **PlanetScale Insights** (PG) | LLM-proposed indexes, validated by syntax check plus HypoPG. Schema recommendations (redundant/unused, PK exhaustion, bloat) | **L1.** Recommendations can be picked up by coding agents via MCP "on a schedule" | Pre-filters to high-impact queries. Only shows planner-validated wins | Included in PlanetScale Postgres | [blog](https://planetscale.com/blog/postgres-new-index-suggestions), [docs](https://planetscale.com/docs/postgres/monitoring/schema-recommendations) |
| **Neon / Lakebase MCP** | `prepare_query_tuning` tests suggested DDL on a temporary branch; `complete_query_tuning` applies it or discards it | **L2**, agent-driven with a branch sandbox | Branch-based what-if on real data | Platform feature | [Neon MCP](https://github.com/neondatabase/mcp-server-neon), [docs](https://neon.com/docs/ai/neon-mcp-server) |
| **Supabase** | Security and performance advisors via a hosted MCP. 30-rule "Postgres best practices" agent skill (Jan 2026) | L1. The MCP can also run DDL on behalf of coding agents | Rules and lints | Platform feature | [skills](https://supabase.com/blog/postgres-best-practices-for-ai-agents) |
| **Aiven AI DB Optimizer** (EverSQL) | Index and SQL rewrite suggestions from the slow log | L1 | Large prior user base | Aiven add-on | [press](https://aiven.io/press/aiven-releases-ai-database-optimizer) |
| **Releem** | ML config tuning, query optimization, schema checks. PG added Apr–May 2026 | L1–L3 (auto config apply on MySQL) | Per-version ML models | SaaS, MySQL-first | [PG](https://releem.com/postgresql) |
| **pgEdge AI DBA Workbench** ("Ellie") | Collector (34 probes), alerter with z-score → pgvector-similarity → LLM anomaly tiers, MCP, chat | **L0–L1.** Read-only; gives SQL to the human | Tool calls executed server-side, `test_query`, per-cluster memory | OSS (PostgreSQL licence) | [blog](https://www.pgedge.com/blog/inside-the-pgedge-ai-dba-workbench-how-ellie-actually-works), [GitHub](https://github.com/pgEdge/ai-dba-workbench) |
| **Xata Agent** | Playbook-driven monitoring, tuning and RCA for RDS/Aurora | L1. Approval workflow was still on the roadmap | Playbooks to curb hallucination | **Archived 2026-06-15** | [GitHub](https://github.com/xataio/agent) |
| **Postgres MCP Pro** (CrystalDBA) | Health checks, HypoPG index search, EXPLAIN, configurable read/write SQL | L1–L2 (depends on access mode) | Classical index-selection algorithm plus experimental LLM mode | OSS (MIT) | [GitHub](https://github.com/crystaldba/postgres-mcp) |
| **Datapace** | Governance layer: agents propose DB changes, a policy check gates them, approval, immutable audit | L2 gated | Policy plus approval plus audit | Multi-engine SaaS | [compare](https://datapace.ai/compare/xata-agent) |
| **Tiger pg-aiguide, Microsoft postgres-skills** | Version-aware PG docs and skills for coding agents | n/a | Curated docs | Free / OSS | [pg-aiguide](https://github.com/timescale/pg-aiguide), [skills](https://github.com/microsoft/postgres-skills) |
| **Research:** DBA-Bench, agentic doc-to-action tuning | Live-workload PG ops benchmark scored on diagnosis, outcome and *safe* outcome. Doc-grounded knob tuning | n/a | Separates diagnosis, outcome and safety | arXiv 2026 | [DBA-Bench](https://arxiv.org/html/2607.22165v1), [tuning](https://arxiv.org/pdf/2605.19988) |

**The pattern:** agents *diagnose but do not act*. The few that act on Postgres each cover one
narrow surface: DBtune (knobs), EDB (indexes, scaling, patches; own platform only), Neon
(branch-sandboxed DDL driven by an outside agent). Only Oracle and Azure SQL combine apply,
verify and auto-revert, and both are proprietary.

## 2. Where pg_sage leads

1. **It acts across the broadest surface on any Postgres, with trust that is earned per action
   class.** Index lifecycle, per-table autovacuum, config, stats, incident playbooks and retention
   all go through one locked executor path (`policy.Gate` / `Executor.Apply`, v1.8.1). Autonomy is
   tracked per class (`internal/earned`), irreversible actions are capped at L1, and an admin
   approves every promotion. No portable-Postgres competitor has per-class earned autonomy. EDB has
   a three-position dial; DBtune has on/off.
2. **Verify-and-revert closer to the Oracle and Azure SQL model.** There is per-query verification
   (`executor/perquery_verify_test.go`, `rollback_eval.go`), a mini Query Store
   (`internal/querystore`, `internal/planhash`) and load admission. Of the Postgres products
   surveyed, only DBtune (for knobs) documents an equivalent automatic revert. EDB's docs describe
   no rollback.
3. **A governed, intent-level write-path MCP.** `internal/mcp/server.go` exposes
   `request_change`, `optimize_query`, `apply_migration`, `ensure_fk_indexes`,
   `declare_table_contract`, `register_consumer`, `propose_policy_change`, `get_ledger`,
   `get_value` and SRE propose/execute/downgrade tools; raw `execute_sql` is rejected
   (`server_test.go`). pganalyze's MCP is read-only, Neon's is unpoliced branch DDL, Datapace is
   governance without DBA logic, and EDB's agent governance is promised for H2.
4. **Evidence-first Postgres-native RCA.** Sage SRE combines bounded catalog probes, a deterministic
   causal graph (`causal-v4`), validated model claims and PgBouncer and failover awareness. It is
   measured by PGIncidentBench. Generic AI SREs (Datadog, AWS, Resolve) are broad but shallow on
   Postgres internals. DBA-Bench shows models fail exactly on multi-layer chains, for example stale
   stats → bad plan → lock pileup → pool exhaustion.
5. **Proof-of-value accounting.** Outcome ledger, verified DBA-hours saved (`internal/value`),
   shadow mode, game days on clones (`internal/gameday`, `clone`) and SLO error budgets. Nobody
   surveyed ties value to individually verified actions.
6. **No extension, any host, any model.** Runs on RDS, Aurora, Cloud SQL, AlloyDB, Neon,
   Supabase, Lakebase and self-managed Postgres; works with cheap models or none.

## 3. Where pg_sage lags

1. **Nobody outside the repo can see the proof.** PGIncidentBench is internal. DBA-Bench is the
   external yardstick the field will cite, and pg_sage has no number on it. Competitors publish
   MTTR and accuracy claims (AWS 94% RCA accuracy, Traversal 82%) even when they are unaudited.
2. **The agent's own footprint and production hygiene.** The dogfood run exposed several problems:
   - its forecaster overloaded the database;
   - its own snapshots took 9.3 GB;
   - it fought the app's migrations;
   - 143 stale incidents stayed open.

   pganalyze, Datadog DBM and pgEdge have years of low-overhead collector engineering. Most of this
   was fixed in v1.8.1, but there is no *declared, enforced overhead budget* yet (inferred).
3. **Telemetry breadth.** pg_sage reads the catalog plus `pg_stat_statements` (and logs where
   available). Datadog correlates DB plans with APM, RUM and deploys. Google and AWS agents read
   managed logs, traces and host metrics natively. pg_sage needs host CPU and IO for automatic index
   admission (README), so on catalog-only adapters it withholds index autonomy that EDB and Azure
   SQL exercise freely.
4. **Pre-apply verification on real data is not the default path (inferred).** Clone providers
   exist, but this read only saw them wired to game days and config. Oracle builds invisible
   indexes, Neon tests on a temporary branch and PostgresAI tests on a DBLab clone. pg_sage relies
   on HypoPG plus post-apply verification.
5. **No closed-loop, measurement-driven knob optimizer.** DBtune iterates on measured TPS and
   latency, is HA-cluster-aware and needs no restart. pg_sage's config advisors are LLM and rule
   proposals that are verified afterwards. That is fine for one-shot changes, but they do not
   *search* the knob space.
6. **Infra-layer actions are missing.** pg_sage does no compute, storage or IOPS rightsizing and no
   minor-version patching, both of which EDB automates. Roadmap item B3 exists; the code was not
   checked.
7. **Approval and promotion UX.** The dogfood user could not tell how to promote anything. EDB's
   Task Manager and PagerDuty's "act or wait" toggle are simpler mental models.
8. **Distribution.** No marketplace listing, hosted tier or public adoption signal. DBtune is on
   the AWS and Azure marketplaces; pganalyze and Datadog sell into existing install bases.

## 4. White space: capabilities nobody does well yet

1. **Safe Pass as the product metric.** DBA-Bench shows a 75-point gap between the best model and a
   human DBA on *safe* remediation, and that 36.7% of "successful" agent fixes broke a safety rule.
   pg_sage's typed actions, scoped locks and gates exist to prevent exactly those unscoped
   interventions. No vendor reports a safe-remediation rate.
2. **A governed change-control plane for coding agents.** Coding agents already drive Supabase,
   Neon and PlanetScale through MCP, yet every Postgres MCP is read-only or raw SQL. Nobody combines
   intent-level requests, policy gating, rehearsal, verified apply with rollback and an agent-readable
   ledger entry. pg_sage has most of the parts.
3. **Change-ownership awareness.** No tool knows *who owns an object* (app migration, ORM, tenant
   provisioning, a human), so none knows when its change will be undone or when the fix belongs in
   a PR to the app repo. pg_sage's "leave app-recreated indexes alone" is a deterministic patch; a
   model reading the migration repo and DDL history could classify ownership and route the fix.
4. **Postgres-native "invisible index" verification.** PostgreSQL has no invisible indexes.
   Approximate Oracle's build → test → make-visible with HypoPG as a filter, branch/clone replay of
   the top affected queryids, post-create per-query verification, and a "soft drop" period before
   any real `DROP`. No portable-Postgres product closes this loop.
5. **Operating agent-spawned fleets cheaply.** Ghost, Lakebase and AlloyDB-for-agents provision and
   scale databases but do not tune, clean up or govern them. They need fleet-level policy
   templates, per-tenant LLM budgets, abandoned-object detection and TTL reconciliation at
   thousands of databases. pg_sage's fleet hot-reload, FleetBudget and AgentDB reconciler are the
   seeds of this. Nobody else is building toward it.
6. **Being the Postgres specialist that generic AI SREs call.** AWS DevOps Agent, Bits, PagerDuty
   and Resolve speak MCP and are weak on Postgres internals. They need a sub-agent that returns a
   cited causal chain and a gated remediation they can request. pg_sage's `sre_*` MCP tools are
   close to that. Being the specialist that big platforms call is a distribution channel, not a
   competitor fight.
7. **An enforced observer overhead budget.** No vendor publishes and enforces a cap on the CPU,
   IO, storage and lock time its agent may consume, with self-throttling. After the dogfood
   incidents, this would turn a weakness into a trust feature.
8. **Hygiene at the object-purpose level.** Tools report 160 leaked `test_*` schemas as thousands
   of duplicate-index findings instead of "abandoned test fixtures". An LLM can classify object
   *purpose* from names, ownership, activity and DDL history and group findings by root object.
9. **Verified-outcome pricing.** Resolve sells credits per investigation; nobody prices or reports
   per *verified* DBA outcome. pg_sage's ledger plus value accounting could support it.
10. **Memory that binds policy.** PagerDuty and pgEdge store free-text agent memories with no
    lifecycle. Facts like "app recreates idx_x" or "batch window is 02:00–03:00" should be typed,
    evidence-linked, model-proposed, approved once, then binding on policy. Nobody does this.

## 5. Top 5 recommendations

**1. Publish a DBA-Bench Safe Pass number; make safe remediation the headline metric.** The best
LLM agent safely fixes 17.9% of live Postgres scenarios against 93.4% for a human. Safe remediation
is the gap pg_sage's typed actions, scoped locks, gates and verification exist to close. Run
pg_sage (model on and off) against the open scenarios, map playbooks to the seven domains, publish
Diagnosis/Outcome/Safe Pass with failures, and fold the scenarios into PGIncidentBench. A Safe Pass
well above frontier baselines is a citable differentiator no competitor has. Guardrail: sandbox
and shadow mode first; report failures honestly.

**2. Ship the governed MCP write path as a product for coding agents.** Agents create and change
most new Postgres databases, and every MCP today is read-only or raw SQL. Package
`request_change` / `optimize_query` / `apply_migration` with clone or HypoPG rehearsal, a policy
decision, verified apply and ledger readback, plus a Claude Code/Cursor plugin and a "pg_sage mode"
for Neon, Supabase and PlanetScale flows. Positioning: pganalyze explains, Neon sandboxes, pg_sage
decides and safely applies. Guardrail: existing `policy.Gate`; no raw SQL; every intent gets a
lease, verification plan and audit row.

**3. Build Postgres "invisible index" verification and enforce an observer budget.** Rehearse on
the best available substrate (branch/clone, then HypoPG, then canary) across the top affected
queryids, add a soft-drop period before any irreversible `DROP`, and give pg_sage a declared,
self-enforced CPU/IO/storage/probe budget that throttles and records when it backs off. Together
these answer the dogfood's two objections ("did it help?" and "is the agent the problem?") with
mechanisms Oracle and Azure SQL users recognize. Effort M–L; low risk (read-only or additive).

**4. Make change ownership and memory first-class: the model classifies, policy binds.** The model
proposes typed, evidence-linked facts (owner: app migration, tenant provisioning or human; purpose:
test fixture, CDC slot, batch window; recreation history). An operator approves each once and it
becomes binding. Owned-by-app findings route to a PR or ticket instead of DDL; abandoned-fixture
clusters collapse into one case. This fixes the 8x index fight and the 160-schema noise at the
root. Guardrail: memories can only narrow or redirect action, never raise autonomy.

**5. Become the Postgres specialist that AI SRE platforms call.** Harden the `sre_*` MCP tools into
a stable investigation contract: cited causal chain, confidence, missing evidence, and a gated
remediation the caller may request but not force. Integrate with AWS DevOps Agent, PagerDuty and
Datadog Bits. They own the pager and budget but are diagnose-only and shallow on Postgres, so this
is distribution without a sales team, and it fits the agent-fleet future. Cost control: existing
FleetBudget and per-investigation token caps.

## Sources

The table links each source. Also used:
[AlloyDB for agents](https://cloud.google.com/blog/products/databases/announcing-postgresql-for-agents-in-alloydb),
[Tiger Ghost](https://siliconangle.com/2026/06/09/tiger-data-launches-postgresql-extension-designed-ai-agents/).
