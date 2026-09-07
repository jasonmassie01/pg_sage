# Real-World PostgreSQL Schema Incidents & Post-Mortems

> Research for pg_sage v0.10 "Schema Intelligence"
> Compiled: 2026-04-13
> Focus: Documented incidents with verifiable sources

---

## Table of Contents

1. [Migration Lock Incidents](#1-migration-lock-incidents)
2. [Integer Overflow Incidents](#2-integer-overflow-incidents)
3. [Vacuum & Bloat Incidents](#3-vacuum--bloat-incidents)
4. [JSONB Anti-Pattern Incidents](#4-jsonb-anti-pattern-incidents)
5. [Materialized View Incidents](#5-materialized-view-incidents)
6. [Missing Foreign Key Index Incidents](#6-missing-foreign-key-index-incidents)
7. [Query Plan & Statistics Incidents](#7-query-plan--statistics-incidents)
8. [Runaway Query Incidents](#8-runaway-query-incidents)
9. [N+1 Query Problem](#9-n1-query-problem)
10. [Cost of Schema Problems](#10-cost-of-schema-problems)
11. [pg_sage Prevention Matrix](#11-pg_sage-prevention-matrix)

---

## 1. Migration Lock Incidents

### 1.1 Heroku/PGBackups: ALTER TABLE vs Backup Lock Collision

- **Company**: Heroku-hosted application
- **Date**: ~2015
- **Duration**: 25 minutes (8:00-8:25 AM EST)

**What happened**: A routine ALTER TABLE migration to modify a `view_count` column on the `articles` table collided with a scheduled PGBackups process. The pg_dump backup held a shared lock on the articles table. When the ALTER TABLE requested an ACCESS EXCLUSIVE lock at 8:00 AM, it queued behind the backup's lock. All subsequent SELECT queries then queued behind the ALTER TABLE request.

**Root cause**: PostgreSQL's lock queue is FIFO. Once an ACCESS EXCLUSIVE lock request enters the queue, even compatible ACCESS SHARE locks (SELECTs) are blocked behind it. By 8:06 AM, 450 lock requests were queued and the connection pool was exhausted.

**How detected**: Application monitoring showed request timeouts starting at 8:00:19 AM.

**How fixed**: Operator manually canceled the backup job (`heroku pg:backups cancel`), releasing the shared lock and unblocking all queued queries.

**Lessons**: Schedule backups during off-hours. Monitor lock queues during migrations. Maintain pre-opened superuser connections for emergency intervention.

**pg_sage prevention**: Schema Intelligence could detect active backup processes before allowing migrations, warn about lock queue depth, and auto-set `lock_timeout` to prevent indefinite blocking.

**Source**: https://gist.github.com/dwbutler/1034446c1aba231ca8d8639d3be78c6b

---

### 1.2 GoCardless: Foreign Key Lock Cascade (15-Second API Outage)

- **Company**: GoCardless (UK payments platform)
- **Date**: ~2018
- **Duration**: 15 seconds of complete API downtime

**What happened**: During a planned database migration to add a foreign key constraint, an unfortunately timed long-running read query on the parent table collided with the migration. The ALTER TABLE statement itself was fast, but waiting for an AccessExclusive lock on the referenced table caused all queries on both tables to be blocked.

**Root cause**: ADD FOREIGN KEY requires a SHARE ROW EXCLUSIVE lock, blocking writes on both the source and target tables. A long-running analytics query prevented the lock from being acquired, and all new queries queued behind the pending DDL.

**How detected**: API error rate monitoring.

**How fixed**: The long-running query eventually completed, releasing the lock cascade.

**Lessons**: GoCardless built and open-sourced `ActiveRecord::SaferMigrations`, which sets `lock_timeout` (750ms default) and `statement_timeout` (1500ms default) on every migration. After 2 months, the timeouts prevented multiple potential locking incidents.

**pg_sage prevention**: Schema Intelligence could detect long-running queries before migration, recommend `lock_timeout` settings, and verify no active transactions would conflict with DDL operations.

**Sources**:
- https://gocardless.com/blog/zero-downtime-postgres-migrations-the-hard-parts/
- https://gocardless.com/blog/zero-downtime-postgres-migrations-a-little-help/

---

### 1.3 Doctolib: Column Addition on Small Table Caused Platform Outage

- **Company**: Doctolib (European healthcare platform)
- **Date**: ~2018
- **Duration**: ~1 minute of complete platform downtime

**What happened**: Engineers added a column without constraints to a relatively small table (less than 100,000 rows). The operation appeared safe but caused the entire platform to go down.

**Root cause**: Even though the DDL operation was fast (milliseconds), at 15,000+ requests per second, the brief ACCESS EXCLUSIVE lock caused a cascade of queued requests that overwhelmed the system. The lock blocks queries even while waiting to be acquired.

**How detected**: Platform-wide monitoring.

**How fixed**: The migration completed, releasing the lock. Doctolib then invested heavily in migration safety tooling.

**Lessons**: Doctolib executes 500+ migrations per year and built the open-source `safe-pg-migrations` Ruby gem that automatically sets lock timeouts (5-second default), uses concurrent index creation, and implements retry mechanisms.

**pg_sage prevention**: Schema Intelligence could calculate the request rate against a table and predict whether even a millisecond-level lock would cause a cascading queue at production traffic levels.

**Source**: https://medium.com/doctolib/stop-worrying-about-postgresql-locks-in-your-rails-migrations-3426027e9cc9

---

### 1.4 GitLab: Repeated Migration Lock Failures

- **Company**: GitLab
- **Date**: 2019-2021 (multiple incidents)
- **Duration**: Variable; deployment stuck for hours in Feb 2021

**What happened**: GitLab.com has short `statement_timeout` settings for stability. When migrations attempted to acquire ACCESS EXCLUSIVE locks on busy tables, they'd sit in the lock queue blocking other queries until the timeout fired, then fail with `PG::LockNotAvailable`. A February 2021 incident saw a canary deployment stuck due to this exact pattern. Additionally, their `with_lock_retries` implementation leaked short `lock_timeout` values (e.g., 100ms) causing transient failures.

**Root cause**: High-traffic tables made it nearly impossible to acquire exclusive locks within timeout windows. The retry mechanism itself had bugs.

**How detected**: Failed deployments; migration timeout errors in logs.

**How fixed**: GitLab built a retry mechanism with exponential backoff and different `lock_timeout` settings per attempt. They documented extensive migration safety guidelines.

**Lessons**: GitLab now maintains one of the most comprehensive PostgreSQL migration safety guides in the industry, covering every dangerous DDL pattern.

**pg_sage prevention**: Schema Intelligence could implement smart migration scheduling, detecting low-traffic windows and automatically applying appropriate lock_timeout + retry strategies.

**Sources**:
- https://docs.gitlab.com/development/database/avoiding_downtime_in_migrations/
- https://gitlab.com/gitlab-com/gl-infra/production/-/issues/3487

---

### 1.5 Preply: Schema Change Caused Production Downtime

- **Company**: Preply (online tutoring platform)
- **Date**: Not specified

**What happened**: A software engineer made one of six common PostgreSQL migration mistakes, causing production to go down. The specific mistake was not disclosed, but the post covers all six dangerous patterns: adding columns with defaults (pre-PG11), adding columns to busy tables, changing column types, creating indexes without CONCURRENTLY, adding CHECK constraints, and adding NOT NULL constraints.

**Root cause**: One of the six patterns above, each of which acquires long-held exclusive locks.

**pg_sage prevention**: Schema Intelligence could classify every DDL statement against these six known-dangerous patterns and either block or suggest safe alternatives.

**Source**: https://medium.com/preply-engineering/postgresql-schema-change-gotchas-bf904e2d5bb7

---

## 2. Integer Overflow Incidents

### 2.1 Basecamp 3: Integer Limit Hit on Events Table (~5-Hour Outage)

- **Company**: Basecamp / 37signals
- **Date**: November 2018
- **Duration**: Nearly 5 hours of read-only mode

**What happened**: Basecamp 3's database hit the 2,147,483,647 ceiling on a very busy `events` table. The database went into read-only mode, preventing all writes to the application.

**Root cause**: The `events` table column was configured as a 4-byte INTEGER rather than BIGINT. Ruby on Rails 5.1 (released 2017) had updated defaults to use BIGINT, but 37signals didn't migrate existing hosted Basecamp databases because they thought they had more time.

**How detected**: Application write failures; database rejecting INSERTs.

**How fixed**: Emergency migration of the column from INT to BIGINT while the service was degraded.

**Lessons**: Monitor sequence utilization proactively. The maximum INT value (2.1 billion) sounds large but can be reached surprisingly fast on high-write tables. DHH and team publicly acknowledged the oversight.

**pg_sage prevention**: Schema Intelligence could monitor sequence utilization percentages and alert at 60%, 75%, 85% thresholds. It could also scan all INT primary keys on table creation and recommend BIGINT for high-volume tables.

**Sources**:
- https://news.ycombinator.com/item?id=18432633
- https://www.packtpub.com/en-at/learning/tech-news/basecamp-3-faces-a-read-only-outage-of-nearly-5-hours

---

### 2.2 Buildkite: Proactive 2-Billion-Row Migration

- **Company**: Buildkite (CI/CD platform)
- **Date**: 2023-2024

**What happened**: Buildkite discovered via PgHero monitoring that one of their largest PostgreSQL tables (2+ billion rows, 8.5TB across three partitions) was approaching integer overflow limits. They executed a zero-downtime migration from INT to BIGINT.

**Root cause**: Original table design used default INTEGER primary keys that were now approaching the 2.1 billion limit.

**Approach**: Three-phase zero-downtime strategy:
1. Added `id_bigint` column with trigger to dual-write
2. Backfilled 2B+ rows using distributed batch updates with load monitoring
3. Atomic swap: renamed columns, transferred sequence ownership, replaced primary key constraint

Key technique: Used `NOT VALID` CHECK constraint + separate VALIDATE step to avoid full table scans under exclusive lock.

**Lessons**: A naive `ALTER TABLE ... ALTER COLUMN id TYPE BIGINT` would have required "downtime in the days to weeks range" for a table of this size. The zero-downtime approach required significant engineering investment.

**pg_sage prevention**: Schema Intelligence could identify tables approaching INT limits months before they become emergencies, and generate the multi-step migration plan automatically.

**Source**: https://buildkite.com/blog/avoiding-integer-overflows-with-zero-downtime

---

### 2.3 Zemanta: INT to BIGINT Migration Under Pressure

- **Company**: Zemanta (advertising technology)
- **Date**: 2021

**What happened**: Multiple high-volume tables were "getting dangerously close" to the INT limit. A standard `ALTER TABLE ... ALTER COLUMN ... TYPE BIGINT` would rewrite the entire table under ACCESS EXCLUSIVE lock, causing "hours" of downtime.

**Approach**: Zero-downtime strategy with 6 phases: add BIGINT column, create trigger for dual-write, chunked backfill in small batches, concurrent index creation with `CREATE INDEX CONCURRENTLY`, `NOT VALID` constraint + separate validation, atomic column swap in a single transaction.

**Lessons**: The approach was reliable enough that Zemanta automated it into a reusable script. Critical: verified the INT column was not a foreign key target before starting (FK dependencies complicate the migration significantly).

**pg_sage prevention**: Schema Intelligence could detect approaching INT limits, check for FK dependencies, and generate the appropriate multi-phase migration script.

**Source**: https://zemanta.github.io/2021/08/25/column-migration-from-int-to-bigint-in-postgresql/

---

### 2.4 Industry-Wide: Crunchy Data Client Incidents

- **Company**: Multiple Crunchy Data clients (unnamed)
- **Date**: Ongoing

**What happened**: Crunchy Data reports helping "a few clients navigate" integer overflow recently. They provide a diagnostic SQL query that calculates "percent until the sequence value exceeds the sequence or column data type," with examples showing sequences at 93% capacity.

**Short-term fix**: Convert sequences to negative number sequencing (start at -1, increment by -1) to buy time while planning the full BIGINT migration.

**pg_sage prevention**: Schema Intelligence could run the diagnostic query on a schedule and project time-to-overflow based on insertion rate.

**Source**: https://www.crunchydata.com/blog/the-integer-at-the-end-of-the-universe-integer-overflow-in-postgres

---

## 3. Vacuum & Bloat Incidents

### 3.1 Duffel: Anti-Wraparound Vacuum Caused 2+ Hour API Outage

- **Company**: Duffel (flight search API)
- **Date**: November 22, 2021
- **Duration**: 2 hours 17 minutes (22:02-00:20 GMT)

**What happened**: The Duffel flight-search API went completely down. All search requests and bookings failed for over two hours.

**Root cause**: A multi-layer lock contention cascade:
1. The `search_results` table's transaction ID age approached 200 million (PostgreSQL's `autovacuum_freeze_max_age` threshold), triggering an **uninterruptible** anti-wraparound vacuum holding a SHARE UPDATE EXCLUSIVE lock.
2. Partition creation jobs issued `CREATE TABLE` requiring ACCESS EXCLUSIVE locks, which queued behind autovacuum.
3. INSERT and UPDATE operations queued behind the DDL statements, creating complete service unavailability.

**Critical detail**: Autovacuum is normally interruptible, but anti-wraparound vacuum is NOT automatically interrupted. This is a PostgreSQL behavior that surprises many engineers.

**How detected**: API error monitoring. Engineers traced the issue through PostgreSQL logs showing processes waiting for RowExclusiveLock and ShareRowExclusiveLock. They proved the anti-wraparound hypothesis by restoring pre-incident backups and calculating the table's TXID aging rate.

**How fixed**: Database restart terminated the autovacuum process. Permanent fix: apply `lock_timeout` and `statement_timeout` to partition creation DDL statements.

**Lessons**: The team estimated they had 30 days before recurrence. DDL operations (even partition creation) must have timeouts in production.

**pg_sage prevention**: Schema Intelligence could monitor `age(relfrozenxid)` on all tables, alert when approaching anti-wraparound thresholds, and ensure DDL operations always have timeouts configured.

**Source**: https://duffel.com/blog/understanding-outage-concurrency-vacuum-postgresql

---

### 3.2 Compass: 7:1 Bloat Ratio on High-Traffic Table

- **Company**: Compass (real estate technology)
- **Date**: Not specified

**What happened**: A code issue caused excessive database bloat in a high-traffic table. One table had 350 million dead tuples against only 50 million active rows -- a 7:1 bloat ratio. Tables were in the "10s to 100s of GB" range.

**Root cause**: Application code was updating rows in a high read/write traffic table "much more often than it should have been," generating dead tuples faster than autovacuum could clean them. The excessive bloat caused PostgreSQL's query planner to make poor decisions (e.g., selecting inferior indexes).

**Impact**: One query fingerprint took upwards of 14 seconds to execute.

**How fixed**: Used `pg_repack` to rebuild the table online (create clean duplicate, trigger-based sync, atomic rename). Temporarily dropped replication slots (used by Fivetran for Redshift sync) to prevent OOM from WAL changes. After cleanup, the problematic query executed in 37ms -- a **99.7% improvement**.

**Lessons**: Fix the root cause (excessive updates), not just the symptom. `pg_repack` is preferred over `VACUUM FULL` because it doesn't require an ACCESS EXCLUSIVE lock for the entire duration.

**pg_sage prevention**: Schema Intelligence could monitor dead tuple ratios, alert when bloat exceeds thresholds (e.g., 3:1), identify tables with excessive UPDATE frequency, and recommend `pg_repack` over `VACUUM FULL`.

**Source**: https://medium.com/compass-true-north/dealing-with-significant-postgres-database-bloat-what-are-your-options-a6c1814a03a5

---

### 3.3 Industry: Autovacuum Falling Behind on Cloud SQL

- **Company**: Multiple organizations on Google Cloud SQL
- **Date**: Ongoing (documented Feb 2026)

**What happened**: Multiple Cloud SQL PostgreSQL instances experienced stuck vacuum processes and escalating table bloat. In highly transactional production databases, dead tuples from UPDATE and DELETE operations remained uncollected.

**Root cause**: Default autovacuum settings are insufficient for high-write workloads. Long-running transactions block vacuum from reclaiming space. On managed services, operators often lack the tuning control they need.

**Lessons**: Autovacuum is NOT "set and forget." It requires continuous monitoring and tuning of `autovacuum_vacuum_cost_limit` and related parameters.

**pg_sage prevention**: Schema Intelligence could continuously monitor `n_dead_tup` per table, compare autovacuum throughput vs dead tuple generation rate, and recommend configuration changes before bloat becomes critical.

**Source**: https://oneuptime.com/blog/post/2026-02-17-how-to-debug-cloud-sql-postgresql-vacuum-process-stuck-and-table-bloat-issues/view

---

## 4. JSONB Anti-Pattern Incidents

### 4.1 Heap: JSONB Query Planner Disaster (2000x Slowdown)

- **Company**: Heap (analytics platform)
- **Date**: ~2016

**What happened**: Heap experienced severe production performance issues from using JSONB to store table data. Analytical queries that joined JSONB columns with other tables became catastrophically slow.

**Root cause**: PostgreSQL cannot maintain statistics on fields within JSONB columns. It relies on a hardcoded estimate of 0.1% selectivity for JSONB field access. This caused the query planner to choose nested loop joins instead of hash joins. Benchmark: a query that should complete in ~300ms took **584 seconds -- approximately 2,000x slower**.

**How detected**: Query performance monitoring; specific queries timing out.

**Emergency fix**: Disabled nested loops entirely with the global setting `enable_nestloop = off` -- described by Heap themselves as something "ordinarily, you should never do."

**Permanent fix**: Pulled 45 commonly queried fields out of JSONB into first-class columns. This also yielded ~30% disk space savings from deduplicating previously repeated JSONB keys across every row.

**pg_sage prevention**: Schema Intelligence could detect JSONB columns being used in JOINs, warn about missing statistics, identify candidate fields for promotion to first-class columns based on query patterns, and flag tables where JSONB query selectivity estimates may be wildly inaccurate.

**Source**: https://www.heap.io/blog/when-to-avoid-jsonb-in-a-postgresql-schema

---

## 5. Materialized View Incidents

### 5.1 Production App: Trigger-Based Refresh Cascade (10-Minute Insert Latency)

- **Company**: Unnamed (documented by engineer)
- **Date**: Not specified

**What happened**: Insert and update operations on source tables started taking 90 seconds to 10 minutes, triggering a war room response as users couldn't update website data.

**Root cause**: A trigger was configured to refresh a materialized view (joining five tables) on every insert/update to source tables. This caused:
1. Continuous refreshes repopulating the entire matview on every write
2. Dead tuples accumulated at 100x the rate of actual rows
3. Auto vacuum couldn't complete because refresh locks blocked it
4. Atomic transactions meant that "when 1000 records are inserted, the 1001st record has to wait till 1000 refreshes are complete"

**How detected**: User complaints about extremely slow data entry.

**How fixed**: Full vacuum to reclaim space, removed insert/update triggers, switched to cron-based scheduled refresh (every 2 minutes), eventually replaced materialized views with regular views where real-time data was needed.

**Lessons**: Never use INSERT/UPDATE triggers to refresh materialized views. Use scheduled refresh with CONCURRENTLY. Ensure matview has a UNIQUE index to enable `REFRESH MATERIALIZED VIEW CONCURRENTLY`.

**pg_sage prevention**: Schema Intelligence could detect materialized views with trigger-based refresh, warn about refresh-without-CONCURRENTLY, monitor dead tuple buildup on matviews, and recommend switching to scheduled refresh.

**Source**: https://kishore-rjkmr.medium.com/my-experience-with-postgres-materialized-view-36d9f3407c87

---

## 6. Missing Foreign Key Index Incidents

### 6.1 Production App: 30-Minute DELETE Reduced to 591ms

- **Company**: Unnamed (documented by developer)
- **Date**: Not specified

**What happened**: Deleting ~50,000 records from a `roles` table took approximately 30 minutes, while deleting the same count from `users_roles` completed in ~100ms -- a 10,000x difference.

**Root cause**: The `users_roles` join table had a foreign key constraint on `role_id` referencing `roles.id`, but no index on `users_roles.role_id`. PostgreSQL had to sequentially scan all 500,000 rows in `users_roles` for every FK constraint check during the parent row deletion.

**Dataset**: 50,000 users, 100,000 roles, 500,000 user-role associations.

**How fixed**:
- Adding B-tree index on `users_roles.role_id`: delete time dropped to **591ms** (~3,000x faster)
- Adding `INITIALLY DEFERRED` constraint: further reduced to **68ms**

**Lessons**: PostgreSQL does NOT automatically create indexes on foreign key columns in the referencing table. This is one of the most common PostgreSQL performance gotchas.

**pg_sage prevention**: Schema Intelligence could scan all foreign key constraints, identify those without corresponding indexes on the referencing column, and flag them as high-priority recommendations. This is a straightforward detection that prevents a 3,000x performance cliff.

**Source**: https://dev.to/jbranchaud/beware-the-missing-foreign-key-index-a-postgres-performance-gotcha-3d5i

---

## 7. Query Plan & Statistics Incidents

### 7.1 Figma: Query Plan Regression After Statistics Change

- **Company**: Figma
- **Date**: January 21-22, 2020

**What happened**: A routine change in database statistics caused PostgreSQL 9 to mis-plan query execution, leading to expensive table scans and writes to temporary buffers. Concurrent aggressive autovacuuming operations exacerbated the problem.

**Root cause**: PostgreSQL 9's query planner made a bad plan based on changed statistics. The combination of the bad plan + autovacuum activity overwhelmed the database.

**How fixed**: Upgrading to PostgreSQL 11, which had improvements to both the query planner (eliminating the bad plan possibility) and autovacuuming performance.

**pg_sage prevention**: Schema Intelligence could detect query plan regressions by monitoring `pg_stat_statements` for sudden execution time changes, and recommend ANALYZE or plan hints when planner statistics appear stale.

**Source**: https://www.figma.com/blog/post-mortem-service-disruption-on-january-21-22-2020/

---

## 8. Runaway Query Incidents

### 8.1 Render: psql Ctrl+C Didn't Actually Cancel the Query

- **Company**: Render
- **Date**: Not specified

**What happened**: Staging deployments were blocked for hours because end-to-end tests couldn't run. Memory dropped to near 0% free, CPU doubled. A database migration step hung indefinitely.

**Root cause**: An engineer ran an ad-hoc validation query via psql:
```sql
SELECT e.id
FROM events e
JOIN postgres_dbs db ON (e.data ->> 'serviceId') = db.database_id
LIMIT 1;
```
When it took too long, they pressed Ctrl+C. But **canceling in psql only cancels reading the result -- the backend process continues executing**. The query ran for 4+ hours, holding locks that prevented `CREATE OR REPLACE VIEW` statements in the migration pipeline.

**How detected**: `pg_stat_activity` showed a process running for "04:12:58.744124" with `DataFileRead` wait events. `pg_blocking_pids()` confirmed it was blocking migration DDL.

**How fixed**: `pg_terminate_backend(311124)` -- immediately unblocked migrations and normalized all metrics.

**Lessons**: Ctrl+C in psql is not a reliable cancellation. Long-running transactions block autovacuum and schema changes. Always use `statement_timeout` for ad-hoc queries.

**pg_sage prevention**: Schema Intelligence could monitor `pg_stat_activity` for long-running queries, auto-detect queries blocking DDL operations, and alert on queries exceeding configurable thresholds.

**Source**: https://render.com/blog/postgresql-simple-query-big-problem

---

## 9. N+1 Query Problem

### 9.1 Industry-Wide: The Silent Performance Killer

While N+1 queries rarely cause a single dramatic outage post-mortem (they're a slow burn rather than a sudden crash), they are one of the most pervasive database performance problems:

**Scale impact** (documented benchmarks):
- 800 items across 17 categories: 18 queries taking 1+ second vs 1 JOIN query 10x faster
- At 100 queries: noticeable latency (~100ms page loads become 1 second)
- At 1,000 queries: user-facing performance degradation
- At 10,000+ queries: potential for connection pool exhaustion and cascading failures

**Detection ecosystem**: Sentry now has built-in N+1 detection that automatically identifies sequential, non-overlapping database spans with similar descriptions. It also detects MN+1 patterns (repeating sets of N queries interspersed with other work).

**pg_sage prevention**: Schema Intelligence could analyze `pg_stat_statements` for patterns of similar queries with incrementing parameter values, correlate with application traces, and recommend batch queries or eager loading.

**Sources**:
- https://docs.sentry.io/product/issues/issue-details/performance-issues/n-one-queries/
- https://www.scoutapm.com/blog/understanding-n1-database-queries/

---

## 10. Cost of Schema Problems

### 10.1 Documented Financial Impact

| Metric | Value | Source |
|--------|-------|--------|
| Mid-size enterprise hourly downtime cost | >$300,000 (90% of companies) | Industry surveys |
| Enterprise hourly downtime cost | $1M-$5M (48% of companies) | Industry surveys |
| Unplanned downtime per minute | >$5,000 | 2025 industry data |
| Series C startup Postgres migration failure | $2.4M revenue lost | Migration cost analyses |
| E-commerce 1-hour downtime ($12M daily) | $840K | Zero-downtime migration ROI |
| 6-day outage from failed migration | $8.4M total cost ($3.2M revenue) | Migration horror stories |
| Fortune 1000 hourly downtime | ~$1M/hour | Industry benchmarks |

### 10.2 Developer Time Costs

| Task | Time Estimate |
|------|--------------|
| Testing & validation of migrations | 30-50% of total project time |
| <100GB simple schema migration | Hours to days |
| 100GB-1TB moderate complexity | Days to weeks |
| 100M+ rows migration (one engineer) | 1-2 weeks |
| 500M+ rows migration | Full project; data transfer alone takes days |

### 10.3 Cloud Cost from Schema Anti-Patterns

Missing indexes force sequential scans, which means:
- More CPU per query = larger instance required
- More I/O = higher IOPS bills
- Longer query times = more connections held = connection pool exhaustion = even larger instance

The Heap JSONB incident (Section 4.1) is a perfect example: a 2,000x query slowdown means 2,000x more compute per query.

The Compass bloat incident (Section 3.2) shows how 7:1 bloat ratios waste 85%+ of storage costs.

**Sources**:
- https://www.ispirer.com/blog/real-cost-of-database-migration
- https://red9.com/blog/enterprise-database-downtime-cost-disaster-recovery/

---

## 11. pg_sage Prevention Matrix

How pg_sage v0.10 Schema Intelligence maps to these real incidents:

| Incident Type | Detection Method | pg_sage Feature |
|---|---|---|
| **Migration lock cascade** | Check `pg_stat_activity` for active queries on target table before DDL | Pre-migration safety check |
| **Lock queue buildup** | Monitor `pg_locks` queue depth during migrations | Real-time migration monitoring |
| **Integer overflow** | Query `pg_sequences` utilization % | Proactive sequence monitoring |
| **Vacuum/bloat failure** | Track `n_dead_tup` / `n_live_tup` ratio | Bloat detection & alerting |
| **Anti-wraparound risk** | Monitor `age(relfrozenxid)` | TXID age monitoring |
| **JSONB statistics gap** | Detect JSONB columns in JOIN/WHERE clauses | Schema anti-pattern detection |
| **Missing FK indexes** | Cross-reference `pg_constraint` with `pg_indexes` | Automated FK index audit |
| **Matview refresh blocking** | Detect REFRESH without CONCURRENTLY, trigger-based refresh | Matview health monitoring |
| **Runaway queries** | Monitor `pg_stat_activity` duration | Long-query detection |
| **N+1 patterns** | Analyze `pg_stat_statements` for similar query clusters | Query pattern analysis |
| **Stale statistics** | Track `last_analyze` age vs table change rate | Statistics freshness monitoring |

### Priority Rankings (by frequency x severity)

1. **Missing FK indexes** -- Extremely common, easy to detect, huge impact (3,000x perf cliff)
2. **Migration lock cascades** -- Multiple companies report incidents; preventable with pre-checks
3. **Integer overflow** -- Catastrophic when it hits; trivial to monitor
4. **Vacuum/bloat failures** -- Slow burn but devastating; requires continuous monitoring
5. **JSONB statistics gap** -- Hard to detect without schema awareness; 2,000x slowdown possible
6. **Anti-wraparound vacuum** -- Rare but causes complete outage; must monitor TXID age
7. **Matview refresh blocking** -- Common in OLAP workloads; detectable via pg_locks
8. **Runaway queries blocking DDL** -- Requires active monitoring of pg_stat_activity
9. **N+1 patterns** -- Pervasive but harder to detect from database side alone
10. **Stale statistics** -- Causes plan regressions; trackable via pg_stat_user_tables

---

## Summary: Tools Built in Response to These Incidents

Companies that suffered these incidents built specific tools:

| Company | Tool | What It Prevents |
|---------|------|-----------------|
| GoCardless | `ActiveRecord::SaferMigrations` | Lock timeout on all migrations |
| Doctolib | `safe-pg-migrations` | Automatic safe DDL patterns |
| GitLab | Custom lock retry framework | Migration lock failures |
| Braintree | `pg_ha_migrations` | DDL/migration safety enforcement |
| Sentry | N+1 query auto-detection | Performance issue detection |

**pg_sage v0.10 opportunity**: Consolidate all of these protections into a single autonomous agent that works at the PostgreSQL level, regardless of application framework. Every tool above is framework-specific (Rails, etc.). pg_sage can provide universal protection by operating directly on the database.

---

## Key Takeaways for v0.10 Design

1. **Pre-migration safety checks are the #1 feature request the market is screaming for.** Every company that had a migration incident built their own tool afterward. pg_sage can provide this universally.

2. **Sequence monitoring is embarrassingly simple but prevents catastrophic outages.** Basecamp's 5-hour outage from INT overflow is 100% preventable with a single query.

3. **Bloat detection must be continuous, not periodic.** The Duffel incident shows that anti-wraparound vacuum triggers can't be predicted from daily checks alone.

4. **JSONB schema analysis is a differentiated feature.** No existing tool warns about JSONB statistics blindness. pg_sage can be the first.

5. **Missing FK index detection is the highest-ROI feature.** It's a simple catalog query that prevents 3,000x performance cliffs, and virtually every production PostgreSQL database has at least one missing FK index.
