# pg_sage v0.10 "Schema Intelligence" -- Competitive Analysis

> Research date: 2026-04-13
> Purpose: Deep competitive landscape for v0.10 feature planning
> Covers: Migration Safety, Schema Anti-Pattern Detection, N+1 Detection, Materialized View Management

---

## Executive Summary

The market for database schema intelligence is fragmented across four categories: migration safety (linting DDL before execution), schema anti-pattern detection (finding design smells in existing schemas), N+1 query detection (identifying application-level query patterns), and materialized view management (automated refresh/lifecycle). No single product covers all four. Most tools are static/offline -- they analyze SQL files or snapshots, not live databases. pg_sage's unique advantage is **runtime access**: pg_stat_statements, pg_stat_activity, pg_locks, the catalog, EXPLAIN plans, AND an LLM. This lets us detect problems that static tools cannot see and provide fixes that offline tools cannot execute.

---

## Part 1: Migration Safety / DDL Safety Tools

### 1.1 Squawk

**What it is**: Static linter for PostgreSQL migrations. Written in Rust.
**GitHub**: github.com/sbdchd/squawk (~1,000 stars)
**License**: Open source (GPL-3.0)
**Pricing**: Free
**Current version**: 2.47.0

**What it does well**:
- 30+ rules covering lock safety, destructive ops, schema design
- Key rules: `require-concurrent-index-creation`, `constraint-missing-not-valid`, `adding-field-with-default`, `ban-drop-column`, `prefer-bigint-over-int`, `prefer-timestamptz`, `prefer-identity`
- VSCode extension and language server for inline linting
- GitHub PR integration (comments directly on PRs)
- GitHub Action available for CI pipelines
- Configurable per-file, per-rule, and per-PG-version
- Comment-based suppression (`squawk-ignore`)
- Fast (Rust, WASM playground available)

**What it misses / user complaints**:
- Static only -- analyzes SQL files, never connects to the database
- Cannot assess actual table size, row count, or lock contention risk
- No understanding of current database state (is the table empty? does the index already exist?)
- No runtime context: a `CREATE INDEX CONCURRENTLY` on a 500M-row table is different from one on a 500-row table, but Squawk treats them the same
- Cannot detect operations that are safe for YOUR specific PG version but flagged generically
- No fix generation -- tells you what is wrong but not how to fix it safely
- No materialized view awareness
- No N+1 detection
- Cannot prioritize findings by impact

**How pg_sage can do it better**:
- We have catalog access: know table sizes, row counts, existing indexes, constraints
- We have pg_locks/pg_stat_activity: can assess real-time lock risk before migration
- We have LLM: can generate the safe alternative DDL, not just flag the problem
- We can execute safely: run `CREATE INDEX CONCURRENTLY` ourselves with monitoring
- We can time migrations: recommend execution windows based on workload patterns
- Can assess version-specific safety (PG14 vs PG17 behavior differences)

---

### 1.2 pgroll (Xata)

**What it is**: Zero-downtime PostgreSQL migration tool using expand/contract pattern. Written in Go.
**GitHub**: github.com/xataio/pgroll (~5,000+ stars)
**License**: Open source (Apache-2.0)
**Pricing**: Free
**Current version**: v0.16.1 (Feb 2026)

**What it does well**:
- True zero-downtime migrations via dual-schema versioning
- Old and new schema versions served simultaneously via PostgreSQL views
- Automatic column backfilling with trigger-based data sync
- Instant rollback during active migrations
- Works with any Postgres 14.0+ (including RDS, Aurora)
- Single binary, no external dependencies
- Reversible migrations by default
- YAML migration file support (v0.11+)

**What it misses / user complaints**:
- search_path dependency: relies on search_path to distinguish schema versions, fragile if apps manipulate it
- ORM/framework limitations: Django, Prisma, Drizzle support still incomplete
- No view versioning: cannot provide access to multiple versions of the same view
- Complex migrations require JSON/YAML definition -- no GUI
- No schema linting or anti-pattern detection
- No query analysis or performance awareness
- Operational complexity: requires understanding expand/contract lifecycle
- Trigger overhead on high-write tables during migration window

**How pg_sage can do it better**:
- We don't need expand/contract for most operations -- we can assess which operations are truly safe at runtime
- For operations that DO need expand/contract, we can plan the multi-step migration automatically with LLM
- We can predict migration duration based on actual table size + current load
- We can monitor migration progress in real-time (pg_stat_progress_*)
- We can recommend the optimal execution window
- We combine safety assessment WITH execution -- not two separate tools

---

### 1.3 Reshape

**What it is**: Zero-downtime PostgreSQL migration tool (similar to pgroll). Written in Rust.
**GitHub**: github.com/fabianlindfors/reshape
**License**: Open source (MIT)
**Pricing**: Free

