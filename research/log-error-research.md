# PostgreSQL Log Error Research: Signal Catalog Gap Analysis

> **Date**: 2026-04-13
> **Purpose**: Identify production-impactful PostgreSQL log errors NOT covered by
> the current 10-signal catalog in v0.9.1-log-based-rca.md.
> **Method**: Web search across Percona, pganalyze, Crunchy Data, Severalnines,
> AWS docs, PostgreSQL mailing lists, Medium, BetterStack, and community forums.

---

## Current Catalog (10 signals)

| # | Signal ID | What It Covers |
|---|-----------|---------------|
| 1 | `log_deadlock_detected` | SQLState 40P01 |
| 2 | `log_temp_file_created` | Sort/hash spill > 10MB |
| 3 | `log_checkpoint_too_frequent` | Checkpoint pressure |
| 4 | `log_connection_refused` | SQLState 53300 (max_connections) |
| 5 | `log_out_of_memory` | SQLState 53200 |
| 6 | `log_archive_failed` | WAL archive_command failure |
| 7 | `log_lock_timeout` | SQLState 55P03 |
| 8 | `log_statement_timeout` | SQLState 57014 |
| 9 | `log_replication_slot_inactive` | Slot lag warning |
| 10 | `log_autovacuum_cancel` | Autovacuum canceled by lock conflict |

---

## Proposed New Signals (Gaps Found)

### 11. `log_txid_wraparound_warning`

