# LLM/AI Automation Opportunities for pg_sage

> Research date: 2026-04-12
> Scope: Capabilities beyond what pg_sage v0.8.3 already handles (query tuning, index optimization, vacuum management, hint plan management, stale stats detection, connection pool analysis, WAL/checkpoint tuning, memory tuning, bloat remediation)

---

## Table of Contents

1. [Query Rewriting and SQL Transformation](#1-query-rewriting-and-sql-transformation)
2. [Schema Evolution Assistance](#2-schema-evolution-assistance)
3. [Capacity Planning and Forecasting](#3-capacity-planning-and-forecasting)
4. [Incident Response Automation](#4-incident-response-automation)
5. [Security and Compliance](#5-security-and-compliance)
6. [Cost Optimization](#6-cost-optimization)
7. [Workload Analysis](#7-workload-analysis)
8. [Developer Experience](#8-developer-experience)
9. [Competitive Landscape](#9-competitive-landscape)
10. [Academic Research](#10-academic-research)
11. [Prioritized Roadmap Recommendation](#11-prioritized-roadmap-recommendation)

---

## 1. Query Rewriting and SQL Transformation

### 1.1 Automatic Materialized View Creation and Refresh Scheduling

**What**: Analyze pg_stat_statements to detect repeated expensive aggregation/join queries, then recommend (or create) materialized views with optimal refresh schedules based on data staleness tolerance and write frequency.

**Evidence**: SQLFlash (2025) demonstrated 90% reduction in manual optimization effort through automated SQL rewriting including materialized view suggestions. Alibaba Cloud published detailed technical documentation on query rewriting based on materialized views, showing significant performance gains for analytical workloads. PostgreSQL natively supports materialized views since 9.3 but has no auto-refresh mechanism — pg_sage could fill this gap.

**Rating**:
- **Impact**: HIGH — Materialized views can reduce query latency by 10-100x for analytical patterns found in OLTP databases
- **Feasibility**: HIGH — pg_stat_statements provides query frequency/cost data; pg_sage already monitors queries; refresh scheduling is a natural extension of vacuum scheduling
- **LLM dependency**: Enhances — Pattern detection is deterministic (frequency + cost thresholds), but LLM excels at understanding query semantics to determine which queries can share a materialized view and explaining trade-offs to users
- **Competitive moat**: STRONG — No existing PostgreSQL tool automatically creates and schedules materialized views based on workload analysis. Cloud SQL Recommender and Aiven AI Optimizer focus on indexes only.

### 1.2 CTE Optimization

**What**: Detect CTEs that defeat PostgreSQL's optimizer (pre-12 always-materialized behavior, or post-12 cases where MATERIALIZED hint is wrongly applied) and recommend inlining or restructuring.

**Evidence**: PostgreSQL 12 introduced automatic CTE inlining for non-recursive, side-effect-free, singly-referenced CTEs. However, many production queries still use patterns that prevent inlining (multi-referenced CTEs, CTEs with side effects). boringSQL's "Good CTE, bad CTE" analysis (2025) documents specific anti-patterns. SQLFlash v2.0 added CTE refactoring as a core optimization rule.

**Rating**:
- **Impact**: MEDIUM — Affects a subset of queries, but those affected can see 2-10x improvement
- **Feasibility**: HIGH — libpg_query can parse CTEs; EXPLAIN output reveals materialization behavior
- **LLM dependency**: Enhances — The rewrite logic is deterministic, but LLM can explain why a CTE restructuring improves performance in human terms
- **Competitive moat**: MODERATE — SQLFlash does this but only as a SaaS product, not as a sidecar agent

### 1.3 Subquery Flattening Recommendations

**What**: Detect correlated subqueries that the planner fails to flatten and recommend JOIN-based rewrites.

**Evidence**: AI2sql's 2025 optimization guide documents subquery unnesting (converting subqueries to joins) as one of the highest-impact automated rewrites. PostgreSQL's planner can flatten some subqueries automatically, but correlated subqueries with certain patterns (EXISTS with complex predicates, subqueries in SELECT list) remain problematic.

**Rating**:
- **Impact**: HIGH — Correlated subqueries are one of the most common performance killers; flattening can yield 10-1000x improvement
- **Feasibility**: MEDIUM — Requires AST analysis of query plans to detect subquery patterns the planner didn't flatten; semantic equivalence verification is non-trivial
- **LLM dependency**: Required — LLM needed for generating semantically equivalent JOIN rewrites for complex cases; deterministic rules cover simple cases
- **Competitive moat**: STRONG — No production PostgreSQL tool does this automatically at the sidecar level

### 1.4 Partition Pruning Advice

**What**: Detect queries scanning partitioned tables where partition pruning is not occurring (visible in EXPLAIN as scanning all partitions) and recommend query modifications or partitioning strategy changes.

**Evidence**: AI-driven optimization tools in 2025 increasingly suggest partitioning based on data patterns and query access paths. PostgreSQL 11+ supports dynamic partition pruning, but many queries defeat it through function calls, type mismatches, or parameterized queries where the planner can't determine partition at plan time.

**Rating**:
- **Impact**: HIGH — For large partitioned tables, proper pruning reduces I/O by orders of magnitude
- **Feasibility**: HIGH — EXPLAIN ANALYZE shows partition scan counts; pg_stat_statements reveals per-partition access patterns
- **LLM dependency**: Enhances — Pattern detection is deterministic; LLM helps explain recommendations
- **Competitive moat**: MODERATE — pganalyze shows partition scan stats but doesn't recommend fixes

### 1.5 N+1 Query Detection and Batching Recommendations

**What**: Detect N+1 patterns at the wire protocol level or through pg_stat_statements temporal analysis — sequences of identical parameterized queries executed in rapid succession that could be consolidated into a single batched query.

**Evidence**: PgDog (2025-2026) demonstrates wire protocol analysis for PostgreSQL, parsing queries and storing ASTs for optimization. A documented case study showed reducing 10+ queries to 1 via joining, dropping response time from 2.7s to 0.3s. No existing production tool specifically detects and recommends fixes for N+1 patterns in PostgreSQL.

**Rating**:
- **Impact**: HIGH — N+1 is the single most common application-level performance anti-pattern; fixes routinely yield 5-50x improvement
- **Feasibility**: MEDIUM — Temporal correlation of queries in pg_stat_statements is possible but requires careful analysis of call patterns, timestamps, and query fingerprints. Wire protocol interception (like PgDog) is more accurate but architecturally invasive.
- **LLM dependency**: Required — LLM needed to understand query relationships and generate consolidated alternatives
- **Competitive moat**: VERY STRONG — Nobody does this well for PostgreSQL. This would be a genuine differentiator.

---

## 2. Schema Evolution Assistance

### 2.1 Schema Anti-Pattern Detection

**What**: Detect EAV (Entity-Attribute-Value) tables, polymorphic associations, missing foreign keys, missing primary keys, overly wide tables, and other structural anti-patterns.

**Evidence**: boringSQL's dryrun (2026) implements 33 rules (19 convention + 14 audit) for PostgreSQL schema analysis including naming violations, missing timestamps, serial vs identity columns, duplicate indexes, and circular foreign keys — all working offline from JSON snapshots. Supabase Agent Skills (Jan 2026) organizes 8 prioritized categories of PostgreSQL optimization guidelines with machine-parseable impact-weighted rules.

The Art of PostgreSQL documents common anti-patterns including EAV, polymorphic associations, and JSONB misuse. GitLab's engineering documentation explicitly recommends against polymorphic associations. However, no tool automatically detects these at the database level and recommends remediation.

**Rating**:
- **Impact**: HIGH — Schema anti-patterns cause compounding performance and maintainability problems; early detection saves months of migration work later
- **Feasibility**: HIGH — Catalog queries against pg_class, pg_attribute, pg_constraint can identify EAV patterns (tables with entity/attribute/value columns), polymorphic associations (type+id column pairs without FK constraints), missing FKs (columns named *_id without constraints)
- **LLM dependency**: Enhances — Pattern detection is largely deterministic; LLM excels at explaining why a pattern is problematic and suggesting remediation paths
- **Competitive moat**: STRONG — dryrun does linting but requires manual snapshot export; pg_sage could do this continuously and autonomously

### 2.2 Safe Migration Planning (Zero-Downtime ALTER TABLE)

**What**: Analyze proposed DDL statements and provide: lock type required, estimated lock duration based on table size, safe alternatives, rollback DDL, and step-by-step zero-downtime migration plans.

**Evidence**: This is an active area with multiple tools:
- **Squawk** (2025): Static linter for PostgreSQL migrations detecting unsafe DDL operations (adding NOT NULL columns, index creation without CONCURRENTLY, blocking constraint creation). Open source, CI-focused.
- **dryrun** (2026): Every DDL statement gets lock type, rewrite risk, safe alternative, and rollback DDL with version-aware knowledge.
- **pgroll** (2025-2026): Zero-downtime, reversible migrations via schema versioning with expand-and-contract pattern. Single Go binary, Postgres 14+.

The risk formula documented across multiple sources: Lock Severity x Duration x Traffic. On a 50M row table, regular CREATE INDEX blocks writes for minutes. The lock queue effect is the most dangerous: a waiting DDL blocks ALL subsequent queries on that table.

Key insight: lock_timeout before any DDL is mandatory, not optional.

**Rating**:
- **Impact**: VERY HIGH — Production outages from unsafe migrations are extremely common; a $4.5M migration mistake was documented by BrightCoding (2025)
- **Feasibility**: HIGH — pg_sage already knows table sizes, row counts, and write rates; DDL analysis via libpg_query is well-understood; lock type mapping is documented
- **LLM dependency**: Enhances — Lock type analysis is deterministic; LLM generates human-readable migration plans and explains trade-offs
- **Competitive moat**: MODERATE — Squawk and dryrun exist but are static/offline tools; pg_sage would provide real-time analysis with actual table statistics (row counts, write rates, active connections) making estimates far more accurate

### 2.3 Column Type Optimization

**What**: Detect suboptimal column types: text vs varchar (no performance difference in PostgreSQL), int vs bigint (wasted space), timestamp vs timestamptz, JSONB columns with consistent structure that should be relational columns, oversized varchar limits.

**Evidence**: PostgreSQL best practices guides (2025) consistently recommend: use text over varchar (no performance benefit to varchar limits in PostgreSQL), timestamptz over timestamp, and generated columns to pull frequently-queried JSONB fields into indexed columns. A hybrid approach — traditional columns for fixed attributes, JSONB for variable parts — is documented as the optimal pattern.

PostgreSQL 12+ generated columns enable automatic denormalization of JSONB fields, running a function whenever a row is inserted/updated.

**Rating**:
- **Impact**: MEDIUM — Storage savings of 10-30% from type optimization; index efficiency improvements from extracting JSONB fields
- **Feasibility**: HIGH — Catalog inspection reveals column types; JSONB structure analysis requires sampling data; pg_sage can correlate with query patterns
- **LLM dependency**: Enhances — Type mismatch detection is deterministic; LLM helps with JSONB structure analysis and migration recommendations
- **Competitive moat**: MODERATE — JSONB structure analysis is unique; basic type recommendations are common knowledge

### 2.4 Denormalization Recommendations

**What**: Based on actual query patterns from pg_stat_statements, recommend specific denormalizations (adding redundant columns, creating summary tables) that would eliminate expensive JOINs in hot queries.

**Evidence**: Modern guidance (2025-2026) recommends data-driven denormalization: aggregations are faster in denormalized versions because data is already stored that way, but require several joins in relational models. The key is using actual query patterns to drive decisions rather than guessing.

**Rating**:
- **Impact**: HIGH — Targeted denormalization of the top 5 most expensive queries can reduce total database CPU by 20-40%
- **Feasibility**: MEDIUM — Requires correlating pg_stat_statements query plans with schema structure to identify JOIN-heavy hot paths; recommending specific denormalization changes is complex
- **LLM dependency**: Required — LLM needed to understand query semantics, propose denormalization strategies, and assess data consistency trade-offs
- **Competitive moat**: VERY STRONG — No existing tool does this

---

## 3. Capacity Planning and Forecasting

### 3.1 Storage Growth Prediction

**What**: Track table and index sizes over time, apply time-series forecasting to predict "disk full" dates, and generate alerts at configurable horizons (30/60/90 days).

**Evidence**: SolarWinds uses ML algorithms for daily storage usage forecasts with 90-day advisory conditions. IDERA offers linear and exponential growth forecasting. tspDB (MIT) enables predictive query functionality in PostgreSQL itself. ManageEngine OpManager delivers AI/ML-powered storage forecasting. However, none of these are PostgreSQL-native sidecar agents.

**Rating**:
- **Impact**: HIGH — Disk full is the #1 cause of unplanned database outages; even 48 hours warning is valuable
- **Feasibility**: VERY HIGH — pg_sage already monitors table sizes; simple linear regression on historical sizes gives good predictions; pg_total_relation_size() is trivial to track
- **LLM dependency**: Optional — Time series forecasting is purely mathematical; LLM can generate human-readable reports
- **Competitive moat**: MODERATE — Many monitoring tools offer this, but none as a PostgreSQL sidecar with DBA-specific context

### 3.2 Connection Count Forecasting

**What**: Track active connections over time, correlate with application deployments, and predict when max_connections will be hit.

**Evidence**: PgBouncer sizing documentation (2025) emphasizes that total potential connections = num_pools x default_pool_size, and this must stay below max_connections. Connection exhaustion is the second most common outage cause after disk full.

**Rating**:
- **Impact**: HIGH — Connection exhaustion causes cascading failures
- **Feasibility**: VERY HIGH — pg_sage already tracks connections; trend analysis is straightforward
- **LLM dependency**: Optional — Purely mathematical forecasting; LLM useful for reports
- **Competitive moat**: LOW — Many monitoring tools do this; value is in integration with other pg_sage insights

### 3.3 Query Volume Trend Analysis

**What**: Track queries-per-second over time, detect trend changes (organic growth vs step changes from deployments), and project when current hardware becomes insufficient.

**Evidence**: Expert consensus (2025-2026) is that static forecasting is no longer sufficient; AI-driven capacity planning must evaluate historical consumption trends, workload growth, and seasonal fluctuations. An 85% increase in AI adoption for capacity planning is predicted.

**Rating**:
- **Impact**: MEDIUM — Useful for planning but less urgent than disk/connection forecasting
- **Feasibility**: HIGH — pg_stat_statements provides queries-per-interval data
- **LLM dependency**: Enhances — Trend detection is mathematical; LLM helps interpret deployment correlations and generate business-readable projections
- **Competitive moat**: LOW — Standard monitoring territory

### 3.4 Cloud Cost Projection

**What**: For RDS/Cloud SQL/Azure DB, project monthly costs based on current usage trends and recommend cost optimizations (gp3 migration, Graviton instances, Reserved Instances vs Savings Plans, storage tier changes).

**Evidence**: AWS Graviton4-based RDS instances (late 2025) deliver up to 40% performance improvement and 29% better price-performance. Migrating from io1/gp2 to gp3 can reduce storage costs by 72%. AWS Database Savings Plans (announced re:Invent 2025) provide new commitment options. Google Cloud SQL Recommender uses AI to observe usage patterns and offer actionable cost savings. Costimizer.ai and Sedai offer cloud database cost optimization guides.

**Rating**:
- **Impact**: VERY HIGH — Database costs are often the #1 or #2 cloud expense; savings of 30-70% are common
- **Feasibility**: MEDIUM — Requires cloud provider API integration (AWS CloudWatch, GCP Monitoring API, Azure Monitor) which adds significant complexity; pg_sage currently targets self-hosted PostgreSQL
- **LLM dependency**: Enhances — Cost calculation is deterministic; LLM generates reports and explains trade-offs
- **Competitive moat**: LOW for generic advice (cloud providers offer their own recommenders); HIGH if combined with pg_sage's deep workload knowledge (e.g., "you're paying for 8 cores but your peak is 3 cores, AND your top query could be 4x faster with this index, meaning you could drop to 2 cores")

---

## 4. Incident Response Automation

### 4.1 Automatic Root Cause Analysis

**What**: When performance degrades, automatically correlate multiple signals: pg_stat_statements changes, lock waits, replication lag, connection counts, bloat levels, vacuum state, and recent DDL changes to identify the root cause.

**Evidence**: incident.io's AI SRE (2025-2026) surfaces root causes by investigating immediately when alerts fire. BigPanda claims 50% MTTR reduction through automated root cause identification. DrDroid provides AI SRE agent for incident response. A 2023 Microsoft paper (arxiv 2305.15778) describes LLM-based automatic root cause analysis for cloud incidents. The "Five Whys" technique combined with knowledge-based GenAI agents revealed that 70% of incidents previously attributed to management failures were actually internal code issues.

**Rating**:
- **Impact**: VERY HIGH — MTTR reduction directly translates to revenue protection; database incidents typically cost $5-10K per minute for mid-size companies
- **Feasibility**: HIGH — pg_sage already collects all relevant signals; the correlation logic is the new work. A decision tree approach (check locks -> check vacuum -> check bloat -> check plans -> check config changes) covers 80% of cases.
- **LLM dependency**: Enhances — Decision tree covers common cases deterministically; LLM excels at unusual correlations and generating human-readable incident reports
- **Competitive moat**: VERY STRONG — No PostgreSQL-specific tool does automated root cause analysis. Generic AIOps tools lack database-specific knowledge.

### 4.2 "What Changed?" Analysis

**What**: Correlate performance shifts with: recent deployments (via git/CI integration), configuration changes (pg_settings snapshots), schema changes (DDL log), and query plan changes (EXPLAIN history).

**Evidence**: pganalyze (2025) correlates query statistics with plan changes to identify regressions. PostgreSQL 18 adds enhanced monitoring with buffer usage in EXPLAIN ANALYZE, pg_stat_io for I/O breakdowns, and pg_aios for async I/O tracking. Modern platforms offer automatic correlation of metrics, logs, and traces with root-cause-analysis algorithms.

**Rating**:
- **Impact**: HIGH — "What changed?" is the first question every DBA asks during an incident; automating this saves 30-60 minutes per incident
- **Feasibility**: HIGH — pg_sage can snapshot pg_settings, track DDL via event triggers, and diff pg_stat_statements between intervals. CI/deployment integration requires webhooks/API.
- **LLM dependency**: Enhances — Diff computation is deterministic; LLM generates narrative explanations ("Performance degraded at 14:32. At 14:28, a deployment introduced query X which is 47x more expensive than the query it replaced.")
- **Competitive moat**: STRONG — pganalyze does some of this but is SaaS-only and costs $500+/month

### 4.3 Runaway Query Termination

**What**: Automatically identify and terminate queries exceeding configurable thresholds (duration, rows examined, temp file size), with escalation policies and safe-harbor rules (never kill replication, never kill specific application users).

**Evidence**: PostgreSQL's statement_timeout and idle_in_transaction_session_timeout provide basic protection, but they are blunt instruments. More sophisticated approaches use pg_stat_activity monitoring with configurable rules. Third-party tools can automatically terminate transactions exceeding lock wait thresholds.

**Rating**:
- **Impact**: HIGH — A single runaway query can cascade into connection exhaustion and full outage
- **Feasibility**: VERY HIGH — pg_terminate_backend() is straightforward; the intelligence is in the policy (which queries are safe to kill, escalation, alerting)
- **LLM dependency**: Optional — Policy execution is deterministic; LLM useful for post-mortem analysis of terminated queries
- **Competitive moat**: LOW — Many tools do basic query killing; value is in pg_sage's holistic awareness (killed because it was causing lock chains affecting 47 other queries, not just because it was slow)

### 4.4 Lock Chain Resolution

**What**: Detect lock chains (query A blocked by B, which is blocked by C, which holds a lock on table T), visualize the chain, identify the root blocker, and recommend/execute resolution (terminate root blocker, or advise on lock ordering).

**Evidence**: PostgreSQL's pg_locks + pg_stat_activity reveals lock chains. Netdata (Aug 2025) published analysis of 10 real-world deadlock scenarios with resolutions. Resolve AI correlates deadlock errors with codebase patterns to find UPDATE statements across tables in different orders. eBPF-based early deadlock detection (pg_stat_kcache + uprobe) enables detection without performance overhead.

**Rating**:
- **Impact**: HIGH — Lock chains cause cascading failures; resolution requires expertise most teams lack
- **Feasibility**: HIGH — Lock chain detection from pg_locks is well-understood; recursive CTE on pg_locks reveals full chains. Resolution (identifying root blocker) is deterministic.
- **LLM dependency**: Enhances — Chain detection is deterministic; LLM generates human-readable explanations and correlates with application code patterns
- **Competitive moat**: STRONG — No sidecar agent automates this end-to-end

### 4.5 Replication Lag Diagnosis and Remediation

**What**: Monitor streaming replication lag, diagnose root cause (I/O bottleneck, network latency, long-running queries on replica, missing indexes on replica), and recommend/execute remediation.

**Evidence**: pg_stat_replication provides all necessary metrics. Common causes are well-documented: I/O bottlenecks on replica, network latency, replay conflicts with long queries, WAL accumulation. PostgresAI published a comprehensive troubleshooting guide (2025). Kyle Hailey documented Datadog-based monitoring showing that write-heavy spikes correlate with lag increases.

**Rating**:
- **Impact**: HIGH — Replication lag causes stale reads in read-replica architectures; severe lag can force failover
- **Feasibility**: HIGH — pg_sage can monitor pg_stat_replication; diagnosis decision tree covers most cases
- **LLM dependency**: Optional — Diagnosis is deterministic; LLM useful for report generation
- **Competitive moat**: MODERATE — Monitoring tools show lag metrics but don't diagnose root cause or suggest fixes

---

## 5. Security and Compliance

### 5.1 Overprivileged Role Detection

**What**: Analyze pg_roles and pg_default_acl to detect roles with excessive privileges: SUPERUSER on application accounts, CREATE/DROP on production databases, access to tables never queried by that role.

**Evidence**: pgdsat v2.0 (2026) checks ~90 PostgreSQL security controls including all CIS Benchmark recommendations for PostgreSQL 17. hoop.dev documents the specific risks of broad roles and recommends granular RBAC. The principle of least privilege is universally recommended but rarely enforced in practice.

**Rating**:
- **Impact**: HIGH — Overprivileged roles are the #1 database security finding in audits; compliance requirements (SOC 2, PCI-DSS, HIPAA) mandate least-privilege enforcement
- **Feasibility**: VERY HIGH — Catalog queries against pg_roles, pg_auth_members, information_schema.role_table_grants are straightforward; cross-referencing with pg_stat_user_tables shows which tables each role actually accesses
- **LLM dependency**: Enhances — Detection is deterministic; LLM generates remediation scripts and explains compliance implications
- **Competitive moat**: MODERATE — pgdsat does static assessment; pg_sage adds continuous monitoring and actual usage correlation

### 5.2 PII Detection in Columns

**What**: Scan column names, data types, and sample data to detect columns likely containing PII (email, phone, SSN, name, address, IP address, credit card) and flag those lacking encryption/masking.

**Evidence**: PiiCatcher uses regex on column names + NLP (spaCy) on sample data. OpenMetadata provides auto-classification with configurable confidence. Databricks' LogSentinel (2026) uses LLM-powered Mixture-of-Experts for context-dependent PII detection with automatic JIRA ticket generation. The approach: hierarchical classification with multiple models running in parallel, selecting the most confident prediction.

**Rating**:
- **Impact**: HIGH — GDPR fines up to 4% of annual revenue; PII exposure is the most expensive compliance failure
- **Feasibility**: MEDIUM — Column name regex catches 60-70% of cases (email, phone, ssn patterns). Data sampling adds accuracy but requires read access to data. JSONB PII detection is harder.
- **LLM dependency**: Required for high accuracy — Regex handles obvious cases; LLM needed for context-dependent classification (is "name" a person's name or a product name? Is "description" free-text that might contain PII?)
- **Competitive moat**: STRONG — No PostgreSQL sidecar does this; enterprise tools (DataSunrise, Strac) are expensive SaaS products

### 5.3 Audit Log Analysis

**What**: If pgaudit is enabled, analyze audit logs for suspicious patterns: bulk data exports, after-hours access, privilege escalation attempts, unusual query patterns from service accounts.

**Evidence**: DataSunrise and similar products provide this for enterprise customers. pgaudit provides the raw data (read, write, ddl operations logged per session). The gap is in automated analysis — most teams enable pgaudit but never analyze the logs systematically.

**Rating**:
- **Impact**: MEDIUM — High value for compliance but less common as a pain point than performance issues
- **Feasibility**: MEDIUM — Requires pgaudit to be enabled; log parsing and pattern detection are straightforward; anomaly detection (baseline + deviation) is moderately complex
- **LLM dependency**: Enhances — Pattern matching is deterministic; LLM helps identify novel suspicious patterns and generate incident reports
- **Competitive moat**: MODERATE — Enterprise SIEM tools do this; pg_sage's advantage is PostgreSQL-specific context

### 5.4 Row-Level Security Recommendations

**What**: For multi-tenant applications, detect tables that should have RLS but don't (tables with tenant_id columns lacking RLS policies), and generate policy recommendations.

**Evidence**: AI2sql (2025) generates RLS SQL from plain English. Bytebase automates approval workflows and policy generation. Supabase Agent Skills (2026) includes security and RLS as one of 8 prioritized optimization categories. RLS policy management is documented as a major DBA bottleneck — organizations accumulate hundreds of interconnected policies without versioning, dependency tracking, or impact analysis.

**Rating**:
- **Impact**: MEDIUM-HIGH — Critical for multi-tenant SaaS; less relevant for single-tenant deployments
- **Feasibility**: MEDIUM — Detecting missing RLS is straightforward (tables with tenant_id-like columns without policies); generating correct policies requires understanding application semantics
- **LLM dependency**: Required — Policy generation needs understanding of application intent; LLM can draft policies from column analysis and query patterns
- **Competitive moat**: STRONG — No PostgreSQL tool automatically recommends RLS policies based on schema analysis

### 5.5 SSL/TLS Configuration Audit

**What**: Check ssl, ssl_cert_file, ssl_key_file, ssl_ca_file settings; verify certificate validity/expiration; detect unencrypted connections via pg_stat_ssl.

**Evidence**: pgdsat v2.0 includes SSL/TLS checks as part of CIS Benchmark compliance. Certificate expiration is a common outage cause. pg_stat_ssl reveals which connections are encrypted and which are not.

**Rating**:
- **Impact**: MEDIUM — Important for compliance; certificate expiration causes outages
- **Feasibility**: VERY HIGH — Simple catalog and configuration checks
- **LLM dependency**: Optional — Entirely deterministic checks
- **Competitive moat**: LOW — pgdsat and basic monitoring tools cover this

---

## 6. Cost Optimization

### 6.1 Instance Right-Sizing

**What**: Analyze actual CPU, memory, and I/O utilization to recommend smaller (or larger) instance types, with specific instance recommendations per cloud provider.

**Evidence**: Google Cloud SQL Recommender (Active Assist) observes SQL usage patterns to offer right-sizing recommendations. Sedai provides automated right-sizing for RDS and Cloud SQL. The "2026 Database Architect's Guide to Cloud Optimization" (Virtual-DBA) documents that most databases are overprovisioned by 2-4x. AWS Graviton4 RDS instances (late 2025) deliver 40% better performance at lower cost.

Key insight from research: right-sizing should be based on real usage, not guesses, and done continuously.

**Rating**:
- **Impact**: VERY HIGH — 30-60% cost savings are typical; a db.r6g.2xlarge vs db.r6g.xlarge difference is ~$4,000/year
- **Feasibility**: MEDIUM — Requires cloud API integration; pg_sage currently focuses on PostgreSQL internals, not cloud infrastructure
- **LLM dependency**: Optional — Utilization analysis is deterministic; LLM generates persuasive reports for budget approvals
- **Competitive moat**: LOW as standalone (cloud providers offer this); HIGH when combined with pg_sage's query optimization (you need fewer resources after optimization)

### 6.2 Storage Tier Optimization

**What**: Recommend gp3 over gp2/io1 (up to 72% savings), analyze IOPS utilization vs provisioned IOPS, recommend storage type changes.

**Evidence**: Migrating from io1/gp2 to gp3 saves up to 72% on storage costs. gp3 allows independent IOPS and throughput configuration. Most databases are overprovisioned on IOPS.

**Rating**:
- **Impact**: HIGH — Storage is often 30-50% of the database bill
- **Feasibility**: MEDIUM — Requires cloud API integration
- **LLM dependency**: Optional — Deterministic cost comparison
- **Competitive moat**: LOW — Cloud providers and third-party tools do this

### 6.3 Read Replica Necessity Analysis

**What**: Analyze read/write ratio, query routing potential, and connection patterns to determine if read replicas are justified or if resources are wasted on underutilized replicas.

**Evidence**: Crunchy Data (2025) published analysis on determining if PostgreSQL is read-heavy or write-heavy and why it matters. A case study showed 85% read traffic making replicas seem obvious, but the actual decision depends on whether reads can tolerate staleness. WAL-based routing (read-your-write consistency) reduced primary CPU by 62%.

Key insight: read replicas need ongoing attention (monitoring, configuration, failover testing); the TCO often exceeds expectations.

**Rating**:
- **Impact**: HIGH — A single read replica costs $2-10K/year in cloud; unnecessary replicas are common
- **Feasibility**: HIGH — pg_stat_statements reveals read vs write query ratios; pg_sage can model what would happen if reads were offloaded
- **LLM dependency**: Enhances — Analysis is deterministic; LLM generates recommendation reports
- **Competitive moat**: MODERATE — Novel analysis combining workload knowledge with cost projection

### 6.4 Connection Pooler Sizing (PgBouncer Tuning)

**What**: Analyze connection patterns and recommend optimal PgBouncer configuration: pool_mode, default_pool_size, max_client_conn, reserve_pool_size, and per-database overrides.

**Evidence**: PgBouncer documentation (2025) establishes that total potential connections = num_pools x default_pool_size, which must stay below max_connections. The starting point for pool size is CPU count. Percona (2025) documents that PostgreSQL's process-per-connection architecture doesn't scale because every connection forks a 5+ MB process. Azure defaults to TRANSACTION mode.

**Rating**:
- **Impact**: MEDIUM-HIGH — Proper pooler sizing prevents connection exhaustion and reduces PostgreSQL memory overhead by 50-80%
- **Feasibility**: HIGH — pg_sage already analyzes connection pools; extending to PgBouncer config recommendations is natural
- **LLM dependency**: Optional — Sizing formulas are deterministic; LLM explains trade-offs between pool modes
- **Competitive moat**: MODERATE — pg_sage's advantage is integrating pooler recommendations with its broader performance analysis

---

## 7. Workload Analysis

### 7.1 Batch vs OLTP Pattern Detection and Separation Advice

**What**: Identify queries that are analytical/batch (full table scans, aggregations, long duration) mixed with OLTP (point lookups, short transactions) and recommend workload separation strategies.

**Evidence**: The HTAP debate (2025-2026) concluded that the dominant architecture remains separate OLTP and OLAP systems. A May 2025 "HTAP is Dead" argument (InfoQ) cited resource contention, system complexity, and hardware constraints. The practical recommendation: separate workloads via read replicas, or time-based scheduling (batch jobs during off-peak hours).

**Rating**:
- **Impact**: HIGH — Mixed workloads cause unpredictable performance; separation can improve P99 latency by 5-10x
- **Feasibility**: HIGH — pg_stat_statements query classification (duration, rows, buffers) enables automatic OLTP/OLAP categorization
- **LLM dependency**: Enhances — Classification is deterministic; LLM helps design separation strategy
- **Competitive moat**: STRONG — No PostgreSQL tool automatically classifies and recommends workload separation

### 7.2 Time-of-Day Pattern Analysis for Maintenance Windows

**What**: Analyze query volume, CPU, and I/O patterns across 24-hour/7-day cycles to recommend optimal maintenance windows for VACUUM, REINDEX, and backups.

**Evidence**: Current best practice is to schedule maintenance during lowest-activity periods, but most teams guess rather than measure. pg_sage already manages vacuum; adding temporal awareness makes scheduling smarter.

**Rating**:
- **Impact**: MEDIUM — Reduces maintenance impact on production traffic by 50-80%
- **Feasibility**: VERY HIGH — pg_stat_statements has queryid + calls_per_interval; simple histogram analysis reveals patterns
- **LLM dependency**: Optional — Pattern detection is mathematical; LLM generates human-readable schedule recommendations
- **Competitive moat**: MODERATE — Simple but surprisingly nobody does this automatically

### 7.3 Read/Write Ratio Analysis for Replica Routing

**What**: Classify queries as read vs write, track ratios over time, and recommend which specific queries should route to replicas (safe reads that tolerate staleness vs reads that need consistency).

**Evidence**: WAL-based read-your-write routing (2025) uses LSN comparison to route reads to replicas only when they've caught up. This approach reduced primary CPU by 62%. The key innovation is per-query routing decisions based on consistency requirements, not blanket read/write splitting.

**Rating**:
- **Impact**: HIGH — Proper read routing can offload 40-70% of primary server load
- **Feasibility**: HIGH — pg_stat_statements identifies reads (SELECT) vs writes; staleness analysis requires understanding application semantics
- **LLM dependency**: Enhances — Read/write classification is deterministic; LLM helps identify which reads tolerate staleness
- **Competitive moat**: STRONG — Per-query routing recommendations based on consistency analysis is novel

### 7.4 Hot Table and Hot Row Detection

**What**: Identify tables and specific rows with disproportionate access (sequential scans, index scans, updates, deletes) that cause contention, and recommend mitigation (partitioning, caching, queue-based writes).

**Evidence**: dryrun (2026) collects per-replica statistics to detect sequential scan hotspots and routing imbalances. HTAP research (2024-2025) documents tiered caching for hot data and adaptive replication. PostgreSQL's pg_stat_user_tables provides per-table access statistics; pg_stat_all_indexes reveals per-index scan counts.

**Rating**:
- **Impact**: HIGH — Hot tables cause I/O contention and lock conflicts; hot rows cause serialization bottlenecks
- **Feasibility**: HIGH — pg_stat_user_tables and pg_stat_all_indexes provide table-level metrics; row-level detection requires analyzing pg_stat_user_tables update/delete counts combined with table structure analysis
- **LLM dependency**: Enhances — Detection is deterministic; LLM recommends mitigation strategies (partitioning scheme, caching layer, etc.)
- **Competitive moat**: MODERATE — Table-level is standard; row-level contention detection combined with remediation advice is unique

---

## 8. Developer Experience

### 8.1 Natural Language Query Explanation

**What**: Take an EXPLAIN ANALYZE output and generate a plain-English explanation: what the query does, why it's slow, what the optimizer chose and why, and what would make it faster.

**Evidence**: PGConf India 2026 features "PostgreSQL Performance Clinic: Live Diagnosis and Tuning" talks. pgMustard provides scored performance advice from EXPLAIN output. pganalyze compares EXPLAIN plans with text-based diffs. explain.dalibo.com visualizes plans. However, none provide natural language explanations accessible to developers who don't understand EXPLAIN output.

Research (2025-2026) on Natural Language Interfaces for Databases shows growing interest but acknowledges "practical deployment depends on user trust, interaction quality, and overall usability."

**Rating**:
- **Impact**: HIGH — Democratizes performance understanding; reduces DBA bottleneck by 50%+ for "why is this slow?" questions
- **Feasibility**: HIGH — EXPLAIN JSON output is structured and well-documented; LLM interpretation is straightforward with proper prompting
- **LLM dependency**: Required — This is fundamentally an LLM task
- **Competitive moat**: STRONG — pgMustard gives tips but not narrative explanations; this would be a genuine developer-facing differentiator

### 8.2 Pre-Deployment Index Suggestion

**What**: Given a SQL query (before it runs in production), analyze it against the current schema and suggest optimal indexes, with estimated performance impact.

**Evidence**: pg_sage already does index optimization for running queries. Extending to pre-deployment (analyzing queries from code review/CI) is a natural evolution. HypoPG enables hypothetical index testing without creating real indexes.

**Rating**:
- **Impact**: HIGH — Preventing slow queries before deployment is 100x cheaper than fixing them in production
- **Feasibility**: HIGH — Requires CI/CD integration (receive query, run EXPLAIN against production schema with HypoPG)
- **LLM dependency**: Optional — Index analysis is deterministic with HypoPG; LLM enhances by explaining recommendations
- **Competitive moat**: MODERATE — pganalyze offers some pre-deployment analysis; pg_sage's HypoPG integration is an advantage

### 8.3 Migration Risk Assessment

**What**: "This ALTER TABLE ADD COLUMN with DEFAULT will lock your 500M row table for ~45 minutes. Here's a safe alternative that takes 3 steps but has zero downtime."

**Evidence**: This is a repackaging of 2.2 (Safe Migration Planning) with a developer-facing UX. The risk formula (Lock Severity x Duration x Traffic) combined with actual table statistics makes estimates actionable. dryrun (2026) and Squawk provide static analysis, but neither gives time estimates based on actual table sizes.

**Rating**:
- **Impact**: VERY HIGH — Migration-related outages are the most common preventable database incidents
- **Feasibility**: HIGH — Combines existing pg_sage knowledge (table sizes, write rates) with DDL analysis
- **LLM dependency**: Enhances — Time estimation is mathematical; LLM generates step-by-step safe migration plans
- **Competitive moat**: STRONG — Time estimates based on actual data are unique

### 8.4 Query Plan Diff Between Environments

**What**: Compare EXPLAIN output between staging and production for the same query, highlighting differences in plans, costs, and row estimates that predict production performance problems.

**Evidence**: pganalyze (2025-2026) provides text-based EXPLAIN plan diff with focus on structural differences. pgplan (CLI tool) compares two plans reporting cost, time, row estimate, and buffer differences across every node. Bytebase supports cross-environment query performance comparison. Key insight: plan differences often predict production issues before they happen.

**Rating**:
- **Impact**: MEDIUM-HIGH — Catches plan regressions before production deployment
- **Feasibility**: MEDIUM — Requires access to both environments or at minimum schema+stats from staging to compare against production
- **LLM dependency**: Enhances — Diff computation is deterministic; LLM explains why plans differ and what it means for performance
- **Competitive moat**: MODERATE — pganalyze does this as SaaS; pg_sage would provide it as part of the sidecar

---

## 9. Competitive Landscape

### Direct Competitors (PostgreSQL-Specific AI/Autonomous DBA)

| Product | Status | Focus | Strengths | Weaknesses |
|---------|--------|-------|-----------|------------|
| **DBtune** | Active (Malmo, Sweden; 2.4M EUR funding 2023) | Knob tuning only | Stanford/Lund research backing; AWS/Azure/GCP support | Narrow scope (knobs only); SaaS model; no query/schema analysis |
| **OtterTune** | DEAD (ceased 2024) | Knob tuning | Carnegie Mellon research; Andy Pavlo's reputation | Failed as a business; "Son of OtterTune" pivoting to proxy/OEM |
| **Crystal DBA** | Stalled (last commit Jan 2026; 25 open issues) | MCP-based advisor | Autonomous vehicle safety methodology; PostgreSQL focus | Appears abandoned; MCP-only interface; no autonomous operation |
| **dba.ai** | Active (2025-2026) | Autonomous performance engineering | End-to-end PostgreSQL optimization; autonomous operation | Limited public documentation; unclear production deployments |
| **Supabase Agent Skills** | Active (Jan 2026) | AI agent guidelines | 18+ AI agent compatibility; 8 optimization categories | Static knowledge (guidelines, not runtime analysis); Supabase-specific |
| **boringSQL dryrun** | Active (2026) | Schema intelligence | 33 lint rules; migration safety; offline-first; MCP integration | Offline/static analysis only; no autonomous operation |
| **Squawk** | Active (OSS) | Migration linting | Focused safety checks; CI integration | Narrow scope (DDL linting only) |
| **pganalyze** | Active (SaaS) | Query performance monitoring | EXPLAIN plan comparison; query analysis | SaaS-only ($500+/month); no autonomous remediation |
| **pgMustard** | Active (SaaS) | EXPLAIN visualization | Scored performance advice | Manual tool; no automation |
| **Aiven AI Optimizer** | Active (Cloud feature) | Index recommendations | Cloud-native integration | Aiven-only; limited to indexes |

### pg_sage Competitive Advantages

1. **Sidecar architecture** — Runs alongside PostgreSQL, not as SaaS; data never leaves the network
2. **Full-spectrum analysis** — Already covers 9+ DBA domains vs competitors' 1-2
3. **Autonomous operation** — Acts, doesn't just advise (with trust-level gating)
4. **Open source** — No vendor lock-in
5. **LLM-enhanced, not LLM-dependent** — Works without LLM; LLM makes it smarter

### Gaps vs Competition

1. No migration safety analysis (Squawk, dryrun have this)
2. No cloud cost optimization (cloud providers own this)
3. No developer-facing query explanation (pgMustard has basic version)
4. No pre-deployment CI/CD integration

---

## 10. Academic Research (2025-2026)

### Key Papers

| Paper | Venue | Key Finding | pg_sage Relevance |
|-------|-------|-------------|-------------------|
| **GPTuner** (Tang et al.) | VLDB 2024, SIGMOD Record 2025 | LLM reads DBMS manuals to suggest knob configurations; 16x faster than SOTA, 30% better performance | pg_sage already does knob tuning; GPTuner's manual-reading approach could improve it |
| **AgentTune** | SIGMOD 2025 | Agent-based LLM framework for database knob tuning | Validates agent-based approach; directly relevant architecture |
| **LLMTune** | arXiv 2024 | LLMs recommend initial configurations from historical tuning tasks | Transfer learning across workloads is relevant for fleet mode |
| **LGTune** | 2025 | LLM-guided reinforcement learning for knob tuning | Hybrid RL+LLM approach shows promise for complex tuning |
| **DemoTuner** | arXiv 2025 | Demonstration-based RL for automatic knob tuning | Learning from examples rather than exploration |
| **lambda-Tune** (Giannakouris, Trummer) | SIGMOD 2025 | LLM generates entire configuration scripts from tuning context documents | Validates holistic configuration generation vs per-knob tuning |
| **SERAG** | arXiv 2025 | Self-Evolving RAG for query optimization | RAG-based approach to improving query optimization over time |
| **RankPQO** | VLDB 2025 | Learning-to-Rank for parametric query optimization | ML-based query plan selection for parameterized queries |
| **Automatic RCA via LLMs** (Microsoft) | arXiv 2023 | LLM-based root cause analysis for cloud incidents | Template for pg_sage's incident response |
| **HTAP Databases Survey** | arXiv 2024 | Comprehensive survey of hybrid transactional/analytical processing | Informs workload separation recommendations |

### Research Trends

1. **LLM + RL hybrid** — Using LLM for initial recommendations, RL for refinement
2. **RAG for database knowledge** — Retrieval-augmented generation using DBMS documentation
3. **Transfer learning across workloads** — Applying tuning knowledge from one database to another (relevant for fleet mode)
4. **Self-evolving systems** — Agents that improve their own optimization strategies over time

---

## 11. Prioritized Roadmap Recommendation

### Tier 1: High Impact, High Feasibility (Next 2 releases)

These build on pg_sage's existing architecture with minimal new infrastructure.

| # | Feature | Impact | Feasibility | LLM | Est. Effort |
|---|---------|--------|-------------|-----|-------------|
| 1 | **Safe Migration Planning** (2.2) | Very High | High | Enhances | 2-3 weeks |
| 2 | **Root Cause Analysis** (4.1) | Very High | High | Enhances | 3-4 weeks |
| 3 | **Schema Anti-Pattern Detection** (2.1) | High | High | Enhances | 2 weeks |
| 4 | **Lock Chain Resolution** (4.4) | High | High | Enhances | 1-2 weeks |
| 5 | **Storage Growth Prediction** (3.1) | High | Very High | Optional | 1 week |
| 6 | **Runaway Query Termination** (4.3) | High | Very High | Optional | 1 week |

**Rationale**: These are the "pays for itself" features. Migration safety and root cause analysis directly prevent outages. Schema anti-patterns and lock chain resolution demonstrate deep DBA expertise. Storage prediction is low-effort, high-visibility.

### Tier 2: High Impact, Moderate Effort (Following 2-3 releases)

These require new subsystems or LLM integration.

| # | Feature | Impact | Feasibility | LLM | Est. Effort |
|---|---------|--------|-------------|-----|-------------|
| 7 | **Natural Language Query Explanation** (8.1) | High | High | Required | 2 weeks |
| 8 | **N+1 Query Detection** (1.5) | High | Medium | Required | 3-4 weeks |
| 9 | **Materialized View Recommendations** (1.1) | High | High | Enhances | 2-3 weeks |
| 10 | **"What Changed?" Analysis** (4.2) | High | High | Enhances | 2-3 weeks |
| 11 | **Batch vs OLTP Separation** (7.1) | High | High | Enhances | 2 weeks |
| 12 | **Overprivileged Role Detection** (5.1) | High | Very High | Enhances | 1 week |

**Rationale**: Natural language explanations and N+1 detection are genuine differentiators with no current competition. Materialized view recommendations extend pg_sage's query optimization moat. "What Changed?" analysis addresses the most common DBA question during incidents.

### Tier 3: Strategic Differentiators (6-12 month horizon)

| # | Feature | Impact | Feasibility | LLM | Est. Effort |
|---|---------|--------|-------------|-----|-------------|
| 13 | **PII Detection** (5.2) | High | Medium | Required | 3-4 weeks |
| 14 | **Cloud Cost Projection** (3.4) | Very High | Medium | Enhances | 4-6 weeks |
| 15 | **Subquery Flattening** (1.3) | High | Medium | Required | 3-4 weeks |
| 16 | **Denormalization Recommendations** (2.4) | High | Medium | Required | 4-6 weeks |
| 17 | **Pre-Deployment Index Suggestion** (8.2) | High | High | Optional | 2-3 weeks |
| 18 | **Migration Risk Assessment** (8.3) | Very High | High | Enhances | 2-3 weeks (builds on 2.2) |
| 19 | **RLS Recommendations** (5.4) | Medium-High | Medium | Required | 3-4 weeks |

### Tier 4: Nice-to-Have (Opportunistic)

| # | Feature | Impact | Effort | Notes |
|---|---------|--------|--------|-------|
| 20 | Connection Count Forecasting (3.2) | High | Low | Quick win |
| 21 | CTE Optimization (1.2) | Medium | Low | Straightforward |
| 22 | Partition Pruning (1.4) | High | Medium | Niche audience |
| 23 | Time-of-Day Patterns (7.2) | Medium | Low | Quick win |
| 24 | Query Plan Diff (8.4) | Medium-High | Medium | Needs multi-env access |
| 25 | Read Replica Analysis (6.3) | High | High | Cloud-specific |
| 26 | SSL/TLS Audit (5.5) | Medium | Very Low | Trivial to add |
| 27 | Audit Log Analysis (5.3) | Medium | Medium | Requires pgaudit |
| 28 | Instance Right-Sizing (6.1) | Very High | Medium | Cloud API dependency |
| 29 | PgBouncer Tuning (6.4) | Medium-High | High | External tool dependency |
| 30 | Column Type Optimization (2.3) | Medium | High | Low urgency |

### Killer Feature Combination

The single most compelling pg_sage pitch combines features 1, 2, 8, and 14:

> "pg_sage detected an N+1 query pattern hitting your orders table 47,000 times per hour. It recommended a batched query that would reduce load by 94%. After applying the fix, it predicted you could downsize from db.r6g.4xlarge to db.r6g.xlarge, saving $18,400/year. When the migration to add the new index caused unexpected lock contention, pg_sage automatically identified the root cause (a long-running analytics query), terminated it safely, and completed the migration in 12 seconds."

That story — detect, fix, save money, handle the incident — is what sells an autonomous DBA agent.