**What it does well**:
- Same expand/contract pattern as pgroll
- Supports tables, columns, indexes, enums, foreign keys, raw SQL
- Helper libraries for Rust, Ruby, Python, Go (client-side schema selection)
- Three-phase workflow: start -> rollout -> complete
- Clean separation of old/new schema via views
- `reshape docs` command for AI assistant integration

**What it misses / user complaints**:
- Less actively maintained than pgroll (smaller community)
- Same view/trigger overhead as pgroll
- No schema linting
- No query analysis
- No runtime awareness
- Migrations must be idempotent for dev iteration

**How pg_sage can do it better**: Same advantages as vs. pgroll.

---

### 1.4 Atlas (Ariga)

**What it is**: Declarative schema management with schema-as-code workflows. Written in Go.
**GitHub**: github.com/ariga/atlas
**License**: Open source core (Apache-2.0), commercial features behind Atlas Pro
**Pricing**:
- Starter: Free (limited databases, basic features)
- Pro: $9/month per dev + $59/month per CI/CD project + $39/month per monitored DB
- Enterprise: Custom pricing (20+ databases)

**What it does well**:
- Declarative schema definition (SQL, HCL, or ORM)
- `atlas migrate lint` with 50+ safety analyzers:
  - Destructive changes (drops)
  - Data-dependent changes (unique constraint on column with duplicates)
  - Backward-incompatible changes (renames)
  - PostgreSQL-specific: concurrent index policy, blocking changes (PG301-PG311), table rewrites
  - Naming convention enforcement
  - SQL injection detection in migration files
  - Nested transaction detection
- Pre-migration checks (assert table is empty before drop, check for duplicates before unique index)
- Drift detection (schema vs. declared state)
- Schema-as-code with Terraform-like workflows
- ORM integration (Django, GORM, Sequelize, etc.)
- Schema ownership policy (CODEOWNERS-style)
- 2025: PII detection, migration hooks, pgvector support, partition management

