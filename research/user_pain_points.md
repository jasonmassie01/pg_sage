# PostgreSQL DBA Pain Points: Real-World Research

> Compiled: 2026-04-12
> Sources: Hacker News, blog posts, postmortems, DBA forums, community articles, X/Twitter discussions
> Focus: Real problems people actually complain about, not theoretical ones

---

## Executive Summary

After surveying dozens of community discussions, production postmortems, blog posts, and DBA forum threads, the same pain points appear repeatedly. They cluster into **10 major categories**, ranked by frequency and severity of complaints. Each maps to existing or potential pg_sage features.

---

## 1. VACUUM / Autovacuum / Table Bloat Management

**Frequency:** VERY HIGH -- appears in nearly every PostgreSQL pain point discussion
**LLM-solvable:** YES -- monitoring, tuning recommendations, proactive alerting

### The Problem
PostgreSQL's MVCC implementation creates dead tuples that must be cleaned up by VACUUM. Autovacuum is supposed to handle this automatically but frequently falls behind on high-churn tables, leading to table bloat, index bloat, and eventual performance degradation.

### User Quotes
- "Postgres requires constant babysitting, vacuums, reindexing, various sorcery." -- HN user (2026)
- "I've started moving away from Postgres to MySQL and SQLite. I don't want to deal with the vacuums/maintenance/footguns." -- HN user (2026)
- "Autovacuum is not 'set and forget.' It's 'set, monitor, and tune constantly.'" -- Database Disasters 2024-2025
- "Table bloat in PostgreSQL is like a slow leak -- you don't notice it until your disk is full and queries are crawling." -- Cloud SQL debugging guide
- "The default autovacuum setup does work most of the time, but when it doesn't, good lord." -- Bytebase

### Real Incidents
- Production database had 1.38 billion live tuples with 612 million dead tuples (44.2% bloat) from 3.25 billion deletes without corresponding VACUUM. (dev.to deep dive)
- Zombie autovacuum processes holding locks prevented maintenance tasks, creating a vicious cycle where bloat accumulated faster than it could be cleared.
- Replication slots blocking vacuum on primary: "If the replica is down or severely behind, the rows in the replication slot can't be vacuumed on the primary."

### pg_sage Mapping
- **Existing:** Bloat detection, autovacuum monitoring, vacuum recommendations
- **Potential:** Per-table autovacuum parameter tuning recommendations, bloat trend prediction, proactive alerts before bloat reaches critical levels, automatic detection of zombie autovacuum processes

---

## 2. Query Performance Diagnosis & Optimization

**Frequency:** VERY HIGH -- the #1 reason people seek help
**LLM-solvable:** YES -- EXPLAIN plan analysis, query rewriting, index suggestions

### The Problem
90% of database performance problems come from 10% of queries, but most teams don't know which queries are the problem until production starts failing. Understanding EXPLAIN ANALYZE output requires deep expertise. The query planner sometimes makes bad decisions due to stale statistics.

### User Quotes
- "We didn't have query metadata or anything similar -- pg_stat_statements wasn't enabled, and we had very little visibility into query history." -- dev.to PostgreSQL deep dive
- "The PostgreSQL explain plan does not provide enough information for finding index filter predicates." -- QueryDoctor
- "A large batch DELETE can lead to wrong index selections because Postgres didn't immediately update its internal statistics." -- Crunchy Data
- "I no longer spend hours manually parsing query stats." -- AI DBA tool author on HN
- Both POWA and ChatGPT recommended the same indexes, but ChatGPT delivered superior value through explanation depth -- query improved from 741ms to under 50ms. (LLM vs POWA comparison)

### Real Incidents
- A simple COUNT query ran for 10 minutes before requiring cancellation due to bloated tables, forcing the team to use Spark for estimation.
- Production database "spiraled into a full meltdown" because Postgres refuses to support query planner hints for emergency override.

### pg_sage Mapping
- **Existing:** Query analysis via pg_stat_statements, EXPLAIN plan interpretation, index recommendations
- **Potential:** Natural language EXPLAIN plan translation, query rewrite suggestions, automatic detection of queries with stale statistics, regression detection when query plans change after ANALYZE

---

## 3. Index Selection & Management

**Frequency:** HIGH -- consistently in top 5 complaints
**LLM-solvable:** YES -- primary LLM strength area

