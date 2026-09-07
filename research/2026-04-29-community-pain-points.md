# pg_sage Community Pain Points Research

Research note for: 2026-04-29  
Scope: PostgreSQL operations pain points surfaced in public community discussions, Q&A, issue trackers, mailing lists/forums, and community/vendor blogs.  
Constraint: no source code changes.

## Executive Summary

Postgres operators are not short on raw metrics. They are short on confident, contextual answers: which query, table, index, connection pattern, vacuum blocker, replica, cloud limit, or vector-search query shape is causing user-visible pain, and what action is safe to take next. Across Reddit, Stack Overflow, PostgreSQL mailing lists, GitHub issues, and community blogs, the same pattern repeats: users can often identify that "Postgres is slow," but they struggle to separate missing indexes from stale statistics, autovacuum from bloat, connection pileups from CPU/memory pressure, plan changes from prepared statement behavior, and managed-cloud behavior from core PostgreSQL behavior.

The best opportunity for pg_sage is an operations advisor that correlates pg_stat_statements, pg_stat_activity, table/index bloat signals, wait events, connection counts, replica lag, provider constraints, and explain plans into a ranked set of root-cause hypotheses with blast-radius-aware remediations.

## Key Takeaways

1. **Slow query diagnosis is still too manual.** Reddit users ask where to start when "Postgres is so slow," and answers converge on logs, pg_stat_statements, EXPLAIN, bloat checks, indexes, and hardware sanity checks rather than one integrated workflow. That is a strong opening for guided triage. [Reddit: slow Postgres thread](https://www.reddit.com/r/PostgreSQL/comments/1otk9ht/where_would_you_start_on_why_postgres_is_so_slow/), [pgDash: slow query limitations](https://pgdash.io/blog/slow-queries-postgres.html)
2. **Index selection pain is about confidence, not just recommendations.** Stack Overflow examples show huge latency swings from wrong index choices, LIMIT/ORDER BY behavior, and cost misestimates. Operators need "why this index" plus write overhead and risk, not a blind list of CREATE INDEX statements. [SO: wrong index plan](https://stackoverflow.com/questions/28740639/postgres-uses-wrong-index-in-query-plan), [SO: wrong index scan cost estimation](https://stackoverflow.com/questions/38256551/postgresql-wrong-index-scan-cost-estimation-leads-to-wrong-plan), [pganalyze planner/index advisor](https://pganalyze.com/blog/deconstructing-the-postgres-planner)
3. **Plan regressions and prepared-statement generic plan flips are painful and under-observed.** A 2025 PostgreSQL bug-list report shows a query running fast for the first five executions, then switching to a much slower generic plan. pg_stat_statements historically did not expose enough plan-type context, which makes this a high-value pg_sage alert class. [PostgreSQL BUG #19076](https://www.postgresql.org/message-id/19076-4d897c04a44a9627%40postgresql.org), [pgconsole on pg_stat_statements plan counters](https://www.pgconsole.com/blog/postgres-19-feature-preview-pg-stat-statements)
4. **Autovacuum and bloat are both common and misunderstood.** Public threads show people with 200GB databases shrinking to 2GB after VACUUM FULL, constant autovacuum after large deletes, and confusion about whether vacuum should return disk to the OS. pg_sage can win by explaining dead tuples, xmin blockers, TOAST/index bloat, and safe cleanup options. [PostgreSQL mailing-list bloat thread](https://www.postgresql.org/message-id/flat/CANzqJaD8w4YzwDWBhSBxGt6R-7TxuYV5g%3DHeB1HUe%3DxEsbjr2g%40mail.gmail.com), [SO: slow after deleting 500M rows](https://stackoverflow.com/questions/75838197/postgres-very-slow-queries-after-deleting-500-million-rows), [Crunchy Data bloat guide](https://www.crunchydata.com/blog/checking-for-postgresql-bloat), [pganalyze bloat guide](https://pganalyze.com/docs/vacuum-advisor/what-is-bloat)
5. **Connection storms are a mainstream operational failure mode.** Community discussions repeatedly show confusion over app pool sizes, serverless bursts, active vs idle connections, and whether max_connections should be raised. pg_sage should turn connection pressure into concrete pooler and workload-shaping guidance. [Reddit: pool size vs requests](https://www.reddit.com/r/PostgreSQL/comments/17ve883/what_is_the_relation_between_connection_pool_size/), [Reddit: Cloud SQL connection surge](https://www.reddit.com/r/googlecloud/comments/1rvyy31/can_cloud_sql_postgres_handle_sudden_connection/), [cr0x connection storms](https://cr0x.net/en/postgresql-connection-storms-pooler-vs-tuning/)
6. **Managed Postgres adds a second diagnostic layer.** RDS/Aurora/Cloud SQL users hit extension availability, support visibility, dashboard ambiguity, restarts, pooler limitations, and provider-specific connection behavior. pg_sage should encode provider-aware checks rather than giving generic Postgres advice. [AWS bloat/pg_repack guide](https://aws.amazon.com/blogs/database/remove-bloat-from-amazon-aurora-and-rds-for-postgresql-with-pg_repack/), [Cloud SQL managed pooling docs](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling), [SO: RDS hangs/timeouts](https://stackoverflow.com/questions/74448644/aws-postgres-instance-hangs-timeouts-for-no-good-reason), [SO: Aurora too many connections](https://stackoverflow.com/questions/79916206/aws-aurora-too-many-connections)
7. **pg_stat_statements is essential but incomplete alone.** It is widely recommended, yet users complain it is cumulative, lacks bind parameter values, can be hard to correlate with waits, and has exporter/queryid/version pitfalls. pg_sage should combine pg_stat_statements deltas with sampled activity, waits, plan snapshots, and source attribution. [Reddit: historical query execution times](https://www.reddit.com/r/PostgreSQL/comments/1c0oe50/how_to_see_historical_actual_query_execution/), [pgDash: slow query discovery](https://pgdash.io/blog/slow-queries-postgres.html), [postgres_exporter issue #1052](https://github.com/prometheus-community/postgres_exporter/issues/1052), [pgconsole pg_stat_statements scaling](https://www.pgconsole.com/blog/postgres-19-feature-preview-pg-stat-statements)
8. **HA and replication pain is a mix of metrics, architecture, and risk language.** Operators ask how to balance read queries on standbys against conflicts and acceptable lag. They need RPO/RTO framing, not just byte-lag metrics. [SO: hot standby conflicts and lag](https://stackoverflow.com/questions/57596445/manage-conflicts-and-lag-on-postgres-replication-in-hot-standby-with-read-heavy), [pghealth replication lag guide](https://pghealth.io/blog/postgresql-replication-lag-detection-root-cause-fix), [PostgreSQL mailing list: replication lag/RPO](https://www.postgresql.org/message-id/CAJzgB-FgxbU15L3iyu84B8ZJdOvqfY9HM4s2Qc27f74%3Da43MKA%40mail.gmail.com)
9. **Vector search on Postgres has new, sharp operational edges.** pgvector users struggle with HNSW/IVFFlat selection, filters applied after ANN scans, index build memory/time, insert cost, and query shapes that unexpectedly bypass indexes. This is a differentiated pg_sage wedge as AI workloads move into Postgres. [pgvector README filtering guidance](https://github.com/pgvector/pgvector), [pgvector issue #721](https://github.com/pgvector/pgvector/issues/721), [SO: pgvector too slow](https://stackoverflow.com/questions/79610767/pgvector-similarity-search-way-to-slow), [Reddit: filtered vector queries](https://www.reddit.com/r/PostgreSQL/comments/1ooeduv/optimizing_filtered_vector_queries_from_tens_of/), [Neon/DEV: HNSW build time and memory](https://dev.to/neon-postgres/pgvector-30x-faster-index-build-for-your-vector-embeddings-46da)
10. **The recurring buying trigger is incident fatigue.** Users describe timeouts, OOM chains, misleading dashboards, locks, sudden 300x slowdowns, and "no obvious metric" situations. pg_sage should package findings as incident-ready explanations, not just dashboards.

## Evidence By Pain Area

### 1. Slow Queries and First-Response Triage

Community discussions show that "slow Postgres" usually starts as a vague symptom. The practical advice from community members is fragmented: check whether the database is really the bottleneck, enable slow query logging, install pg_stat_statements, inspect EXPLAIN/ANALYZE, look for bloat, and verify hardware/IO health. The problem is not lack of available tools; it is that the workflow is scattered and assumes DBA fluency.

The pgDash slow-query writeup captures a core product gap: pg_stat_statements is invaluable, but it is cumulative and needs periodic snapshots to catch regressions during a time window. It also does not capture bind parameter values, which can matter when the same normalized query is fast for one parameter and slow for another. [pgDash](https://pgdash.io/blog/slow-queries-postgres.html)

**pg_sage opportunity:** provide an incident entrypoint: "why is Postgres slow right now?" It should rank query CPU, IO, lock waits, temp files, stale stats, bloat, connection pressure, and cloud limits, then link each hypothesis to evidence.

### 2. Index Selection, Missing Indexes, and Write-Cost Tradeoffs

Stack Overflow is full of examples where Postgres picks an unexpected index or plan. In one RDS-hosted case, two nearly identical queries differed by LIMIT and produced a 300x performance difference, with the answer pointing toward a composite index. [SO](https://stackoverflow.com/questions/28740639/postgres-uses-wrong-index-in-query-plan) Another case argues that wrong index-scan cost estimation can make a cold-data query run over an hour while a rewritten version finishes much faster. [SO](https://stackoverflow.com/questions/38256551/postgresql-wrong-index-scan-cost-estimation-leads-to-wrong-plan)

pganalyze's planner/index-advisor writeup is useful because it acknowledges the hard part: planner selectivity is hidden, statistics drive estimates, and creating every possible suggested index slows writes. [pganalyze](https://pganalyze.com/blog/deconstructing-the-postgres-planner)

**pg_sage opportunity:** recommend indexes with a confidence score, estimated read benefit, write overhead, storage overhead, lock/concurrent-build risk, and workload coverage from pg_stat_statements.

### 3. Plan Regressions and Generic Plan Flips

A PostgreSQL bug report from October 2025 describes a prepared statement that is fast for the first five executions, then uses a generic plan that is dramatically slower. The custom plan sample is around 100 ms, while the generic plan sample is around 7.1 seconds. [PostgreSQL BUG #19076](https://www.postgresql.org/message-id/19076-4d897c04a44a9627%40postgresql.org)

The pgconsole Postgres 19 preview frames this as an observability gap: generic/custom plan counters are being added because plan flipping has historically been hard to diagnose from pg_stat_statements alone. [pgconsole](https://www.pgconsole.com/blog/postgres-19-feature-preview-pg-stat-statements)

**pg_sage opportunity:** detect plan instability by comparing queryid-level latency distributions, plan hashes, generic/custom plan behavior where available, and EXPLAIN samples. Suggest safe mitigations such as ANALYZE/statistics changes, query rewrites, or plan_cache_mode guidance with warnings.

### 4. Autovacuum, Dead Tuples, TOAST, and Bloat

The PostgreSQL mailing-list thread on autovacuum, dead tuples, and bloat is a vivid example: a user reports a database at 200GB with 99% bloat that drops to 2GB after VACUUM FULL, then asks how to verify whether dead tuples are reusable. The follow-up discussion points to pg_stat_all_tables, vacuum/autovacuum/analyze fields, logs, long-running transactions, and TOAST-table behavior. [PostgreSQL mailing list](https://www.postgresql.org/message-id/flat/CANzqJaD8w4YzwDWBhSBxGt6R-7TxuYV5g%3DHeB1HUe%3DxEsbjr2g%40mail.gmail.com)

A Stack Overflow case after deleting 500 million rows shows the same operational tension: autovacuum seems to run constantly, VACUUM FULL would cause downtime, and the answer discusses throttling, autovacuum_work_mem, competing locks, manual vacuum, tuple movement, and REINDEX CONCURRENTLY. [SO](https://stackoverflow.com/questions/75838197/postgres-very-slow-queries-after-deleting-500-million-rows)

Community blogs reinforce the gap. Crunchy Data explains that VACUUM usually marks space reusable rather than returning it to the OS, and that VACUUM FULL rewrites and locks the table. [Crunchy Data](https://www.crunchydata.com/blog/checking-for-postgresql-bloat) pganalyze emphasizes xmin horizon blockers, insufficient or inefficient vacuum, and the need to watch bloat trends. [pganalyze](https://pganalyze.com/docs/vacuum-advisor/what-is-bloat)

**pg_sage opportunity:** turn bloat into an actionable table/index queue: current bloat estimate, dead tuple trend, last vacuum/analyze, xmin blockers, autovacuum settings, lock risks, storage pressure, and recommended action: tune autovacuum, run manual VACUUM, REINDEX CONCURRENTLY, pg_repack, or schedule a heavier rewrite.

### 5. Connection Storms, Pooling, and Serverless Bursts

Connection management is a strong pain theme. A Reddit thread asks whether web request scale implies needing 1K-4K database connections; responses explain that too many connections relative to cores wastes resources and recommend PgBouncer for high connection counts. [Reddit](https://www.reddit.com/r/PostgreSQL/comments/17ve883/what_is_the_relation_between_connection_pool_size/)

A recent Cloud SQL thread is even closer to an incident: 1K-2K Cloud Functions hit PostgreSQL, managed connection pooling is enabled, dashboards show confusing client/server connection metrics, simple queries hang for nine minutes, and the user considers leaving Postgres. [Reddit](https://www.reddit.com/r/googlecloud/comments/1rvyy31/can_cloud_sql_postgres_handle_sudden_connection/)

The cr0x connection-storm guide defines the failure mode as rate of new connections or reconnects overwhelming process creation, auth, TLS, CPU scheduling, kernel limits, or context switching. [cr0x](https://cr0x.net/en/postgresql-connection-storms-pooler-vs-tuning/)

**pg_sage opportunity:** detect connection storms and pooler misconfiguration by correlating connection creation rate, active vs idle sessions, wait events, backend start latency, max_connections, CPU cores, memory, app names, and provider-specific pool settings. Provide guidance for PgBouncer/RDS Proxy/Cloud SQL MCP and for queueing bursty writes.

### 6. Managed Postgres: RDS, Aurora, and Cloud SQL Friction

Managed Postgres shifts pain from "how do I configure Postgres?" to "which parts can I configure, which extensions exist, and what does the provider dashboard actually mean?" AWS's bloat guide for RDS/Aurora documents pg_repack prerequisites and warns that VACUUM, VACUUM FULL, and pg_repack are I/O intensive. [AWS](https://aws.amazon.com/blogs/database/remove-bloat-from-amazon-aurora-and-rds-for-postgresql-with-pg_repack/)

Cloud SQL's managed connection pooling documentation shows both usefulness and caveats: it targets sudden connection spikes, but it has requirements, restarts existing instances when enabled, unsupported SQL features in transaction pooling, prepared-statement caveats, client IP tracking limitations, and max_connections planning guidance. [Google Cloud](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)

Stack Overflow managed-cloud threads expose the user's side of this friction: RDS hangs/timeouts with little obvious dashboard evidence, Aurora connections rising under load, and recommendations to use RDS Proxy for sudden spikes. [SO RDS](https://stackoverflow.com/questions/74448644/aws-postgres-instance-hangs-timeouts-for-no-good-reason), [SO Aurora](https://stackoverflow.com/questions/79916206/aws-aurora-too-many-connections)

**pg_sage opportunity:** provider-aware runbooks: detect when advice is blocked or altered by RDS/Aurora/Cloud SQL permissions, extensions, parameter groups, maintenance restarts, poolers, logging retention, or monitoring granularity.

### 7. pg_stat_statements and Observability Gaps

pg_stat_statements is everywhere, but the community keeps bumping into its boundaries. Reddit users want historical actual execution times without third-party tools and ask how to separate waiting from execution. The suggested answer is to sample pg_stat_activity and combine snapshots with query IDs. [Reddit](https://www.reddit.com/r/PostgreSQL/comments/1c0oe50/how_to_see_historical_actual_query_execution/)

The postgres_exporter issue tracker shows setup-level friction: a user can query pg_stat_statements directly, but exporter collection returns HTTP 500. [GitHub issue #1052](https://github.com/prometheus-community/postgres_exporter/issues/1052) pgconsole highlights pg_stat_statements entry bloat and lack of generic/custom plan visibility before newer changes. [pgconsole](https://www.pgconsole.com/blog/postgres-19-feature-preview-pg-stat-statements)

**pg_sage opportunity:** treat pg_stat_statements as one input, not the whole answer. Store deltas, preserve plan/query context, correlate waits and blockers, identify reset/eviction anomalies, and support version-specific column differences.

### 8. HA, Replication Lag, and Failover Risk

A Stack Overflow user running Google Cloud PostgreSQL with a read-heavy standby asks how to avoid hot-standby conflict errors while keeping replication lag to a few seconds. Their attempts trade conflict errors for lag measured in hundreds or thousands of seconds. [SO](https://stackoverflow.com/questions/57596445/manage-conflicts-and-lag-on-postgres-replication-in-hot-standby-with-read-heavy)

pghealth's guide frames lag as an HA risk, not a cosmetic metric: replay lag affects data visibility, failover can lose unapplied WAL, WAL retention can grow, and reporting queries can compete with WAL replay. [pghealth](https://pghealth.io/blog/postgresql-replication-lag-detection-root-cause-fix)

**pg_sage opportunity:** explain replication health in RPO/RTO language: write/flush/replay lag, byte lag, WAL generation rate, slot lag, conflicts, standby IO saturation, and whether read traffic belongs on physical replicas or a separate reporting path.

### 9. Vector Search: pgvector, HNSW, Filtering, and Build Costs

Postgres vector search is a fast-growing pain cluster. The pgvector README documents the central surprise: with approximate indexes, filtering happens after the index scan; if a filter matches 10% of rows and ef_search is 40, only a few rows may match unless the query/index strategy changes. It suggests raising ef_search, iterative scans, partial indexes, and partitioning. [pgvector README](https://github.com/pgvector/pgvector)

A pgvector GitHub issue reports HNSW being bypassed when LIMIT or filter selectivity crosses a threshold, with execution time around 3.4 seconds without the vector index versus around 64 ms with it in a lower-LIMIT example. [GitHub issue #721](https://github.com/pgvector/pgvector/issues/721)

Stack Overflow and Reddit show the user pain: similarity search taking seconds or tens of seconds, filters scanning too many rows, unpredictable cloud compute performance, and a production writeup about filtered vector queries going from tens of seconds to single-digit milliseconds after query/index restructuring. [SO: pgvector slow](https://stackoverflow.com/questions/79610767/pgvector-similarity-search-way-to-slow), [SO: unpredictable pgvector performance](https://stackoverflow.com/questions/78793856/unpredictable-bad-performance-of-vector-similarity-search-in-postgres-database), [Reddit](https://www.reddit.com/r/PostgreSQL/comments/1ooeduv/optimizing_filtered_vector_queries_from_tens_of/)

HNSW operational costs are also visible. The pgvector README notes HNSW has slower build times and uses more memory than IVFFlat. [pgvector README](https://github.com/pgvector/pgvector) A Neon/DEV post describes HNSW's memory and build-time pain and says million-row HNSW builds can take hours without parallel build improvements. [Neon/DEV](https://dev.to/neon-postgres/pgvector-30x-faster-index-build-for-your-vector-embeddings-46da) X/Twitter was only partially accessible in browser/search, but an accessible Supabase X result around pgvector 0.6.0 promoted "30x faster" parallel index builds, which is consistent with the broader index-build-time signal. [Supabase on X](https://x.com/supabase/status/1752415014244507858)

**pg_sage opportunity:** build a pgvector advisor: explain whether an ANN index is used, why it was bypassed, whether filters are pre/post-index, what ef_search/probes/iterative_scan should be tried, when partial indexes or partitioning help, and what memory/build-time/write amplification tradeoffs apply.

## Potential pg_sage Features Suggested By The Research

1. **Incident Triage Mode:** one command/dashboard answer for "why is Postgres slow now?" with ranked hypotheses and evidence.
2. **Plan Regression Watcher:** queryid + plan hash + latency distribution + generic/custom plan flip detection.
3. **Workload-Aware Index Advisor:** read benefit, write cost, storage cost, lock risk, duplicate/unused-index cleanup, and confidence.
4. **Autovacuum/Bloat Advisor:** table/index/TOAST bloat, xmin blockers, vacuum progress, autovacuum settings, and safe cleanup paths.
5. **Connection Storm Advisor:** connection creation rate, active/idle split, backend pressure, pool sizing, provider pooler settings, and burst dampening.
6. **Managed Cloud Runbooks:** RDS/Aurora/Cloud SQL-specific capability checks, extension availability, restart requirements, logging retention, and monitoring caveats.
7. **pg_stat_statements Enhancer:** delta storage, reset/eviction detection, wait correlation, query-source mapping, bind-sensitive warnings, and exporter setup checks.
8. **Replication Risk Monitor:** lag in RPO/RTO terms, conflict counters, standby query pressure, WAL retention, slot bloat, and failover readiness.
9. **pgvector Advisor:** ANN index usage, filter selectivity, HNSW/IVFFlat tuning, index build resource planning, recall/latency tradeoffs, and query rewrite suggestions.
10. **Safety-Aware Remediation Planner:** distinguishes advisory vs safe automatic actions, flags locks/restarts/provider limits, and prefers reversible or concurrent operations where possible.

## Source Catalog

### Reddit

- [PostgreSQL pain points in real world](https://www.reddit.com/r/PostgreSQL/comments/1kvkkro/postgresql_pain_points_in_real_world/)
- [Where would you start on why Postgres is so slow?](https://www.reddit.com/r/PostgreSQL/comments/1otk9ht/where_would_you_start_on_why_postgres_is_so_slow/)
- [What is the relation between connection pool size and number of web requests?](https://www.reddit.com/r/PostgreSQL/comments/17ve883/what_is_the_relation_between_connection_pool_size/)
- [Can Cloud SQL (Postgres) handle sudden connection surge?](https://www.reddit.com/r/googlecloud/comments/1rvyy31/can_cloud_sql_postgres_handle_sudden_connection/)
- [How to see historical actual query execution times without third-party tools](https://www.reddit.com/r/PostgreSQL/comments/1c0oe50/how_to_see_historical_actual_query_execution/)
- [Optimizing filtered vector queries from tens of seconds to single-digit milliseconds in PostgreSQL](https://www.reddit.com/r/PostgreSQL/comments/1ooeduv/optimizing_filtered_vector_queries_from_tens_of/)

### Stack Overflow

- [Postgres uses wrong index in query plan](https://stackoverflow.com/questions/28740639/postgres-uses-wrong-index-in-query-plan)
- [PostgreSQL wrong index scan cost estimation leads to wrong plan](https://stackoverflow.com/questions/38256551/postgresql-wrong-index-scan-cost-estimation-leads-to-wrong-plan)
- [Postgres very slow queries after deleting 500 million rows](https://stackoverflow.com/questions/75838197/postgres-very-slow-queries-after-deleting-500-million-rows)
- [AWS Postgres instance hangs/timeouts for no good reason](https://stackoverflow.com/questions/74448644/aws-postgres-instance-hangs-timeouts-for-no-good-reason)
- [AWS Aurora: too many connections](https://stackoverflow.com/questions/79916206/aws-aurora-too-many-connections)
- [Manage conflicts and lag on Postgres replication in hot standby](https://stackoverflow.com/questions/57596445/manage-conflicts-and-lag-on-postgres-replication-in-hot-standby-with-read-heavy)
- [Pgvector similarity search way too slow](https://stackoverflow.com/questions/79610767/pgvector-similarity-search-way-to-slow)
- [Unpredictable bad performance of vector similarity search with pgvector](https://stackoverflow.com/questions/78793856/unpredictable-bad-performance-of-vector-similarity-search-in-postgres-database)

### PostgreSQL Mailing Lists / Forums

- [BUG #19076: Generic query plan is extremely slow](https://www.postgresql.org/message-id/19076-4d897c04a44a9627%40postgresql.org)
- [Autovacuum, dead tuples and bloat thread](https://www.postgresql.org/message-id/flat/CANzqJaD8w4YzwDWBhSBxGt6R-7TxuYV5g%3DHeB1HUe%3DxEsbjr2g%40mail.gmail.com)
- [Replication lag in Postgres / RPO discussion](https://www.postgresql.org/message-id/CAJzgB-FgxbU15L3iyu84B8ZJdOvqfY9HM4s2Qc27f74%3Da43MKA%40mail.gmail.com)

### GitHub Issues / Repositories

- [pgvector README](https://github.com/pgvector/pgvector)
- [pgvector issue #721: HNSW index bypassed when LIMIT/filter selectivity exceeds threshold](https://github.com/pgvector/pgvector/issues/721)
- [pgvector issue #409: Parallel index builds for HNSW](https://github.com/pgvector/pgvector/issues/409)
- [postgres_exporter issue #1052: pg_stat_statements metrics collection issue](https://github.com/prometheus-community/postgres_exporter/issues/1052)

### Community / Vendor Blogs and Docs

- [pganalyze: What is Bloat?](https://pganalyze.com/docs/vacuum-advisor/what-is-bloat)
- [pganalyze: Deconstructing the Postgres planner to find indexing opportunities](https://pganalyze.com/blog/deconstructing-the-postgres-planner)
- [Crunchy Data: Checking for PostgreSQL Bloat](https://www.crunchydata.com/blog/checking-for-postgresql-bloat)
- [AWS: Remove bloat from Amazon Aurora and RDS for PostgreSQL with pg_repack](https://aws.amazon.com/blogs/database/remove-bloat-from-amazon-aurora-and-rds-for-postgresql-with-pg_repack/)
- [Google Cloud: Cloud SQL managed connection pooling](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)
- [pgDash: Dealing With Slow Queries With PostgreSQL](https://pgdash.io/blog/slow-queries-postgres.html)
- [pgconsole: Postgres 19 pg_stat_statements normalization and plan counters](https://www.pgconsole.com/blog/postgres-19-feature-preview-pg-stat-statements)
- [pghealth: PostgreSQL replication lag detection, root cause and fix](https://pghealth.io/blog/postgresql-replication-lag-detection-root-cause-fix)
- [cr0x: PostgreSQL connection storms](https://cr0x.net/en/postgresql-connection-storms-pooler-vs-tuning/)
- [Neon/DEV: pgvector 30x faster index build](https://dev.to/neon-postgres/pgvector-30x-faster-index-build-for-your-vector-embeddings-46da)
- [Supabase on X: pgvector 0.6.0 parallel index builds](https://x.com/supabase/status/1752415014244507858)