**What it misses / user complaints**:
- Best analyzers (concurrent index, blocking changes, ownership) require Atlas Pro (paid)
- No runtime analysis -- lints SQL files, not live database
- Cannot assess actual impact (table size, current load, replication lag)
- No query performance analysis
- No N+1 detection
- No materialized view management
- No automated fix generation (flags problems, doesn't fix them)
- Requires adoption of Atlas workflow (not a drop-in for existing migration tools)

**How pg_sage can do it better**:
- Runtime-aware: we know if the table has 10 rows or 10 billion
- We can execute the safe alternative, not just warn about it
- We can combine Atlas-level linting with actual execution monitoring
- We provide LLM-generated migration plans, not just static rules
- We work with whatever migration tool the user already has (Flyway, Liquibase, raw SQL)
- No workflow adoption required -- we're a sidecar, not a migration framework

---

### 1.5 Bytebase

**What it is**: Database DevSecOps platform. Written in Go + TypeScript.
**GitHub**: github.com/bytebase/bytebase
**License**: Open source (AGPL, with commercial features)
**Pricing**:
- Community: Free (up to 20 users, 10 DB instances)
- Pro: $20/user/month
- Enterprise: Custom

**What it does well**:
- 200+ SQL review rules (38 PostgreSQL-specific)
- GitOps integration (GitHub/GitLab)
- Multi-database support (14 engines)
- Schema drift detection
- Approval workflows with risk assessment
- Data masking
- Audit logging
- Online schema change support
- Web UI for managing migrations

**What it misses / user complaints**:
- Schema drift detection has accuracy issues (GitHub #123 -- views/stored procedures showing as drift)
- AUTO_INCREMENT values compared as structural changes
- Only 38 PG-specific rules vs. 200+ total (MySQL-centric)
- No query performance analysis
- No N+1 detection
- No materialized view management
- No index advisor
- No VACUUM advisor
- Heavy platform -- requires deploying a full web application
- Pro tier is cloud-only (no self-host)
- State-based approach cannot distinguish rename vs. drop+create

**How pg_sage can do it better**:
- Lightweight sidecar vs. heavy platform
- Runtime query analysis (pg_stat_statements)
- Index optimization with HypoPG validation
- VACUUM tuning
- LLM-powered fix generation
- We can integrate with Bytebase as a data source rather than competing

---

### 1.6 Flyway (Redgate) / Liquibase

**Flyway**:
- **License**: Community (free), Enterprise (paid, per-user, price not public)
- **Safety features**: Checksum validation (detects modified migrations), dry run mode (Enterprise only), undo migrations (Enterprise only)
- **Drift detection**: Enterprise only
- **Policy enforcement**: Enterprise only
- **User complaints**: Huge price jump from free to Enterprise; dry runs, undo, and advanced features gated behind paywall

**Liquibase**:
- **License**: Community (free, Apache-2.0), Secure ($5,000+/year)
- **Safety features**: Precondition checks, rollback support, changeset validation
- **Policy Checks (Liquibase Pro/Secure)**: Custom Python scripts for compliance, separation of duties enforcement, tamper-evident audit trails
- **2025 updates**: Liquibase Secure 5.0 with VS Code extension, policy enforcement in IDE

**What they both miss**:
- No runtime database analysis
- No query performance awareness
- No index or VACUUM advisory
- No N+1 detection
- No materialized view management
- Migration-focused only -- no ongoing schema health monitoring
- Safety features (where they exist) are mostly gated behind expensive paid tiers

**How pg_sage can do it better**:
- We complement these tools rather than replace them
- We can lint the SQL before Flyway/Liquibase applies it
- We can monitor the migration as it executes (lock waits, progress, duration)
- We can detect problems these tools create (missing concurrent index, table rewrites)
- We provide ongoing schema health monitoring, not just migration-time checks

---

### 1.7 gh-ost / pt-online-schema-change

**What they are**: MySQL online schema change tools. Not PostgreSQL, but the patterns apply.

**gh-ost (GitHub)**:
- Triggerless approach using binlog streaming
- True pause capability (zero load when paused)
- Replica testing mode (validate migration without affecting master)
- Replication lag control (configurable threshold, default 1.5s)
- Postponable cut-over (control when the final table swap happens)
- Limitation: No FK support, no trigger support, no Galera support

**pt-online-schema-change (Percona)**:
- Trigger-based approach (INSERT/UPDATE/DELETE triggers on source table)
- Safety checks: max thread count (default 50), PK/unique index required
- FK handling with multiple methods (rebuild_constraints, drop_swap)
- Limitation: 2x disk space required, metadata locks unavoidable

**Patterns pg_sage should adopt**:
- Pausable migrations with zero overhead when paused
- Replication lag awareness before/during DDL
- Cut-over timing control (choose when to do the final swap)
- Progress monitoring with real-time metrics
- Automatic throttling based on server load
- Replica-first testing for DDL validation

---

### 1.8 dryrun (boringSQL)

**What it is**: PostgreSQL MCP server for offline schema intelligence. Written in Rust.
**GitHub**: github.com/boringSQL/dryrun (17 stars, very new)
**License**: Open source (BSD 2-Clause)
**Pricing**: Free

**What it does well**:
- 33 lint rules (19 convention + 14 audit)
- Migration safety: lock type, rewrite risk, safe alternative, rollback DDL for every statement
- PG version-aware (knows what is safe on your version)
- Offline-first: works from JSON schema snapshot, no DB connection needed
- MCP server (16 tools) for AI coding assistants
- Vacuum health monitoring from snapshot
- Security-conscious: no prod credentials required

**What it misses**:
- 17 GitHub stars -- very early, unproven at scale
- Offline only -- cannot assess runtime state (actual table sizes, live locks, current load)
- No query analysis
- No N+1 detection
- No materialized view management
- No automated execution
- Snapshot can be stale
- Cannot assess impact of DDL on running workload

**How pg_sage can do it better**:
- We have live database access -- not working from a snapshot
- We can assess actual runtime impact
- We can execute the safe alternative, not just suggest it
- We have LLM for generating complex migration plans
- We already have 18+ deterministic rules + LLM-powered analysis
- We can be BOTH an MCP server AND a live monitor

---

### 1.9 pgMustard

**What it is**: PostgreSQL query plan visualization and analysis tool.
**Pricing**: 95 EUR/year (~$110 USD)
**License**: Commercial (SaaS)

**What it does well**:
- Beautiful EXPLAIN plan visualization with color-coded bottlenecks
- Calculates per-operation timings (not just cumulative)
- Hints for specific optimization opportunities
- Supports PG17 features (SERIALIZE, MEMORY)
- Public API for plan scoring/benchmarking

**What it misses**:
- Manual process: paste EXPLAIN output, get visualization
- No live monitoring or automation
- No migration safety
- No schema linting
- No N+1 detection
- No materialized view management
- Single-query focus, no workload-level analysis

**How pg_sage can do it better**:
- We automatically collect and analyze EXPLAIN plans
- We correlate plan analysis with pg_stat_statements metrics
- We detect plan regressions over time
- We suggest AND execute fixes (indexes, query hints)
- We're always-on, not a manual paste-and-analyze workflow

---

## Part 2: Schema Anti-Pattern Detection / Schema Linting

### 2.1 SchemaCrawler

**What it is**: Free database schema discovery and comprehension tool. Written in Java.
**GitHub**: github.com/schemacrawler/SchemaCrawler
**License**: Open source (Eclipse Public License)
**Pricing**: Free

**Lint rules (22 built-in)**:
1. `LinterColumnTypes` -- Columns with same name but different types across tables
2. `LinterForeignKeyMismatch` -- FK column type differs from referenced PK type
3. `LinterForeignKeySelfReference` -- Table FK references own PK (deletion issues)
4. `LinterForeignKeyWithNoIndexes` -- FKs without indexes
5. `LinterNullColumnsInIndex` -- Nullable columns in unique indexes
6. `LinterNullIntendedColumns` -- Default value is string 'NULL' instead of actual NULL
7. `LinterRedundantIndexes` -- Indexes with redundant column sequences
8. `LinterTableAllNullableColumns` -- All non-PK columns are nullable
9. `LinterTableCycles` -- Cyclical table relationships
10. `LinterTableEmpty` -- Empty tables
11. `LinterTableWithBadlyNamedColumns` -- Columns matching undesired patterns
12. `LinterTableWithIncrementingColumns` -- Denormalized incrementing column names
13. `LinterTableWithNoIndexes` -- Tables without any indexes
14. `LinterTableWithNoPrimaryKey` -- Missing primary keys
15. `LinterTableWithNoRemarks` -- Missing documentation/comments
16. `LinterTableWithNoSurrogatePrimaryKey` -- Multi-column PKs without surrogate
17. `LinterTableWithPrimaryKeyNotFirst` -- PK columns not positioned first
18. `LinterTableWithQuotedNames` -- Spaces or reserved words in names
19. `LinterTableWithSingleColumn` -- Tables with 0 or 1 column
20. `LinterTooManyLobs` -- Excessive LOB columns
21. `LinterCatalogSql` -- Custom SQL-based checks
22. `LinterTableSql` -- Per-table custom SQL checks

Additional PG-specific lints available via `schemacrawler-additional-lints` extension.

**What it does well**:
- Comprehensive cross-database schema discovery
- Schema diagrams and diff-able text output
- Extensible via custom SQL and scripting
- JDBC-based, works with any database

**What it misses**:
- Java dependency, heavy for a PostgreSQL-only use case
- No runtime query analysis
- No lock/migration awareness
- No fix generation
- No N+1 detection
- No materialized view management
- Lint rules are fairly basic compared to Atlas or Squawk
- No CI/CD integration out of the box

---

### 2.2 SchemaHero

**What it is**: Kubernetes operator for declarative database schema management.
**GitHub**: github.com/schemahero/schemahero
**License**: Open source (Apache-2.0)
**Pricing**: Free

**What it does well**:
- Declarative YAML schema definitions
- Automatic migration generation (diff desired vs. actual)
- GitOps integration (ArgoCD, Flux)
- Kubernetes-native workflow
- Supports Postgres and MySQL

**What it misses**:
- Kubernetes-only -- useless outside K8s
- No schema linting or anti-pattern detection
- No runtime analysis
- No query performance analysis
- No N+1 detection
- No materialized view management
- No safety analysis on generated migrations
- Dead-simple tool -- no intelligence layer

---

### 2.3 pganalyze

**What it is**: PostgreSQL performance monitoring and optimization platform. SaaS.
**Pricing**:
- Scale: $149/server/month (up to 100 queries)
- Business: $249/server/month (up to 500 queries)
- Enterprise: $399/server/month (unlimited queries)
- 14-day free trial

**What it does well**:
- **Index Advisor**: Analyzes workload, recommends optimal index set, considers Index Write Overhead
- **VACUUM Advisor**: Per-table autovacuum tuning, bloat detection, freezing analysis, VACUUM simulator
- **Query Advisor**: Detects anti-patterns in EXPLAIN plans (inefficient nested loops, poor index usage)
- **Schema Statistics**: Per-table bloat estimates, column statistics
- **Log Insights**: Continuous Postgres log monitoring
- **Wait Events**: I/O and lock wait analysis
- Drift detection between query performance over time
- Per-query stats trending

**What it misses / user complaints**:
- No migration safety analysis or DDL linting
- No N+1 query detection
- No materialized view management
- No schema anti-pattern detection (naming, design smells)
- No automated execution (advisory only, never acts)
- Pricing high for small teams ($149-$399/server/month)
- Learning curve for new users
- Interface doesn't auto-refresh
- No lock contention insights beyond basics
- No custom query tagging
- No official MCP server for agentic workflows
- Pricing can be opaque

**How pg_sage can do it better**:
- We DO everything pganalyze does for indexes and VACUUM, plus we EXECUTE
- We add migration safety (they don't have it)
- We add schema linting (they don't have it)
- We can detect N+1 patterns via pg_stat_statements (they don't)
- We use LLM for richer, context-aware recommendations
- We're a sidecar (your data stays local) vs. SaaS (data shipped to cloud)
- We're open source vs. $149-$399/server/month
- Our trust ramp model (monitor -> advisory -> autonomous) goes beyond advisory-only

---

### 2.4 Datadog Database Monitoring

**What it is**: Database monitoring as part of Datadog's observability platform.
**Pricing**: $70/DB host/month (annual) or $84/host/month (on-demand)

**What it does well**:
- Unified APM + database monitoring (correlate app traces with DB queries)
- Query samples and execution plans
- Schema Explorer (view schema without DB credentials)
- Wait event analysis
- Automatic anomaly detection
- Beautiful dashboards
- Integration with full Datadog ecosystem (logs, APM, infra)

**What it misses**:
- No EXPLAIN plan analysis or index recommendations
- No VACUUM advisory
- No bloat detection
- No schema linting or anti-pattern detection
- No migration safety
- No N+1 detection at the database level
- No materialized view management
- Thin PostgreSQL integration -- strong on metrics, weak on intelligence
- Cannot work through pgbouncer/connection poolers (inaccurate metrics)
- Expensive when combined with full Datadog stack
- No automated remediation

**How pg_sage can do it better**:
- Deep PostgreSQL intelligence vs. generic database metrics
- Index advisor, VACUUM tuner, query rewriter
- Schema linting and migration safety (they have none)
- Automated execution via trust ramp
- Orders of magnitude cheaper (open source vs. $70+/host/month + Datadog lock-in)

---

### 2.5 PostgresAI / Database Lab Engine

**What it is**: Database branching and thin cloning for PostgreSQL testing.
**License**: Open source (Apache-2.0) for Standard Edition; Enterprise is commercial
**Pricing**: SE free, EE custom pricing

**What it does well**:
- Clone 10 TiB database in <2 seconds
- Dozens of independent clones simultaneously
- CI Observer for monitoring schema changes in CI/CD
- EXPLAIN plan analysis on clones
- Safe testing environment for schema changes
- Works with RDS, Aurora, CloudSQL, AlloyDB

**What it misses**:
- Clone infrastructure -- not a monitoring/advisory tool
- No schema linting
- No N+1 detection
- No materialized view management
- No ongoing production monitoring
- Requires significant infrastructure (ZFS, dedicated hardware)
- No automated remediation

**How pg_sage can do it better**:
- We monitor production continuously, not just test on clones
- We provide recommendations AND execution
- Complementary tool: pg_sage could recommend, Database Lab could test

---

## Part 3: N+1 Query Detection

### 3.1 Bullet (Ruby/Rails)

**What it is**: Rails gem for detecting N+1 queries and unused eager loading.
**GitHub**: github.com/flyerhzm/bullet
**License**: Open source (MIT)
**Pricing**: Free

**What it does well**:
- Real-time detection during development
- Suggests eager loading fixes
- Multiple notification channels (log, Honeybadger, browser)
- Detects unused eager loading (over-fetching)
- Supports ActiveRecord and Mongoid
- Widely adopted in Rails ecosystem

**What it misses / limitations**:
- Rails/Ruby only -- useless for Go, Python, Java, Node.js
- False negatives: misses N+1 in some cases
- False positives: can flag legitimate patterns
- Does not detect N+1 in controller tests (no view rendering)
- Does not detect N+1 in view tests (no DB queries)
- Development-time only -- no production monitoring
- ORM-dependent: only works with supported ORMs
- Cannot detect N+1 patterns that span multiple HTTP requests

---

### 3.2 django-silk

**What it is**: Django profiling middleware.
**GitHub**: github.com/jazzband/django-silk
**License**: Open source (MIT)
**Pricing**: Free

**What it does well**:
- Intercepts all SQL queries per request
- Visual inspection of query count and patterns
- Per-request query breakdown
- EXPLAIN analysis support

**What it misses**:
- No dedicated N+1 detection (must manually inspect query patterns)
- Django only
- Development-time only
- Performance overhead in production
- Fork `django-silky` added explicit N+1 detection, but it's a fork

---

### 3.3 nplusone (Python)

**What it is**: Auto-detecting N+1 queries in Python ORMs.
**GitHub**: github.com/jmcarp/nplusone
**License**: Open source (MIT)
**Pricing**: Free

**What it does well**:
- Supports SQLAlchemy, Peewee, Django ORM
- Detects inappropriate eager loading (loaded but never accessed)
- Can be used in tests to force N+1 failures
- Context manager for non-HTTP code paths

**What it misses**:
- Python only
- ORM-dependent (must use supported ORM)
- Development-time only
- No production monitoring
- Maintenance appears sporadic

---

### 3.4 APM-Level Detection (AppSignal, Scout APM, New Relic)

**AppSignal**:
- Labels N+1 queries with count (e.g., "N+32")
- Visible in Event Timeline and Performance Issue overview
- Automatic detection, no setup required
- Supports Ruby, Python, Node.js, Elixir
- Pricing: starts ~$17/month

**Scout APM**:
- Detects N+1 consuming >150ms
- Shows exact line of code causing N+1
- Tracks database rows returned
- Supports Ruby, Python, PHP, Elixir
- Pricing: starts ~$39/month

**New Relic**:
- No dedicated N+1 detection feature
- Can identify patterns via query-level insights and execution plans
- 2025: Deep Query Analysis GA with wait types and explain plans
- Pricing: consumption-based, expensive at scale

**What APMs miss**:
- Application-side only -- require instrumentation in every app
- Language/framework specific
- Cannot detect N+1 from database perspective (only see individual queries)
- Cannot detect N+1 across different applications hitting same DB
- Expensive for database-only use case
- No fix execution

**How pg_sage can detect N+1 from the database side**:
- pg_stat_statements shows normalized query patterns with call counts
- A query called 1000x in 1 second with incrementing parameter values = N+1
- Correlate with pg_stat_activity to see concurrent identical queries from same client
- No application instrumentation required
- Language/framework agnostic -- works with ANY application
- Can detect N+1 across ALL applications hitting the database
- LLM can suggest the batched/joined alternative query
- Can quantify the impact: "this N+1 pattern costs 2.3s per request, batching would reduce to 50ms"

**This is a major differentiator.** No existing tool detects N+1 at the database level.

---

## Part 4: Materialized View Management

### 4.1 pg_ivm (Incremental View Maintenance)

**What it is**: PostgreSQL extension for incrementally maintaining materialized views.
**GitHub**: github.com/sraoss/pg_ivm
**License**: Open source
**Current version**: 1.13 (Oct 2025)
**Supports**: PostgreSQL 13-18

**What it does well**:
- Immediate maintenance: IMMV updated on every base table change via triggers
- No manual REFRESH needed
- Supports: inner joins (1.0+), outer joins (1.13+), aggregates, subqueries, CTEs
- Significant performance improvement for read-heavy workloads with infrequent writes

**Limitations**:
- High write overhead: every INSERT/UPDATE/DELETE triggers IMMV update
- Blocks writes during materialized view update
- Not effective for frequently modified base tables
- Unsupported: window functions, HAVING, ORDER BY, LIMIT/OFFSET, UNION/INTERSECT/EXCEPT, DISTINCT ON, TABLESAMPLE, VALUES, FOR UPDATE/SHARE
- Only simple equijoins for outer joins
- Base tables must be simple tables (no views, no partitioned tables, no foreign tables)
- No json, xml, or point types in target list
- No logical replication support
- min/max maintenance can be slow when current min/max is deleted (rescans base table)
- Third-party extension -- not in core PostgreSQL (proposed but not accepted)

---

### 4.2 TimescaleDB Continuous Aggregates

**What it is**: Incrementally updated materialized views for time-series data.
**License**: Timescale License (source-available, not fully OSS)
**Pricing**: Free (self-hosted Timescale), paid cloud tiers

**What it does well**:
- Automatic incremental refresh -- only recalculates changed time buckets
- Real-time mode: combines materialized data with fresh raw data (default since 2.0)
- Refresh policies: scheduled automatic refresh at configurable intervals
- Hierarchical aggregates: stack continuous aggregates on top of each other (2.9+)
- Compression support (10-20x compression ratios)
- MERGE-based refresh (v2.17.0+ on PG15+) -- more efficient than delete+reinsert

**Limitations**:
- TimescaleDB only -- requires Timescale extension
- Hypertables only -- not for regular PostgreSQL tables
- Time-series focused -- not general-purpose materialized views
- Real-time mode can have performance overhead on large result sets
- Cannot use all SQL features in aggregate definitions

---

### 4.3 Native PostgreSQL Materialized Views

**Current state**: No built-in auto-refresh, no incremental maintenance.

**Common workarounds**:
- pg_cron / pgAgent for scheduled REFRESH
- Application-level triggers to REFRESH after data changes
- Manual REFRESH MATERIALIZED VIEW CONCURRENTLY

**Problems with status quo**:
- REFRESH is a full recompute -- expensive on large tables
- CONCURRENTLY requires a unique index
- No visibility into refresh duration, staleness, or failure
- No way to know when a matview is "too stale"
- No automatic staleness detection
- No cost-benefit analysis (is this matview worth its refresh cost?)

**How pg_sage can do it better -- Materialized View Intelligence**:

1. **Discovery**: Automatically find all materialized views via catalog
2. **Staleness Detection**: Compare matview data age with base table modification time (pg_stat_user_tables.last_analyze, n_mod_since_analyze)
3. **Refresh Cost Analysis**: Track REFRESH duration history, predict cost of next refresh
4. **Usage Analysis**: Cross-reference matview with pg_stat_statements to see which queries use it
5. **Cost-Benefit Score**: (query_speedup * query_frequency) / refresh_cost
6. **Auto-Refresh Scheduling**: Recommend or execute REFRESH based on staleness threshold + workload patterns
7. **Concurrent Refresh Safety**: Verify unique index exists before CONCURRENTLY, assess lock impact
8. **Matview Recommendation**: LLM analyzes slow queries with repeated aggregation patterns and suggests new matviews
9. **Matview Retirement**: Detect unused matviews (zero references in pg_stat_statements) and recommend dropping
10. **Incremental Refresh Planning**: For cases where pg_ivm is available, recommend IMMV creation

**No existing tool does this.** This is greenfield opportunity.

---

## Part 5: Competitive Matrix

| Feature | Squawk | pgroll | Atlas | Bytebase | pganalyze | Datadog | Bullet | pg_ivm | **pg_sage v0.10** |
|---|---|---|---|---|---|---|---|---|---|
| DDL Safety Linting | **YES** | -- | **YES** | **YES** | -- | -- | -- | -- | **YES** |
| Runtime-Aware Safety | -- | -- | -- | -- | -- | -- | -- | -- | **YES** |
| Zero-Downtime Migration | -- | **YES** | -- | partial | -- | -- | -- | -- | planned |
| Schema Anti-Pattern Detection | partial | -- | partial | partial | partial | -- | -- | -- | **YES** |
| Index Advisor | -- | -- | -- | -- | **YES** | -- | -- | -- | **YES** |
| VACUUM Advisor | -- | -- | -- | -- | **YES** | -- | -- | -- | **YES** |
| N+1 Detection | -- | -- | -- | -- | -- | -- | **YES*** | -- | **YES** |
| Matview Management | -- | -- | -- | -- | -- | -- | -- | **YES*** | **YES** |
| LLM-Powered Analysis | -- | -- | partial | -- | -- | -- | -- | -- | **YES** |
| Automated Execution | -- | **YES** | **YES** | **YES** | -- | -- | -- | -- | **YES** |
| Open Source | YES | YES | partial | partial | -- | -- | YES | YES | **YES** |
| Database-Level (no app changes) | YES | YES | YES | YES | YES | YES | -- | YES | **YES** |

*Bullet = app-level only, Ruby only. pg_ivm = extension, limited query support.

---

## Part 6: Pricing Landscape

| Product | Model | Cost | Notes |
|---|---|---|---|
| Squawk | Free OSS | $0 | |
| pgroll | Free OSS | $0 | |
| Reshape | Free OSS | $0 | |
| Atlas | Freemium | $0 - $9/dev + $59/CI + $39/DB/month | Best analyzers require Pro |
| Bytebase | Freemium | $0 - $20/user/month | Enterprise for approval workflows |
| Flyway | Freemium | $0 - enterprise (not public) | Dry run, undo require Enterprise |
| Liquibase | Freemium | $0 - $5,000+/year | Policy checks require Pro/Secure |
| pganalyze | SaaS | $149-399/server/month | |
| Datadog DBM | SaaS | $70-84/host/month | Plus Datadog platform costs |
| pgMustard | SaaS | $110/year | |
| dryrun | Free OSS | $0 | Very early stage |
| SchemaCrawler | Free OSS | $0 | |
| pg_ivm | Free OSS | $0 | |
| **pg_sage** | **Free OSS** | **$0** | **All features included** |

---

## Part 7: Strategic Recommendations for v0.10

### 7.1 DDL Safety Advisor (High Priority)

**What to build**: Runtime-aware migration safety analysis that surpasses all static linters.

**Rules to implement** (baseline from Squawk + Atlas + Bytebase):
1. `CREATE INDEX` without `CONCURRENTLY` on tables > N rows
2. `ALTER TABLE ADD COLUMN` with `DEFAULT` and `NOT NULL` on PG < 11
3. `ALTER TABLE ADD CONSTRAINT` without `NOT VALID`
4. `ALTER TABLE ALTER COLUMN TYPE` (table rewrite)
5. `DROP COLUMN` / `DROP TABLE` (destructive)
6. Column rename (backward-incompatible)
7. Table rename (backward-incompatible)
8. Adding `UNIQUE` constraint (data-dependent -- check for duplicates first)
9. `ALTER TABLE SET NOT NULL` (full table scan required on PG < 12)
10. Foreign key without index on referencing column
11. `CLUSTER` / `REINDEX` without `CONCURRENTLY`
12. Nested transactions in migration scripts
13. Missing `statement_timeout` / `lock_timeout` in migration

**pg_sage advantage over static tools**: For each flagged DDL, we can provide:
- Actual table size and row count from catalog
- Current lock contention from pg_locks
- Estimated duration based on table size + IO throughput
- Active connections that would be blocked
- Safe alternative DDL generated by LLM
- Recommended execution window based on workload patterns
- Option to execute the safe alternative via trust-ramped executor

### 7.2 Schema Anti-Pattern Detection (High Priority)

**What to build**: Continuous schema health monitoring.

**Rules to implement** (from SchemaCrawler + dryrun + industry best practices):
1. Tables without primary keys
2. Foreign keys without indexes (SchemaCrawler rule)
3. Redundant/duplicate indexes (already in pg_sage)
4. Columns with same name but different types across tables
5. FK column type mismatch with referenced PK
6. Circular foreign key relationships
7. Tables with all nullable columns
8. SERIAL vs IDENTITY usage (prefer IDENTITY)
9. CHAR vs TEXT usage (prefer TEXT)
10. INT vs BIGINT for primary keys (prefer BIGINT)
11. timestamp vs timestamptz (prefer timestamptz)
12. Missing column comments/documentation
13. Single-column tables
14. Tables with no indexes at all
15. Nullable columns in unique indexes
16. Over-indexed tables (more indexes than columns)
17. Wide tables (> 50 columns)
18. Missing updated_at / created_at timestamps
19. ENUM anti-patterns (prefer lookup tables for > 10 values)
20. Unused columns (never referenced in pg_stat_statements queries)

**pg_sage advantage**: We can correlate schema issues with runtime impact. A missing FK index on a table with 10 rows does not matter. A missing FK index on a table with 10M rows referenced 1000x/second is critical. Static tools cannot make this distinction.

### 7.3 N+1 Query Detection (Medium-High Priority)

**What to build**: Database-level N+1 detection using pg_stat_statements.

**Detection algorithm**:
1. Query pg_stat_statements for normalized queries with high `calls` count relative to time window
2. Identify queries that differ only in parameter values (already normalized by pg_stat_statements)
3. Cross-reference with pg_stat_activity to find concurrent identical queries from same application/PID
4. Score by impact: (calls * mean_exec_time) = total time wasted
5. LLM generates the batched/joined alternative query

**Alert thresholds**:
- Same normalized query called > 100x in < 1 second from same application
- Same normalized query with `rows` = 1 called > 50x in succession
- Total time for N+1 pattern > configurable threshold (e.g., 500ms)

**This is a greenfield opportunity.** No tool detects N+1 at the database level today. Every existing solution requires application instrumentation.

### 7.4 Materialized View Intelligence (Medium Priority)

**What to build**: Full lifecycle management for materialized views.

**Features**:
1. Discovery and inventory of all matviews
2. Staleness scoring (time since last refresh vs. base table modification rate)
3. Usage tracking (which queries use each matview, how often)
4. Cost-benefit analysis (refresh cost vs. query savings)
5. Auto-refresh recommendations or execution
6. Unused matview detection and retirement recommendations
7. New matview suggestions based on repeated aggregation patterns in pg_stat_statements
8. CONCURRENTLY safety checks (unique index verification)
9. Refresh failure alerting and retry

---

## Part 8: Key Differentiators Summary

**Why pg_sage wins in each category**:

| Category | Static Tools (Squawk/Atlas/dryrun) | SaaS Monitors (pganalyze/Datadog) | App-Level (Bullet/Scout) | **pg_sage** |
|---|---|---|---|---|
| Knows table size | NO | YES | NO | **YES** |
| Knows current load | NO | YES | NO | **YES** |
| Knows lock state | NO | partial | NO | **YES** |
| Can execute fixes | NO | NO | NO | **YES** |
| LLM intelligence | NO | NO | NO | **YES** |
| Language agnostic | YES | YES | NO | **YES** |
| No app changes | YES | YES | NO | **YES** |
| Open source | mostly | NO | mostly | **YES** |
| Continuous monitoring | NO | YES | NO | **YES** |
| Covers all 4 categories | NO (1-2 max) | NO (1-2 max) | NO (1 only) | **YES** |

**The fundamental insight**: Every competitor is stuck in one quadrant. Linters know syntax but not runtime. Monitors know metrics but cannot act. App tools know code but not databases. pg_sage operates across all quadrants because it has runtime access AND LLM intelligence AND an executor with a trust ramp.