### The Problem
Knowing which indexes to create, which are unused, and which are bloated requires expertise most teams lack. PostgreSQL lacks MySQL's "invisible indexes" feature for safe testing. Missing indexes cause slow queries; excess indexes slow writes and waste storage.

### User Quotes
- "POWA recommended indexes on created_timestamp, but it did not take into account combined columns." -- LLM vs POWA study
- MySQL's invisible indexes allow "test index effectiveness without impacting existing queries or disable indexes temporarily -- a crucial feature for production optimization." PostgreSQL lacks this. -- Bytebase
- "Vacuum was taking hours on tables that should have been much quicker to vacuum, all because the indexes were severely bloated." -- Kendra Little

### pg_sage Mapping
- **Existing:** Index recommendations via HypoPG, unused index detection, index bloat monitoring
- **Potential:** Composite index suggestions, partial index recommendations, index consolidation (detecting redundant indexes), safe index testing workflow (simulate invisible indexes)

---

## 4. Connection Management & Pool Exhaustion

**Frequency:** HIGH -- frequent production incident trigger
**LLM-solvable:** PARTIALLY -- diagnosis and configuration advice, not runtime pooling

### The Problem
PostgreSQL's process-per-connection model doesn't scale well. Connection exhaustion is one of the most common causes of production outages. PgBouncer adds complexity with its own set of footguns (session-level state, prepared statements, authentication).

### User Quotes
- "Having more native support for general connection pooling has been a requested feature in Postgres for a while." -- HN discussion
- "The bug is almost never that you forgot to use a pool. It's that you accidentally created too many of them." -- DEV Community
- "PgBouncer has three modes with different tradeoffs, the configuration is full of footguns, and if you get it wrong you get subtle bugs that are much harder to debug than the original connection error." -- Aiven troubleshooting guide
- "Serverless talking to a database sucks." -- HN user on connection overhead
- "Multiple Celery workers, each running several concurrent processes, were quietly opening more connections than the database could handle." -- Production outage postmortem

### Real Incidents
- Clerk.com (Sept 2025): A PostgreSQL minor version upgrade changed connection lock handling. Combined with static 15-minute connection lifetime, all containers synchronized their connection recycling, creating a "thundering herd" every 15 minutes. Took 4 days to diagnose.
- Connection pool exhaustion from misconfigured ORM (set to 150 instead of 300) caused complete application stall during traffic surge.

### pg_sage Mapping
- **Existing:** Connection monitoring
- **Potential:** Connection pool health analysis, idle connection detection, PgBouncer configuration validation, connection leak detection, thundering herd pattern detection

---

## 5. Monitoring & Alerting Gaps

**Frequency:** HIGH -- frustration with every existing tool
**LLM-solvable:** YES -- contextual analysis is where LLMs excel

### The Problem
Generic monitoring tools (Datadog, New Relic) show that the database is slow but not why. PostgreSQL-specific tools require installation and expertise. Alert fatigue from threshold-based alerts that lack context. DBAs spend more time diagnosing than fixing.

### User Quotes
- "Forty-seven alerts in one hour. All saying the same thing: 'connection count above threshold.' Not a single one mentioned that one idle-in-transaction session was holding a lock." -- Philip McClarence (2026)
- "Forty minutes of investigation for what turned out to be a 30-second fix." -- same author
- "Datadog's PostgreSQL monitoring is a thin integration layer with no EXPLAIN plans, no index advisor, no vacuum analysis, no bloat detection, and no health scoring -- it just tells you that the database is slow." -- CubeAPM comparison
- "During incidents I've often found that the issue has been obvious or brewing for some time." -- HN user on Xata Agent
- "I do wonder about cost of this at scale; compared to the cost of the services being monitored. Hopefully an Agent tax doesn't become another Datadog tax." -- HN user
- "Cloud status pages lag reality. Independent health checks prove essential." -- Database Disasters 2024-2025

### What DBAs Actually Want
1. Contextual intelligence at alert time (breakdown of active vs. idle connections, PIDs, durations, suggested fixes)
2. Severity-appropriate routing (P1/P2 immediate notification; P3/P4 daily digests)
3. Historical trending to identify false positives and tune thresholds
4. Alerts that tell you WHY, not just WHAT

### pg_sage Mapping
- **Existing:** Health checks, monitoring dashboard
- **Potential:** Root-cause analysis in alerts (not just "connections high" but "idle-in-transaction session PID 12345 holding lock for 47 minutes"), trend detection, predictive alerting, smart severity classification

---

