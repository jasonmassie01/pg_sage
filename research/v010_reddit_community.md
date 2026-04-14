# v0.10 "Schema Intelligence" - Community Pain Point Research

> Research date: 2026-04-13
> Sources: Reddit (r/postgresql, r/database, r/devops, r/sre, r/django, r/rails, r/node),
> Hacker News, Medium, dev.to, GitHub Issues, PostgreSQL mailing lists, company blogs

---

## Table of Contents

1. [Safe Migration Planning (DDL Locking Disasters)](#1-safe-migration-planning-ddl-locking-disasters)
2. [Schema Anti-Pattern Detection](#2-schema-anti-pattern-detection)
3. [N+1 Query Detection at Database Level](#3-n1-query-detection-at-database-level)
4. [Materialized View Recommendations](#4-materialized-view-recommendations)
5. [Migration Tool Complaints](#5-migration-tool-complaints)
6. [Real Postmortems and Incidents](#6-real-postmortems-and-incidents)
7. [Product Opportunity Summary](#7-product-opportunity-summary)

---

## 1. Safe Migration Planning (DDL Locking Disasters)

### Pain Point 1.1: ALTER TABLE + AccessExclusiveLock = Total Outage

**The Problem:** Most ALTER TABLE operations acquire ACCESS EXCLUSIVE locks, the
strongest lock level in PostgreSQL. This blocks everything -- SELECT, INSERT,
UPDATE, DELETE, even other DDL. While the lock is held, the table is completely
unavailable.

**How Common:** This is the #1 self-inflicted outage cause in PostgreSQL. Every
company blog, HN thread, and Reddit discussion about PostgreSQL downtime
references this. Multiple detailed postmortems exist (see Section 6).

**Community Quotes:**
- "If you do that on a table or two with millions of rows, you've effectively
  locked your database from all other access, even SELECTs." (HN user ttfkam)
- "A 3-second ALTER TABLE ADD COLUMN took down an application for 45 minutes"
  (multiple sources report this pattern)

**Existing Tools:**
- [Squawk](https://squawkhq.com/) - SQL linter for migrations, catches unsafe
  patterns in CI
- [strong_migrations](https://github.com/ankane/strong_migrations) - Ruby gem,
  catches unsafe operations at migration time
- [safe-pg-migrations](https://github.com/doctolib/safe-pg-migrations) -
  Doctolib's Rails safety wrapper
- [pgroll](https://github.com/xataio/pgroll) - Zero-downtime expand/contract
  migrations
- [Reshape](https://github.com/fabianlindfors/reshape) - Similar
  expand/contract approach

**What's Missing (pg_sage Opportunity):**
- None of these tools operate at the database level as a sidecar
- No tool proactively warns BEFORE a migration runs based on table size, active
  queries, and lock conflicts
- No tool correlates pg_stat_activity with incoming DDL to predict lock queue
  cascades
- No tool provides real-time "migration safety score" factoring in table size,
  active connections, replication lag, and current lock state

**Ideal Solution:**
A database-side agent that intercepts or advises on DDL statements by:
1. Checking current lock state and active queries on target tables
2. Estimating operation duration based on table size and operation type
3. Recommending safe alternatives (e.g., NOT VALID + VALIDATE pattern)
4. Setting appropriate lock_timeout automatically
5. Providing a "safe migration window" recommendation

---

### Pain Point 1.2: The Lock Queue Cascade

**The Problem:** PostgreSQL's lock queue is FIFO. When a DDL statement waits for
ACCESS EXCLUSIVE lock (because a long-running query holds a conflicting lock),
ALL subsequent queries queue behind the waiting DDL -- even simple SELECTs. The
total outage duration is: blocking query time + DDL time + queue drain time.

**How Common:** Extremely common. GoCardless published a detailed blog post about
a 15-second outage from exactly this pattern. The Xata blog calls this "still a
PITA." Every migration guide warns about it.

**Community Quotes:**
- "When a lock can't be acquired because of a lock held by another transaction,
  it goes into a queue. Any locks that conflict with the queued lock will queue
  up behind it." (GoCardless engineering blog)
- "The total outage duration is not the time the DDL takes. It's the time the
  blocking query was already running plus the time the DDL takes plus the time
  to drain the queued requests." (Xata blog)

**Existing Tools:**
- lock_timeout setting (manual, often forgotten)
- Squawk can warn about missing lock_timeout in SQL files

**What's Missing:**
- No tool automatically monitors for lock queue buildup in real-time
- No tool can preemptively kill the blocking query or suggest optimal timing
- No tool provides a "lock queue depth" metric that could trigger alerts

**Ideal Solution:**
pg_sage could monitor pg_locks and pg_stat_activity to detect lock queue
cascades forming, alert immediately, and recommend or take action (kill the
blocking query, set lock_timeout, retry the DDL).

---

### Pain Point 1.3: CREATE INDEX Without CONCURRENTLY

**The Problem:** Standard CREATE INDEX acquires a SHARE lock, blocking all writes
until completion. On large tables (100M+ rows), this can take minutes. Migration
frameworks often don't default to CONCURRENTLY.

**Compounding Problem:** CREATE INDEX CONCURRENTLY can fail partway through,
leaving an INVALID index that still consumes write overhead. Using IF NOT EXISTS
then silently skips the invalid index instead of fixing it.

**How Common:** Very common, especially with ORM-generated migrations. Django
required a [ticket (#21039)](https://code.djangoproject.com/ticket/21039) and
custom AddIndexConcurrently operation. Rails requires explicit
`disable_ddl_transaction!`.

**Existing Tools:**
- Squawk catches non-concurrent index creation
- strong_migrations warns in Rails

**What's Missing:**
- No tool detects orphaned INVALID indexes from failed concurrent builds
- No tool recommends REINDEX CONCURRENTLY for invalid indexes
- No tool verifies index creation actually succeeded

**Ideal Solution:**
pg_sage should:
1. Detect INVALID indexes via pg_index.indisvalid
2. Recommend cleanup or rebuild
3. Warn when non-concurrent index creation is attempted on tables > N rows

---

### Pain Point 1.4: SET NOT NULL Without the CHECK Trick

**The Problem:** ALTER TABLE SET NOT NULL requires ACCESS EXCLUSIVE lock while
scanning the entire table to verify no NULLs exist. On large tables, this is a
long blocking operation.

**The Safe Alternative (PostgreSQL 12+):**
1. ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID (fast, metadata only)
2. VALIDATE CONSTRAINT ... (slower but holds weaker SHARE UPDATE EXCLUSIVE lock)
3. SET NOT NULL (now metadata-only since PG recognizes the validated CHECK)
4. DROP the redundant CHECK constraint

**How Common:** Extremely common mistake. The 4-hour postmortem incident (Section
6) was caused by exactly this pattern -- adding a CHECK constraint that
validated 84M rows.

**Existing Tools:**
- Squawk warns about unsafe SET NOT NULL
- The pattern is documented but not automated by any tool

**What's Missing:**
- No tool automatically recommends the NOT VALID + VALIDATE pattern
- No tool calculates expected lock duration based on table size

---

### Pain Point 1.5: Foreign Key Constraints Lock BOTH Tables

**The Problem:** ALTER TABLE ADD CONSTRAINT ... FOREIGN KEY acquires locks on
BOTH the referencing table AND the referenced table while scanning for
constraint validity. This means modifying a small child table can lock a
heavily-queried parent table.

**How Common:** GoCardless documented a 15-second production outage from this
exact pattern. The "empty tables are safe" assumption fails because the
referenced parent table gets locked too.

**Safe Alternative:**
1. ADD CONSTRAINT ... NOT VALID (fast, metadata only)
2. VALIDATE CONSTRAINT (holds weaker lock, allows reads/writes)

**What's Missing:**
- No tool warns that FK constraints lock the referenced table
- No tool checks if the referenced table is under heavy query load before
  allowing the constraint

---

### Pain Point 1.6: Integer to BigInt Migration Nightmare

**The Problem:** When an int4 primary key approaches 2^31 (2.1 billion), teams
need to convert to bigint. ALTER COLUMN TYPE requires a full table rewrite with
ACCESS EXCLUSIVE lock. Foreign keys cascade this lock to ALL dependent tables.

**How Common:** Multiple production incidents documented. Cleo documented
migrating a 1.7 billion row table with 10+ interconnected tables ranging from
100MB to 500GB. Rails changed its default PK type to bigint in
[PR #26266](https://github.com/rails/rails/pull/26266) because this was so
painful.

**Existing Tools:** No tool solves this automatically.

**What's Missing:**
- No tool warns when integer sequences approach overflow
- No tool recommends bigint at table creation time
- No tool provides a safe migration path for existing int4 PKs

**Ideal Solution:**
pg_sage should monitor sequences and warn when they approach 50% of int4 max,
and recommend identity columns with bigint for all new tables.

---

### Pain Point 1.7: Enum Type Migration Footguns

**The Problem:** ALTER TYPE ... ADD VALUE cannot be used inside a transaction
block in a way that allows using the new value in the same transaction.
Migration frameworks that wrap statements in transactions cause "unsafe use of
new value" errors. Enum values also cannot be removed once created.

**How Common:** Major issue for Prisma (issue #5290), TypeORM (issue #7217),
Payload CMS (issue #15071), and Alembic users. Multiple GitHub issues across
ORM ecosystems.

**What's Missing:**
- No tool warns about enum limitations before they bite
- No tool recommends CHECK constraints over enums for mutable value lists

---

## 2. Schema Anti-Pattern Detection

### Pain Point 2.1: Missing Indexes on Foreign Key Columns

**The Problem:** PostgreSQL does NOT automatically create indexes on foreign key
columns. This means DELETE/UPDATE on parent rows triggers sequential scans on
child tables for constraint enforcement. Also, any JOIN using the FK column
requires a full table scan.

**How Common:** The single most common PostgreSQL performance surprise. Every
PostgreSQL performance guide mentions it. Cybertec, Percona, Backstage (PayFit),
Arktekk, and dozens of blog posts cover this. One developer reported a delete
query going from 100ms to 30 minutes due to a missing FK index.

**Existing Tools:**
- Manual queries against pg_catalog can find missing FK indexes
- pganalyze detects this
- Various one-off scripts exist on GitHub

**What's Missing:**
- No tool continuously monitors for new FKs added without indexes
- No tool estimates the performance impact of missing FK indexes based on table
  sizes and query patterns

**Ideal Solution:**
pg_sage should automatically detect FK columns without indexes by joining
pg_constraint with pg_index, estimate impact based on parent/child table sizes,
and recommend CREATE INDEX CONCURRENTLY with specific DDL.

---

### Pain Point 2.2: UUIDv4 Primary Keys

**The Problem:** Random UUIDv4 values scatter inserts across B-tree index pages,
causing excessive page splits, random I/O, and index bloat. The index must be
16 bytes vs 8 bytes for bigint.

**How Common:** Very common in microservice architectures. Multiple HN threads
(item #40884878, #27345837) debate this extensively. The PostgreSQL wiki "Don't
Do This" page doesn't list it, but expert consensus is strong.

**Existing Tools:** No automated detection.

**What's Missing:**
- No tool detects UUIDv4 usage and recommends UUIDv7 or bigint alternatives
- No tool measures the actual I/O impact of random PK distribution

**Ideal Solution:**
pg_sage should detect UUID PK columns, check if they're random (v4) vs
time-ordered (v7), and recommend alternatives with estimated I/O savings.

---

### Pain Point 2.3: Using serial Instead of identity

**The Problem:** The serial pseudo-type has awkward permission, dependency, and
schema behavior. The PostgreSQL wiki explicitly says "Don't use serial."
Identity columns (GENERATED ALWAYS AS IDENTITY) are the modern replacement.

**How Common:** Extremely widespread in legacy schemas. Every PostgreSQL tutorial
before ~2018 taught serial. Rails still defaults to serial in many
configurations.

**Existing Tools:** No automated detection.

**What's Missing:**
- No tool scans for serial columns and recommends identity migration
- No tool warns about serial permission issues

---

### Pain Point 2.4: Wrong Timestamp Types

**The Problem:** Using `timestamp` (without time zone) instead of `timestamptz`
stores "wall clock pictures" rather than actual points in time. The PostgreSQL
wiki explicitly warns against this. Also, `timestamp(0)` rounds unexpectedly.

**How Common:** Extremely common, especially with Django (which historically
defaulted to timestamp without tz) and custom schemas.

**What's Missing:**
- No tool scans for timestamp columns and recommends timestamptz
- No tool detects timestamp(0) truncation risks

---

### Pain Point 2.5: char(n) and varchar(n) Misuse

**The Problem:** char(n) wastes space with padding and causes whitespace
comparison bugs. varchar(n) with arbitrary limits (varchar(255) is the MySQL
legacy) risks production errors when data exceeds the limit. The PostgreSQL wiki
says to use text + CHECK constraints instead.

**How Common:** Universal in schemas migrated from MySQL. varchar(255) is the
most common anti-pattern from the MySQL world.

**Existing Tools:** No automated detection.

**What's Missing:**
- No tool identifies varchar(255) columns and recommends text
- No tool detects char(n) columns

---

### Pain Point 2.6: Medium-Text Column Performance Trap

**The Problem:** Text values between ~128 bytes and 2KB (the TOAST threshold)
remain stored inline, making rows extremely wide. A table with 500K rows of
~1.8KB strings occupies 977MB vs 21MB for small strings. This causes 6x slower
queries even on warm cache.

**How Common:** Discovered and documented by Haki Benita with specific
benchmarks. Not widely known but affects any table storing error messages, URLs,
descriptions, or JSON snippets.

**Existing Tools:** No tool detects this.

**What's Missing:**
- No tool identifies columns where avg value size falls in the "medium text
  danger zone" (128B - 2KB)
- No tool recommends lowering toast_tuple_target for affected tables

**Ideal Solution:**
pg_sage should analyze pg_stats for average column widths and recommend TOAST
tuning for tables with medium-text performance traps.

---

### Pain Point 2.7: Ultra-Wide Tables

**The Problem:** Tables with dozens of columns pull unnecessary data, increase
TOAST overhead, and create unreadable slow query log entries. Queries that only
need 3 columns must read entire 50-column rows from disk.

**How Common:** Common in Rails apps (STI patterns), Django apps (model
inheritance), and any app that evolved organically.

**What's Missing:**
- No tool identifies tables that could benefit from vertical partitioning
- No tool correlates column usage (from pg_stat_statements) with table width

---

### Pain Point 2.8: Bloated Indexes on High-Churn Tables

**The Problem:** Every UPDATE/DELETE leaves dead index entries. On high-churn
tables (job queues, sessions, events), indexes grow rapidly and queries that
took 2ms crawl to 20ms within hours.

**How Common:** Very common for queue-like tables. Multiple blog posts document
this pattern with delayed_jobs, Sidekiq tables, and session stores.

**Existing Tools:**
- pgstattuple extension can measure bloat
- pg_repack can rebuild indexes online
- REINDEX CONCURRENTLY (PG 12+)

**What's Missing:**
- No tool automatically detects index bloat trends
- No tool recommends REINDEX schedules based on churn rate

---

### Pain Point 2.9: Missing Primary Keys

**The Problem:** Tables without primary keys prevent logical replication, make
VACUUM less efficient, and indicate fundamental schema design problems.

**How Common:** Found in legacy schemas, especially those evolved from ad-hoc SQL
scripts rather than ORM migrations.

**What's Missing:**
- No tool continuously monitors for tables without PKs

---

### Pain Point 2.10: Text-Based Status/Enum Columns

**The Problem:** Storing status values as text ('pending', 'active', 'cancelled')
wastes storage and makes indexes less efficient vs smallint + application enum.

**How Common:** Very common pattern. Most ORMs default to string-based enums.

---

## 3. N+1 Query Detection at Database Level

### Pain Point 3.1: N+1 Queries Are Invisible at the Database Level

**The Problem:** N+1 queries appear as one "normal" query executed thousands of
times. The database sees `SELECT * FROM comments WHERE post_id = $1` called
10,000 times but has no context that this is a loop, not legitimate application
behavior.

**How Common:** The #1 ORM performance problem across all frameworks. Every
framework community (Django, Rails, Node/Prisma, Go) struggles with this.
Called "The Silent Performance Killer" by multiple sources.

**Community Quotes:**
- "The naming makes it much clearer if called '1+N problem' since you execute
  one initial query, then N additional queries based on results." (HN)
- "rows_per can tell you when people have crappy filters in their code (no where
  clause) and total calls can let you focus on the ones with the biggest
  improvement" (HN, on pg_stat_statements)

**Existing Tools:**
- Bullet gem (Ruby) - catches N+1 in development/test
- Django Debug Toolbar - shows query counts per request
- MiniProfiler (.NET) - request-level query analysis
- pganalyze - can identify high-call-count queries

**What's Missing (Major Opportunity):**
- ALL existing tools operate at the application layer
- No tool detects N+1 patterns purely from database-side telemetry
- No tool correlates pg_stat_statements call counts with query patterns to
  identify N+1 candidates

**Detection Strategy for pg_sage:**
Using pg_stat_statements, identify N+1 candidates by:
1. **High calls, low mean_exec_time:** A query called 50,000 times averaging
   0.1ms is likely N+1
2. **Parameterized single-row lookups:** `SELECT ... WHERE id = $1` with
   extremely high call counts
3. **Temporal correlation:** Multiple queries to the same table executed in rapid
   succession within the same backend PID (via pg_stat_activity sampling)
4. **Call ratio analysis:** If query A (fetching posts) has 100 calls and query
   B (fetching comments by post_id) has 10,000 calls, B is likely N+1 of A
5. **Missing batch pattern:** Detect `WHERE id = $1` that could be `WHERE id =
   ANY($1)` based on call patterns

**Ideal Solution:**
pg_sage should provide a "N+1 detection score" for each normalized query in
pg_stat_statements, ranking by:
- calls / mean_exec_time ratio (high calls, low time = likely N+1)
- Correlation with other queries on related tables
- Estimated savings from batching (calls * mean_time vs 1 * batch_time)

---

### Pain Point 3.2: ORM-Generated Queries Are Opaque

**The Problem:** ORMs generate SQL that developers never see. Slow queries hidden
behind model.objects.all() or Post.includes(:comments) are invisible until
production performance degrades.

**How Common:** Universal. Every ORM user eventually hits this.

**What's Missing:**
- No database-side tool maps normalized queries back to likely ORM patterns
- No tool recommends specific ORM changes (e.g., "add .select_related()" or
  "use .includes()")

---

## 4. Materialized View Recommendations

### Pain Point 4.1: No Built-In Auto-Refresh

**The Problem:** PostgreSQL has no native mechanism to automatically refresh
materialized views when underlying data changes. Teams must use pg_cron, cron
jobs, or application-triggered refreshes.

**How Common:** Every team using materialized views faces this. Multiple blog
posts, HN threads, and StackOverflow questions about scheduling.

**Existing Tools:**
- pg_cron (requires extension installation, not available on all managed
  services)
- External cron jobs
- pg_ivm (incremental view maintenance, not available on RDS/Aurora)

**What's Missing:**
- No tool recommends optimal refresh intervals based on data change rates
- No tool monitors staleness (time since last refresh vs data change volume)
- No tool alerts when matview data diverges significantly from base tables

**Ideal Solution:**
pg_sage should:
1. Track last refresh time for all matviews
2. Monitor change rates on base tables (via pg_stat_user_tables n_tup_ins/upd)
3. Recommend refresh intervals based on staleness tolerance
4. Alert when matview is stale beyond configured threshold

---

### Pain Point 4.2: REFRESH Without CONCURRENTLY Blocks Reads

**The Problem:** REFRESH MATERIALIZED VIEW without CONCURRENTLY acquires ACCESS
EXCLUSIVE lock, blocking all reads of the matview during refresh. For dashboards
or APIs serving from matviews, this causes visible downtime.

**How Common:** Very common mistake. Many teams discover this in production.

**What's Missing:**
- No tool warns when a non-concurrent refresh is running on a matview being
  actively queried
- No tool automatically suggests CONCURRENTLY when appropriate

---

### Pain Point 4.3: CONCURRENTLY Requires Unique Index

**The Problem:** REFRESH MATERIALIZED VIEW CONCURRENTLY requires at least one
UNIQUE index on the matview. If no unique index exists, CONCURRENTLY fails with
an error. This is often discovered at 3am during the first attempted concurrent
refresh.

**How Common:** Common gotcha. The error message is clear but teams often don't
plan for it.

**What's Missing:**
- No tool pre-checks matviews for CONCURRENTLY readiness
- No tool recommends adding a unique index to matviews that should use
  CONCURRENTLY

**Ideal Solution:**
pg_sage should check all matviews for unique indexes and warn about those that
can't use CONCURRENTLY.

---

### Pain Point 4.4: Concurrent Refresh Doubles Disk Usage

**The Problem:** REFRESH CONCURRENTLY builds a complete new copy of the matview,
compares with the old version, then applies deltas. This temporarily requires
roughly 2x the matview's disk space plus additional temp space.

**How Common:** Surprise factor is high. Teams with large matviews (10GB+) on
tight disk budgets discover this painfully.

**What's Missing:**
- No tool estimates disk requirements for concurrent refresh
- No tool alerts when disk space is insufficient before attempting refresh

---

### Pain Point 4.5: Full Recomputation on Every Refresh

**The Problem:** Every refresh recomputes the entire underlying query from
scratch, even if only a handful of rows changed. For complex aggregation queries
over large tables, this wastes massive compute resources.

**How Common:** Fundamental limitation that frustrates everyone using matviews at
scale.

**Existing Tools:**
- pg_ivm provides incremental maintenance but is not available on managed
  services (RDS, Aurora, Cloud SQL)
- Materialize (streaming database, separate product)

**What's Missing:**
- No tool estimates refresh cost vs change volume
- No tool recommends when to switch from matviews to other approaches (caching
  tables, application-level caching, streaming)

---

### Pain Point 4.6: Trigger-Based Refresh Creates Cascading Disasters

**The Problem:** Using INSERT/UPDATE triggers to automatically refresh matviews
causes a refresh for every single row change. High-volume tables trigger
thousands of refreshes, each doing a full recomputation, causing runaway CPU and
dead tuple accumulation.

**How Common:** Every beginner attempt at "auto-refresh" hits this. Well-
documented footgun.

**What's Missing:**
- No tool detects trigger-based matview refresh patterns and warns about them
- No tool recommends debounced or batched refresh strategies

---

## 5. Migration Tool Complaints

### Pain Point 5.1: Flyway

- Rollback is a paid feature
- No built-in support for lock_timeout or statement_timeout
- Requires manual `flyway:executeInTransaction=false` annotation for
  CONCURRENTLY operations
- GitHub issue #2087 requests "always-executed" SQL files for setting timeouts

### Pain Point 5.2: Liquibase

- Environment variables are a paid feature
- Steeper learning curve with XML/YAML/JSON changelog formats
- Configuration complexity increases with version updates
- Advanced features create "noise rather than helping"

### Pain Point 5.3: Django Migrations

- Default behavior wraps all statements in a transaction (incompatible with
  CONCURRENTLY)
- AddField with default + NOT NULL causes full table rewrite on older PG
- Required third-party packages for safe migrations:
  - django-pg-zero-downtime-migrations (Yandex)
  - django-zero-downtime-migrations
- Ticket #28273: "Document how to prevent adding columns with defaults"

### Pain Point 5.4: Rails Migrations

- Running multiple migrations simultaneously causes race conditions
  (rails/rails#22092)
- disable_ddl_transaction! required for concurrent operations (easy to forget)
- add_index rollback doesn't use concurrently (rails/rails#24190)
- strong_migrations can't detect dangerous backfills inside transactions

### Pain Point 5.5: Prisma Migrate

- Advisory lock timeout is hardcoded at 10 seconds (not configurable until
  5.3.0)
- Failed migrations leave advisory locks held (prisma/prisma#12999)
- Multiple replicas cause advisory lock contention (prisma/prisma#27636)
- ALTER TYPE enum migrations fail inside transactions (prisma/prisma#5290)

### Pain Point 5.6: Goose (Go)

- SQL statement delimiter is semicolons only; PL/pgSQL requires manual
  annotation
- Limited support for programmatic logic in migrations
- Default schema is public only; configuring other schemas requires explicit
  options
- No built-in safety checks for lock-heavy operations

### Pain Point 5.7: Alembic (Python/SQLAlchemy)

- Tied to Python ecosystem; multi-language service architectures can't share it
- No built-in repeatable migrations
- Enum value additions fail when used in same transaction as new default values

---

## 6. Real Postmortems and Incidents

### Incident 6.1: 4-Hour Production Lockout (April 2026)

**Source:** Medium / Engineering Playbook postmortem

**What Happened:**
- Engineer ran `ALTER TABLE users ADD CONSTRAINT users_phone_number_check
  CHECK (phone_number IS NOT NULL)` on a table with 84 million rows
- AccessExclusiveLock held for entire 4 hours 12 minutes of validation
- 12.4 million users unable to log in
- All checkout API requests failed
- Estimated revenue loss: ~$48,000

**Timeline:**
- 2:47 PM: Migration started
- 2:52 PM: API alerts fired (response times hit 8,400ms)
- 3:07 PM: Attempts to cancel/terminate failed
- 5:34 PM: Migration 67% complete
- 6:59 PM: Migration completed

**Root Causes:**
1. Staging had 10,000 rows; production had 84 million (8,400x difference)
2. No lock_timeout set
3. No rollback strategy tested
4. Should have used NOT VALID + VALIDATE pattern

---

### Incident 6.2: GoCardless 15-Second Outage (January 2018)

**Source:** GoCardless Engineering Blog

**What Happened:**
- Adding foreign key constraints during migration
- FK constraint locks BOTH tables (the new table AND the referenced parent table)
- Long-running SELECT query held AccessShare lock on parent table
- ALTER TABLE queued for AccessExclusive lock
- All subsequent API queries queued behind it
- 15 seconds of total API unavailability

**Key Lesson:**
"Empty tables are safe" assumption failed because FK constraints lock the
referenced table, not just the table being modified.

---

### Incident 6.3: Clerk Database Incident (September 2025)

**Source:** Clerk engineering blog

**What Happened:**
- Cloud provider auto-upgraded PostgreSQL minor version
- New version optimized connection lock granting from O(n^2) to O(1)
- This removed an unintentional rate limiter on connection bursts
- Connection storms overwhelmed the database
- 4 days of intermittent failures (Sept 14-18)

**Key Lesson:**
Even minor PostgreSQL version changes can alter locking behavior in ways that
cascade to application-level failures.

---

### Incident 6.4: GitHub Schema Migration Incidents

**Source:** GitHub availability reports

GitHub has documented multiple schema migration incidents in their availability
reports. Large companies like GitHub, GitLab, and Meta maintain extensive
internal migration guides specifically because they've been burned by DDL
locking issues.

---

### Incident 6.5: Handshake Lock Queue Incident

**Source:** Handshake Engineering Blog

Documented the PostgreSQL lock queue problem where DDL statements without
lock_timeout waited indefinitely, queuing all subsequent queries and causing
cascading connection pool exhaustion.

---

## 7. Product Opportunity Summary

### What People Wish Existed

Based on community pain points, the ideal tool would:

1. **Pre-flight DDL Analysis:** Before any DDL runs, analyze the target table's
   size, current lock state, active queries, and replication lag to predict
   whether the operation is safe

2. **Safe Alternative Suggestions:** Automatically recommend the safe version of
   every dangerous DDL operation:
   - `SET NOT NULL` -> NOT VALID CHECK + VALIDATE pattern
   - `CREATE INDEX` -> CONCURRENTLY
   - `ADD CONSTRAINT FK` -> NOT VALID + VALIDATE
   - `ALTER COLUMN TYPE` -> expand-contract with new column
   - `ADD COLUMN DEFAULT (volatile)` -> nullable + backfill + constraint

3. **Continuous Schema Health Monitoring:**
   - Missing FK indexes
   - Approaching int4 sequence overflow
   - Invalid indexes from failed CONCURRENTLY builds
   - Unused indexes consuming write overhead
   - Tables without primary keys
   - char(n)/varchar(255) anti-patterns
   - timestamp without timezone
   - serial instead of identity
   - UUIDv4 random primary keys

4. **N+1 Query Detection from pg_stat_statements:**
   - High-call-count single-row lookups
   - Call ratio analysis between parent/child table queries
   - Estimated savings from batching

5. **Materialized View Management:**
   - Staleness monitoring (time since refresh vs data change rate)
   - CONCURRENTLY readiness check (unique index presence)
   - Disk space estimation for concurrent refresh
   - Refresh scheduling recommendations
   - Detection of trigger-based refresh anti-patterns

### Competitive Landscape Gaps

| Capability | pganalyze | Squawk | strong_migrations | pgroll | pg_sage |
|---|---|---|---|---|---|
| Pre-flight DDL safety | No | CI only | Dev only | No | **Yes** |
| Lock queue prediction | No | No | No | No | **Yes** |
| Schema anti-pattern scan | Partial | No | No | No | **Yes** |
| N+1 detection (DB-side) | Partial | No | No | No | **Yes** |
| Matview management | No | No | No | No | **Yes** |
| Safe DDL rewriting | No | No | Partial | Yes | **Yes** |
| Continuous monitoring | Yes | No | No | No | **Yes** |
| Sequence overflow warning | No | No | No | No | **Yes** |
| Invalid index detection | No | No | No | No | **Yes** |

### Priority Ranking by Community Pain Severity

1. **DDL lock safety** - Causes actual production outages, revenue loss
2. **Missing FK indexes** - Most common silent performance killer
3. **N+1 detection** - Highest cumulative performance impact
4. **Schema anti-patterns** - Prevents future pain (proactive value)
5. **Matview management** - Operational burden on teams using matviews
6. **Sequence overflow** - Ticking time bomb, catastrophic when it hits
7. **Invalid index cleanup** - Silent write performance degradation

---

## Sources

### Postmortems and Incident Reports
- [Database Migration That Locked Production for 4 Hours](https://medium.com/engineering-playbook/the-database-migration-that-locked-production-for-4-hours-complete-postmortem-1a4955f64b63)
- [Clerk Database Incident Postmortem (Sept 2025)](https://clerk.com/blog/2025-09-18-database-incident-postmortem)
- [GoCardless: Zero-Downtime Postgres Migrations - The Hard Parts](https://gocardless.com/blog/zero-downtime-postgres-migrations-the-hard-parts/)
- [Database Migration Horror Stories: 10 Companies](https://medium.com/the-tech-draft/database-migration-horror-stories-lessons-from-10-companies-that-got-it-wrong-and-right-71857e3319da)

### Technical Guides and Analysis
- [Common DB Schema Change Mistakes (PostgresAI)](https://postgres.ai/blog/20220525-common-db-schema-change-mistakes)
- [Schema Changes and the Postgres Lock Queue (Xata)](https://xata.io/blog/migrations-and-exclusive-locks)
- [Postgres Schema Changes Are Still a PITA (Xata)](https://xata.io/blog/postgres-schema-changes-pita)
- [5 PostgreSQL Migration Mistakes (dev.to)](https://dev.to/mickelsamuel/the-5-postgresql-migration-mistakes-that-cause-production-outages-ngg)
- [Complete Guide to PostgreSQL Lock Types (dev.to)](https://dev.to/mickelsamuel/the-complete-guide-to-postgresql-lock-types-for-schema-changes-4b70)
- [Safe and Unsafe Operations for High Volume PostgreSQL](http://leopard.in.ua/2016/09/20/safe-and-unsafe-operations-postgresql)
- [Zero-Downtime Schema Migrations: lock_timeout and Retries (PostgresAI)](https://postgres.ai/blog/20210923-zero-downtime-postgres-schema-migrations-lock-timeout-and-retries)
- [When Does ALTER TABLE Require a Rewrite (Crunchy Data)](https://www.crunchydata.com/blog/when-does-alter-table-require-a-rewrite)
- [ALTER TABLE ADD COLUMN Done Right (Cybertec)](https://www.cybertec-postgresql.com/en/postgresql-alter-table-add-column-done-right/)

### Anti-Patterns and Best Practices
- [Don't Do This - PostgreSQL Wiki](https://wiki.postgresql.org/wiki/Don't_Do_This)
- [Five PostgreSQL Anti-Patterns (Shey Sewani)](https://shey.ca/2025/09/12/five-db-anti-patterns.html)
- [10 Postgres Anti-Patterns Killing Your Concurrency](https://medium.com/@Praxen/10-postgres-anti-patterns-killing-your-concurrency-cdd2ae32d408)
- [10 Things I Hate About PostgreSQL (Rick Branson)](https://rbranson.medium.com/10-things-i-hate-about-postgresql-20dbab8c2791)
- [Surprising Impact of Medium-Size Texts (Haki Benita)](https://hakibenita.com/sql-medium-text-performance)
- [Beware The Missing Foreign Key Index (dev.to)](https://dev.to/jbranchaud/beware-the-missing-foreign-key-index-a-postgres-performance-gotcha-3d5i)
- [Foreign Key Indexing and Performance (Cybertec)](https://www.cybertec-postgresql.com/en/index-your-foreign-key/)

### Schema Migration Tools
- [Squawk - Linter for PostgreSQL Migrations](https://squawkhq.com/)
- [strong_migrations (Ruby)](https://github.com/ankane/strong_migrations)
- [pgroll - Zero-Downtime Migrations](https://github.com/xataio/pgroll)
- [Reshape - Zero-Downtime Migrations](https://github.com/fabianlindfors/reshape)
- [safe-pg-migrations (Doctolib)](https://github.com/doctolib/safe-pg-migrations)

### N+1 Query Detection
- [Solving N+1 Postgres Queries for Rails (Crunchy Data)](https://www.crunchydata.com/blog/postgresql-for-solving-n+1-queries-in-ruby-on-rails)
- [N+1 Query Problem and How to Detect It (Digma)](https://digma.ai/n1-query-problem-and-how-to-detect-it/)
- [Understanding the N+1 Queries Problem (HN Discussion)](https://news.ycombinator.com/item?id=34207974)

### Materialized Views
- [Postgres Materialized View Auto Refresh (Epsio)](https://www.epsio.io/blog/postgres-materialized-view-auto-refresh)
- [Real-Time Materialized Views with pg_ivm](https://medium.com/@sjksingh/real-time-materialized-views-with-pg-ivm-8a3f4d6cd464)
- [Limitations with Postgres Materialized Views](https://kishore-rjkmr.medium.com/my-experience-with-postgres-materialized-view-36d9f3407c87)
- [Creating and Refreshing Materialized Views (Cybertec)](https://www.cybertec-postgresql.com/en/creating-and-refreshing-materialized-views-in-postgresql/)

### Hacker News Discussions
- [Schema Changes and the Postgres Lock Queue](https://news.ycombinator.com/item?id=40735092)
- [pgroll: Zero-Downtime Reversible Schema Migrations](https://news.ycombinator.com/item?id=37752366)
- [Zero-Downtime Schema Migrations Using Reshape](https://news.ycombinator.com/item?id=29825520)
- [Migrated PostgreSQL with 11 Seconds Downtime](https://news.ycombinator.com/item?id=39048317)
- [PostgreSQL and UUID as Primary Key](https://news.ycombinator.com/item?id=40884878)