- **Error Pattern**: `WARNING: database "{db}" must be vacuumed within {n} transactions` / `ERROR: database is not accepting commands to avoid wraparound data loss in database "{db}"`
- **SQLState**: n/a (WARNING/ERROR level message)
- **Why It Matters**: Transaction ID wraparound is one of the most catastrophic PostgreSQL failures. At 99.85% XID utilization (3M transactions remaining), PostgreSQL shuts down entirely and refuses all writes. Multiple production outage postmortems document this as a top-5 cause of PostgreSQL downtime. Anti-wraparound autovacuum holds SHARE UPDATE EXCLUSIVE locks that block DDL, compounding the problem.
- **Suggested Severity**: critical
- **Actionability**: Immediate VACUUM FREEZE on the affected database. Monitor `age(datfrozenxid)` and `age(datminmxid)`.
- **Sources**:
  - [Managing Transaction ID Wraparound (Crunchy Data)](https://www.crunchydata.com/blog/managing-transaction-id-wraparound-in-postgresql)
  - [How to fix transaction wraparound (Fastware)](https://www.postgresql.fastware.com/blog/how-to-fix-transaction-wraparound-in-postgresql)
  - [Downtime Caused by Postgres Transaction ID Wraparound (SQLServerCentral)](https://www.sqlservercentral.com/articles/i-too-have-a-production-story-a-downtime-caused-by-postgres-transaction-id-wraparound-problem)
  - [VACUUM: Freezing - Approaching Transaction ID Wraparound (pganalyze)](https://pganalyze.com/docs/checks/vacuum/txid_wraparound)
  - [Autovacuum wraparound protection (Cybertec)](https://www.cybertec-postgresql.com/en/autovacuum-wraparound-protection-in-postgresql/)

### 12. `log_data_corruption`

- **Error Pattern**: `WARNING: invalid page in block {N} of relation {path}` / `ERROR: invalid page in block {N} of relation {path}` / `WARNING: page verification failed, calculated checksum {X} but expected {Y}`
- **SQLState**: `XX001` (data_corrupted) / `XX002` (index_corrupted)
- **Why It Matters**: Data corruption is the highest-severity event a DBA can encounter. With checksums enabled (PG12+ default on many distributions), PostgreSQL detects corruption at read time. Without checksums, you may get silently wrong data. pganalyze classifies this as their S6 server event category. Recovery options are limited: restore from backup, use `zero_damaged_pages` as a last resort, or attempt PITR.
- **Suggested Severity**: critical (highest possible)
- **Actionability**: Identify affected table/index via file path OID mapping. Check for hardware issues (disk, memory). Restore from backup if needed. Enable `ignore_checksum_failure` temporarily to salvage data.
- **Sources**:
  - [S6: Data corrupted (pganalyze)](https://pganalyze.com/docs/log-insights/server/S6)
  - [PostgreSQL Invalid Page and Checksum Verification Failed (Ardent Performance)](https://ardentperf.com/2019/11/08/postgresql-invalid-page-and-checksum-verification-failed/)
  - [Troubleshooting PostgreSQL Page Corruption (MinervaDB)](https://minervadb.xyz/troubleshooting-postgresql-page-corruption/)
  - [PostgreSQL Checksum and Data Corruption Issues (postgreshelp)](https://postgreshelp.com/postgresql-checksum/)

### 13. `log_panic_server_crash`

- **Error Pattern**: `PANIC: could not write to log file` / `PANIC: could not open file` / `LOG: server process (PID {N}) was terminated by signal {S}` / `LOG: terminating any other active server processes` / `LOG: all server processes terminated; reinitializing`
- **SQLState**: n/a (PANIC level, system-wide)
- **Why It Matters**: A PANIC event shuts down the entire PostgreSQL cluster. All sessions are terminated. The system enters crash recovery on restart. Common causes include disk full in pg_wal, filesystem errors, extension bugs, and OOM killer (SIGKILL). pganalyze tracks this as S1 (server process terminated) and S3 (database system interrupted). Every crash is an outage event that should generate an immediate alert.
- **Suggested Severity**: critical (highest possible)
- **Actionability**: Check dmesg/syslog for OOM killer events. Verify disk space on data and WAL partitions. Review extension list. Check for hardware errors.
- **Sources**:
  - [S1: Server process was terminated (pganalyze)](https://pganalyze.com/docs/log-insights/server/S1)
  - [Terminating connection because of crash of another server process (Percona Forums)](https://forums.percona.com/t/terminating-connection-because-of-crash-of-another-server-process/21827)
  - [8 Steps to Handle PostgreSQL Disaster Recovery (pgEdge)](https://www.pgedge.com/blog/8-steps-to-proactively-handle-postgresql-database-disaster-recovery)

### 14. `log_disk_full`

- **Error Pattern**: `ERROR: could not extend file "{path}": No space left on device` / `PANIC: could not write to file "{path}": No space left on device` / `ERROR: could not resize shared memory segment "{name}" to {N} bytes: No space left on device`
- **SQLState**: `53100` (disk_full)
- **Why It Matters**: Disk exhaustion is consistently cited as a top-3 production PostgreSQL issue across all sources. It can manifest in data directory (base/), WAL directory (pg_wal/), temp directory (pgsql_tmp/), or shared memory (/dev/shm). A full pg_wal directory causes a PANIC and immediate shutdown. The shared memory variant is especially common in Docker containers with default 64MB /dev/shm. On AWS RDS, this is one of the most common support cases.
- **Suggested Severity**: critical
- **Actionability**: Identify which filesystem is full. For pg_wal: check archive_command status, replication slot lag, and WAL retention settings. For base/: run VACUUM FULL or delete old data. For /dev/shm: increase Docker --shm-size or reduce parallel workers.
- **Sources**:
  - [Postgres is Out of Disk and How to Recover (Crunchy Data)](https://www.crunchydata.com/blog/postgres-is-out-of-disk-and-how-to-recover-the-dos-and-donts)
  - [How to solve the problem if pg_wal is full (Fastware)](https://www.postgresql.fastware.com/blog/how-to-solve-the-problem-if-pg-wal-is-full)
  - [Resolve DiskFull errors on Amazon RDS for PostgreSQL (AWS)](https://repost.aws/knowledge-center/diskfull-error-rds-postgresql)
  - [Postgres Troubleshooting - DiskFull (Crunchy Data)](https://www.crunchydata.com/blog/postgres-troubleshooting-diskfull-error-could-not-resize-shared-memory-segment)
  - [How to Fix PQ Could Not Resize Shared Memory (SigNoz)](https://signoz.io/guides/pq-could-not-resize-shared-memory-segment-no-space-left-on-device/)

### 15. `log_replication_conflict`

- **Error Pattern**: `ERROR: canceling statement due to conflict with recovery` / `FATAL: terminating connection due to conflict with recovery` / `LOG: recovery still waiting after {N}ms`
- **SQLState**: `40001` (serialization_failure) on standby
- **Why It Matters**: This is the most common error on hot standby replicas. When VACUUM on the primary removes rows still needed by queries on the replica, PostgreSQL cancels the standby query (or terminates the connection entirely after max_standby_streaming_delay). AWS and Azure both have dedicated troubleshooting docs for this specific error. High rates indicate either long-running queries on the replica or aggressive vacuuming on the primary.
- **Suggested Severity**: warning (escalate to critical if FATAL variant or high frequency)
- **Actionability**: Enable `hot_standby_feedback`. Increase `max_standby_streaming_delay`. Optimize long-running replica queries.
- **Sources**:
  - [Troubleshoot "canceling statement" error (AWS re:Post)](https://repost.aws/knowledge-center/rds-postgresql-error-conflict-recovery)
  - [Canceling statement due to conflict with recovery (Azure)](https://learn.microsoft.com/en-us/azure/postgresql/troubleshoot/troubleshoot-canceling-statement-due-to-conflict-with-recovery)
  - [B95: Canceling statement due to recovery conflict (pganalyze)](https://pganalyze.com/docs/log-insights/standby/B95)
  - [Troubleshooting Streaming Replication (Crunchy Data)](https://www.crunchydata.com/blog/wheres-my-replica-troubleshooting-streaming-replication-synchronization-in-postgresql)

### 16. `log_wal_segment_removed`

- **Error Pattern**: `ERROR: requested WAL segment {segment} has already been removed` / `FATAL: could not receive data from WAL stream: ERROR: requested WAL segment {segment} has already been removed`
- **SQLState**: n/a
- **Why It Matters**: This means a replica has fallen so far behind that the primary has already cleaned up the WAL segments it needs. The replica cannot recover without a full base backup. Common when wal_keep_size is too small, replication slots are not used, or the replica was down too long. This is a replication-breaking event.
- **Suggested Severity**: critical
- **Actionability**: Rebuild replica from pg_basebackup. Increase wal_keep_size or use replication slots. Investigate what caused the replica to fall behind.
- **Sources**:
  - [Troubleshooting Streaming Replication (Crunchy Data)](https://www.crunchydata.com/blog/wheres-my-replica-troubleshooting-streaming-replication-synchronization-in-postgresql)
  - [Troubleshooting Guide for WAL Replication Issues (Medium)](https://medium.com/@ShivIyer/troubleshooting-guide-for-wal-replication-issues-in-postgresql-8009226b3cf5)
  - [Streaming Replication (PostgreSQL Wiki)](https://wiki.postgresql.org/wiki/Streaming_Replication)

### 17. `log_serialization_failure`

- **Error Pattern**: `ERROR: could not serialize access due to concurrent update` / `ERROR: could not serialize access due to read/write dependencies among transactions`
- **SQLState**: `40001` (serialization_failure)
- **Why It Matters**: Serialization failures occur in REPEATABLE READ and SERIALIZABLE isolation levels. Unlike deadlocks, PostgreSQL does NOT automatically retry -- the application must. High rates of this error indicate an application bug (missing retry logic) or a workload pattern incompatible with the chosen isolation level. This error often leads to silent data loss if the application doesn't handle it correctly.
- **Suggested Severity**: warning (escalate to critical if high frequency)
- **Actionability**: Verify application implements transaction retry with exponential backoff. Consider downgrading to READ COMMITTED if serializable guarantees are not needed. Add indexes to reduce conflict surface area.
- **Sources**:
  - [U138: Could not serialize access (pganalyze)](https://pganalyze.com/docs/log-insights/app-errors/U138)
  - [PostgreSQL Serialization Failures: Beyond 'Just Retry' (Michal Drozd)](https://www.michal-drozd.com/en/blog/postgresql-serialization-failure-retry/)
  - [PostgreSQL Transactions, Locks, and Why Serializable Fails (DEV)](https://dev.to/radilov/postgresql-transactions-locks-and-why-serializable-fails-8lm)

### 18. `log_authentication_failure`

- **Error Pattern**: `FATAL: password authentication failed for user "{user}"` / `FATAL: no pg_hba.conf entry for host {ip}, user {user}, database {db}` / `FATAL: role "{role}" does not exist` / `FATAL: Peer authentication failed for user "{user}"`
- **SQLState**: `28P01` (invalid_password) / `28000` (invalid_authorization_specification)
- **Why It Matters**: Authentication failures are a security signal AND a misconfiguration signal. A sudden spike in auth failures can indicate a brute-force attack, a misconfigured application deployment, or an accidental pg_hba.conf change. PostgreSQL logs these by default. Every managed-service provider monitors this as a security event.
- **Suggested Severity**: warning (escalate to critical if high frequency, which suggests attack or mass app failure)
- **Actionability**: Check pg_hba.conf for recent changes. Verify application connection strings. If brute-force suspected, review source IPs and consider fail2ban or connection rate limiting.
- **Sources**:
  - [Decoding PostgreSQL Error Logs (Severalnines)](https://severalnines.com/blog/decoding-postgresql-error-logs/)
  - [Troubleshooting 10 of the Most Common PostgreSQL Errors (Percona)](https://www.percona.com/blog/10-common-postgresql-errors/)
  - [5 Common Connection Errors in Postgres (Tiger Data)](https://www.tigerdata.com/blog/5-common-connection-errors-in-postgresql-and-how-to-solve-them)

### 19. `log_multixact_wraparound_warning`

- **Error Pattern**: `WARNING: database "{db}" must be vacuumed within {N} multixacts` / related multixact age warnings
- **SQLState**: n/a
- **Why It Matters**: Similar to transaction ID wraparound but for MultiXact IDs. PostgreSQL uses MultiXacts for shared row locks (SELECT ... FOR SHARE). The storage area can grow up to ~20GB before wraparound, and aggressive vacuum scans are triggered when it exceeds ~10GB. If ignored, the database will refuse new MultiXact IDs. This is less well-known than XID wraparound but equally dangerous.
- **Suggested Severity**: critical
- **Actionability**: Run VACUUM on affected tables. Monitor `age(datminmxid)`. Review application use of SELECT FOR SHARE / FOR KEY SHARE.
- **Sources**:
  - [VACUUM: Freezing - Approaching Multixact ID Wraparound (pganalyze)](https://pganalyze.com/docs/checks/vacuum/mxid_wraparound)
  - [PostgreSQL MultiXactId Error in Vacuum (Medium)](https://medium.com/in-the-weeds/postgresql-multixactid-error-in-vacuum-106af8fbf022)
  - [PostgreSQL Documentation: Routine Vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html)

### 20. `log_idle_in_tx_timeout`

- **Error Pattern**: `FATAL: terminating connection due to idle-in-transaction timeout`
- **SQLState**: `25P03` (idle_in_transaction_session_timeout)
- **Why It Matters**: When `idle_in_transaction_session_timeout` is configured, PostgreSQL terminates sessions that sit idle inside an open transaction for too long. Each occurrence means an application left a transaction open and got killed. This is both a symptom (the app has a connection leak or missing commit) and a positive signal (the timeout is working). High rates indicate a systemic application bug. These terminated connections often cause cascading errors in application logs.
- **Suggested Severity**: warning
- **Actionability**: Identify the application_name and user from the log. Trace back to the application code that's not committing transactions. Correlate with connection pool exhaustion signals.
- **Sources**:
  - [How to Resolve Idle in Transaction Issues (Chat2DB)](https://chat2db.ai/resources/blog/idle-in-transaction)
  - [Simple Timeouts for Idle-in-Transaction Exhaustion (Medium)](https://demirhuseyinn-94.medium.com/simple-timeouts-have-a-powerful-impact-in-taming-idle-in-transaction-exhaustion-in-postgresql-20ca385df920)
  - [PostgreSQL idle_in_transaction_session_timeout docs](https://postgresqlco.nf/doc/en/param/idle_in_transaction_session_timeout/)

### 21. `log_slow_query`

- **Error Pattern**: `LOG: duration: {N}ms statement: {query}` (when log_min_duration_statement is set)
- **SQLState**: n/a
- **Why It Matters**: Slow query logging is the most universally recommended PostgreSQL log setting for production. Every DBA guide recommends setting `log_min_duration_statement` to 500ms-1000ms. While not an "error" per se, aggregating slow query frequency and identifying sudden spikes is critical for detecting performance regressions, missing indexes, and plan changes. This is the single most useful signal for performance-oriented RCA.
- **Suggested Severity**: info (escalate to warning if frequency spikes)
- **Actionability**: Extract the query, cross-reference with pg_stat_statements. Check for plan changes. Recommend EXPLAIN ANALYZE. Suggest index creation if sequential scans detected.
- **Sources**:
  - [Logging Tips for Postgres, Featuring Your Slow Queries (Crunchy Data)](https://www.crunchydata.com/blog/logging-tips-for-postgres-featuring-your-slow-queries)
  - [3 ways to detect slow queries in PostgreSQL (Cybertec)](https://www.cybertec-postgresql.com/en/3-ways-to-detect-slow-queries-in-postgresql/)
  - [Postgres Logging for Performance Optimization (Crunchy Data)](https://www.crunchydata.com/blog/postgres-logging-for-performance-optimization)

---

## Signals Considered but Deprioritized

These patterns are real but either too noisy, not actionable enough, or too
edge-case for the initial signal catalog.

### SSL/TLS Connection Errors

- **Pattern**: `LOG: could not accept SSL connection: ...`
- **Why Deprioritized**: Often caused by client-side issues (port scanners, health checks using non-SSL connections). Very noisy in production. The "Success" variant is an OpenSSL bug, not a real error. Not actionable by the DBA in most cases.
- **Revisit**: v0.10 could add this with a high-frequency threshold filter.

### Permission Denied / Insufficient Privilege

- **Pattern**: `ERROR: permission denied for table {name}` / `ERROR: must be owner of table {name}`
- **Why Deprioritized**: Application-level issue, not infrastructure. Usually caught during deployment testing, not in steady-state production. Would generate too much noise if monitoring all permission errors.
- **Revisit**: Could be useful as a security audit signal if scoped to DDL operations.

### Syntax Errors / Invalid Input

- **Pattern**: `ERROR: syntax error at or near "{keyword}"` / `ERROR: invalid input syntax for type {type}`
- **Why Deprioritized**: Application bugs, not DBA-actionable infrastructure issues. Every application generates some of these. No infrastructure root cause to analyze.

### Worker Process Terminated

- **Pattern**: `LOG: worker process: {name} (PID {N}) was terminated by signal {S}`
- **Why Deprioritized**: Partially covered by `log_panic_server_crash` (signal 13). Standalone worker terminations (e.g., a parallel worker killed by OOM) are worth watching but overlap with OOM detection. Could be added as a sub-signal under log_panic_server_crash.

---

## Summary: Recommended v0.9.1 Catalog Expansion

| Priority | Signal ID | Category | Impact |
|----------|-----------|----------|--------|
| **P0** | `log_txid_wraparound_warning` | Vacuum / XID | Database shutdown if ignored |
| **P0** | `log_data_corruption` | Storage | Data loss |
| **P0** | `log_panic_server_crash` | Server | Full outage |
| **P0** | `log_disk_full` | Storage | Full outage / write failure |
| **P1** | `log_replication_conflict` | Replication | Query cancellation on standby |
| **P1** | `log_wal_segment_removed` | Replication | Replica broken, needs rebuild |
| **P1** | `log_authentication_failure` | Security | Security event / mass app failure |
| **P2** | `log_serialization_failure` | Application | Silent data loss if unhandled |
| **P2** | `log_multixact_wraparound_warning` | Vacuum / MXID | Database shutdown if ignored |
| **P2** | `log_idle_in_tx_timeout` | Connections | App connection leak indicator |
| **P2** | `log_slow_query` | Performance | Performance regression detection |

**Recommendation**: Add P0 signals (11-14) immediately to the v0.9.1 spec. They
represent the most catastrophic production failures and are trivially
pattern-matchable from log lines. P1 signals (15-17) should follow in the same
release if time permits. P2 signals (18-21) can be deferred to v0.9.2 or
added opportunistically.

### Decision Tree Integration Notes

Several new signals create powerful cross-signal correlations:

- `log_disk_full` + `log_archive_failed` -> "WAL archive failure caused disk
  exhaustion in pg_wal" (very common cascade)
- `log_txid_wraparound_warning` + `log_autovacuum_cancel` -> "Autovacuum
  repeatedly canceled while XID wraparound approaches" (emergency)
- `log_panic_server_crash` + `log_out_of_memory` -> "OOM killer terminated
  PostgreSQL" (check vm.overcommit and shared_buffers sizing)
- `log_replication_conflict` + `replication_lag_increasing` (metric signal) ->
  "Replication lag causing query cancellations on standby"
- `log_authentication_failure` + `connections_high` (metric signal) ->
  "Connection exhaustion may include failed auth retries"

---

## pganalyze Log Insights Reference

For comparison, pganalyze's Log Insights tracks 50+ event categories. Their
server-level categories (the ones most relevant to pg_sage) include:

| ID | Name | Covered by pg_sage? |
|----|------|-------------------|
| S1 | Server process terminated | NEW: `log_panic_server_crash` |
| S2 | Database server starting | Not needed (informational) |
| S3 | Database system interrupted | NEW: `log_panic_server_crash` |
| S4 | Database system shutting down | Not needed (informational) |
| S5 | Out of memory | EXISTING: `log_out_of_memory` |
| S6 | Data corrupted | NEW: `log_data_corruption` |
| S7 | Temporary file created | EXISTING: `log_temp_file_created` |
| S8 | Miscellaneous server notice | Not needed (catch-all) |
| S9 | Reloading configuration files | Not needed (informational) |
| S10 | Worker process exited | Partial: under `log_panic_server_crash` |
| S11 | Stats collector not responding | Rare, deprioritized |

With the 11 new signals, pg_sage's log catalog would cover all
production-critical pganalyze server categories plus application-level errors
that pganalyze tracks separately.