## 6. Long-Running Transactions & Idle-in-Transaction

**Frequency:** HIGH -- cascading impact makes this especially painful
**LLM-solvable:** YES -- detection, alerting, and remediation guidance

### The Problem
Idle-in-transaction sessions hold locks, block autovacuum, prevent dead row cleanup, and can cause table bloat and transaction ID wraparound. They're usually application bugs (transaction opened but never committed/rolled back).

### User Quotes
- "A session that shows the same query over and over but is always idle in transaction is likely a bug in your application -- a transaction was opened but never committed or rolled back." -- PostgreSQL community
- Long-running transactions "can cause VACUUM to not clean out dead rows" leading to cascading bloat problems.

### pg_sage Mapping
- **Existing:** Activity monitoring
- **Potential:** Idle-in-transaction detection with automatic alerting, correlation between long transactions and bloat growth, application-level transaction pattern analysis, recommended timeout settings

---

## 7. Schema Migrations & DDL Locking

**Frequency:** HIGH -- especially in CI/CD environments
**LLM-solvable:** PARTIALLY -- can advise on safe migration patterns

### The Problem
DDL operations acquire ACCESS EXCLUSIVE locks that block all reads and writes. A migration waiting for a lock queues everything behind it, exhausting connection pools within seconds. Zero-downtime migrations require careful orchestration that most teams get wrong.

### User Quotes
- "A seemingly harmless schema change can bring your entire application to a halt if not executed carefully." -- Bytebase
- "A migration tries to acquire ACCESS EXCLUSIVE on a table where a long-running query is already active, the migration waits. While it waits, every new query that touches that table queues behind the migration. Within seconds, your connection pool is full." -- PostgresAI
- PostgreSQL "still lacks" mature, production-ready tools comparable to MySQL's gh-ost and Percona's pt-online-schema-change. -- Bytebase

### pg_sage Mapping
- **Existing:** Lock monitoring
- **Potential:** DDL lock risk analysis (warn before running ALTER TABLE on busy tables), safe migration pattern recommendations, lock queue depth monitoring, migration dry-run analysis

---

## 8. WAL Management & Disk Space Emergencies

**Frequency:** MEDIUM-HIGH -- extremely painful when it happens
**LLM-solvable:** YES -- monitoring and prevention

### The Problem
The pg_wal directory fills up when replication slots lag, archive commands fail, or checkpoints are misconfigured. When pg_wal fills the disk, PostgreSQL panics and terminates all connections. Recovery under disk pressure is extremely stressful.

### User Quotes
- "When the pg_wal disk fills up, PostgreSQL panics and terminates the server and all connections since the database cannot make any more changes." -- Percona
- "If archive_command is set but failing silently, disk usage will grow without warning." -- CYBERTEC
- "Disk full causes immediate database shutdown" -- it's "the one alert you absolutely cannot miss." -- DrDroid monitoring guide
- Pro tip: "Create a dummy file beforehand of specific size (e.g., 300MB) in the pg_wal directory, which is handy to have when needing the space back" during recovery.

### pg_sage Mapping
- **Existing:** Basic disk monitoring
- **Potential:** WAL growth rate prediction, replication slot health monitoring, archive command success rate tracking, disk space exhaustion forecasting, automatic alerting on inactive replication slots

---

## 9. Replication Lag & High Availability

**Frequency:** MEDIUM-HIGH -- critical for production systems
**LLM-solvable:** PARTIALLY -- diagnosis and tuning advice

### The Problem
Replication lag has multiple causes (network, I/O contention, lock replay, long transactions) making diagnosis complex. DDL commands cause extended lag on replicas due to exclusive locks. Vanilla PostgreSQL lacks built-in automatic failover.

### User Quotes
- "Fixing PostgreSQL replication lag isn't always about tuning one parameter -- it's an exercise in deep debugging, infrastructure awareness, and operational rigor." -- Sunny Jain (Medium)
- "If the real issue is lock contention, adding memory or indexes may not help at all." -- AWS
- "Vanilla PostgreSQL clustering for geographic distribution was described as 'pulling teeth' compared to MySQL variants with built-in HA tooling." -- HN discussion
- "Postgres is not a CP database" and "can lose writes during network partitions even with synchronous replication." -- HN user
- MySQL's Group Replication provides "both single-primary and multi-primary replication with automatic failover and conflict detection built-in" while PostgreSQL "relies on external tools like Patroni, pg_auto_failover" requiring "non-trivial operational overhead." -- Bytebase

### pg_sage Mapping
- **Existing:** Replication monitoring
- **Potential:** Multi-dimensional replication lag analysis (network vs. I/O vs. lock replay), replication slot health dashboard, failover readiness assessment, replica promotion risk analysis

---

## 10. Major Version Upgrades

**Frequency:** MEDIUM -- painful but infrequent (every 1-2 years)
**LLM-solvable:** PARTIALLY -- planning and compatibility analysis

### The Problem
Major version upgrades require significant downtime for large databases. pg_upgrade needs double the disk space. Logical replication for zero-downtime upgrades is complex. Post-upgrade, statistics need rebuilding (improved in PG18). Extension compatibility breaks.

### User Quotes
- "Even with the right tools and experience, most PostgreSQL upgrades still need breathing room, as something always takes longer than expected." -- Data Egret (2026)
- "You need roughly double the disk space, and the downtime is proportional to the size of your data, with multi-TB databases potentially experiencing hours or even days of downtime." -- pgEdge
- "Organizations still running PostgreSQL 13 (which stopped receiving security patches in November 2025) are now vulnerable to every future security disclosure." -- Database Disasters 2024-2025

### pg_sage Mapping
- **Existing:** Version awareness
- **Potential:** Upgrade readiness assessment, extension compatibility check, pre-upgrade checklist generation, post-upgrade statistics rebuild monitoring, EOL version alerting

---

## 11. Configuration Tuning

**Frequency:** MEDIUM -- but affects everything else
**LLM-solvable:** YES -- well-suited to rule-based + contextual analysis

### The Problem
PostgreSQL ships with conservative defaults optimized for a Raspberry Pi. Every production deployment needs tuning, but the parameter space is large and interactions are complex. Tools like PGTune exist but provide only starting points.

### User Quotes
- "Most issues aren't actually bugs; they're things like permissions problems, connection failures, or a full disk, and the tricky part is figuring out what the message actually means and how to fix it fast." -- Percona

### pg_sage Mapping
- **Existing:** Configuration analysis
- **Potential:** Workload-aware configuration recommendations, parameter interaction analysis, configuration drift detection, before/after comparison for config changes

---

## Bonus: Emerging Pain Points

### Security & Credential Management
- PostgreSQL CVE-2025-1094 SQL injection vulnerability exploited in the wild against BeyondTrust and the U.S. Treasury.
- Questions about sending database info to third-party AI tools: "Are there risks associated with sending DB info off to these third parties?" -- HN user on Xata Agent

### AI Tool Skepticism
- "Just give me a real example of when this design gave real advice and actually optimized PostgreSQL?" -- HN commenter challenging AI DBA tools
- "Using preset commands doesn't prevent LLM hallucinations from causing unintended behavior." -- HN skeptic
- "[Xata Agent] is an expert at MONITORING PostgreSQL. It's not for writing queries from natural language. I'm extremely interested in the latter but not at all in the first." -- HN user distinguishing monitoring from optimization

### DBA Role Evolution
- "The agent dramatically increases my ability to get things done, to the point where it's not too hard for me to see how _my_ role in the situation may rapidly become optional, then unnecessary." -- Kendra Little (25-year DBA veteran, 2025)
- "An AI Agent Doesn't have to be Perfect -- Just Make Fewer Mistakes Than a Person." -- Kendra Little
- Database administrators face 48% overall AI exposure, with cloud-managed databases and AI-driven optimization automating routine tasks.

---

## Competitive Landscape: AI-Powered Postgres Tools (2025-2026)

| Tool | Approach | Strengths | Weaknesses |
|------|----------|-----------|------------|
| **pganalyze** | SaaS, index advisor | Mature indexing engine, EXPLAIN plan analysis | Expensive, SaaS-only, no autonomous action |
| **Xata Agent** | Preset SQL commands, read-only | Safe (locked down), good monitoring | No optimization actions, monitoring only |
| **PlanetScale AI** | LLM + HypoPG | Good index suggestions | Limited to PlanetScale platform |
| **AWS PI Reporter** | Bedrock/Claude integration | Cloud-native, good for RDS/Aurora | AWS-only, requires Performance Insights |
| **Aiven SQL Optimizer** | Online tool | Easy to use | Manual, no continuous monitoring |
| **pg_sage** | Sidecar agent, local LLM | Autonomous, runs locally, full DBA scope | Early stage, needs trust building |

### pg_sage Differentiators
1. **Runs locally** -- no data sent to third parties (addresses security concern)
2. **Autonomous agent** -- not just monitoring, can take action with trust ramp
3. **Full DBA scope** -- indexes, vacuum, queries, config, not just one dimension
4. **Sidecar model** -- no SaaS dependency, works on any Postgres deployment

---

## Priority Matrix: What to Build Next

| Pain Point | Frequency | LLM Fit | pg_sage Coverage | Priority |
|-----------|-----------|---------|-----------------|----------|
| Vacuum/Bloat Management | Very High | High | Partial | P0 |
| Query Performance Diagnosis | Very High | Very High | Partial | P0 |
| Index Selection | High | Very High | Good | P1 (expand) |
| Connection Management | High | Medium | Low | P1 |
| Monitoring/Alerting Gaps | High | Very High | Low | P0 |
| Long-Running Transactions | High | High | Low | P1 |
| Schema Migration Safety | High | Medium | None | P2 |
| WAL/Disk Space | Medium-High | High | Low | P1 |
| Replication Lag | Medium-High | Medium | Low | P2 |
| Major Version Upgrades | Medium | Medium | None | P3 |
| Config Tuning | Medium | High | Partial | P2 |

---

## Sources

### Hacker News Discussions
- [AI agent automating PostgreSQL DBA work](https://news.ycombinator.com/item?id=45718151)
- [Xata Agent: AI agent expert in PostgreSQL](https://news.ycombinator.com/item?id=43356039)
- [It's 2026, Just Use Postgres](https://news.ycombinator.com/item?id=46905555)
- [Thoughts on PostgreSQL in 2024](https://news.ycombinator.com/item?id=38848001)

### Production Postmortems
- [Clerk.com Database Incident Sept 2025](https://clerk.com/blog/2025-09-18-database-incident-postmortem)
- [Database Disasters 2024-2025](https://www.canartuc.com/database-disasters-2024-2025-eight-production-failures-and-how-to-survive-them/)
- [Healthchecks.io Database Outage April 2025](https://blog.healthchecks.io/2025/05/post-mortem-database-outage-on-april-30-2025/)
- [PostgreSQL Connection Pool Exhaustion Postmortem](https://medium.com/@ngungabn03/postmortem-database-connection-pool-exhaustion-causing-service-outage-9afd33a45311)

### Technical Deep Dives
- [Diagnosing Critical PostgreSQL Performance Issues](https://dev.to/pedrohgoncalves/diagnosing-and-fixing-critical-postgresql-performance-issues-a-deep-dive-3jj)
- [10 Things I Hate About PostgreSQL](https://rbranson.medium.com/10-things-i-hate-about-postgresql-20dbab8c2791)
- [Features I Wish Postgres Had](https://dev.to/bytebase/features-i-wish-postgres-had-but-mysql-already-has-216b)
- [LLM vs POWA for SQL Optimization](https://medium.com/@devops_63089/llm-vs-powa-optimizing-sql-queries-with-ai-vs-traditional-tools-536fe08b255a)
- [PostgreSQL Alerting That Tells You Why](https://medium.com/@philmcc/postgresql-alerting-that-tells-you-why-not-just-what-4320322c784b)
- [AI Will Eliminate DBA Jobs](https://kendralittle.com/2025/03/02/ai-will-eliminate-dba-jobs-faster-than-you-think/)

### Monitoring & Tools
- [Debugging Postgres Autovacuum: 13 Tips](https://www.citusdata.com/blog/2022/07/28/debugging-postgres-autovacuum-problems-13-tips/)
- [Index Bloat in Postgres](https://kendralittle.com/2025/12/01/index-bloat-postgres-why-it-matters-how-to-identify-and-resolve/)
- [Zero-Downtime Schema Migrations](https://postgres.ai/blog/20210923-zero-downtime-postgres-schema-migrations-lock-timeout-and-retries)
- [Five Reasons WAL Segments Accumulate](https://www.percona.com/blog/five-reasons-why-wal-segments-accumulate-in-the-pg_wal-directory-in-postgresql/)
- [AWS Lock Manager Contention Guide](https://aws.amazon.com/blogs/database/improve-postgresql-performance-diagnose-and-mitigate-lock-manager-contention/)
- [PlanetScale AI-Powered Index Suggestions](https://planetscale.com/blog/postgres-new-index-suggestions)
