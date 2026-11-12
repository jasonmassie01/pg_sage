package collector

import "github.com/pg-sage/sidecar/internal/selfmonitor"

// Session-based snapshot inputs (perf v1.8.3, perf-selfexcl). pg_sage's
// own sessions (application_name pg_sage: its pools, probes, EXPLAINs and
// index builds) are not the workload: active and idle-in-transaction
// counts, locks, connection states and the load circuit breaker
// leave them out with selfmonitor.ActivityExclusionSQL. Connection slots
// (total_backends, client_backends) are capacity and still count every
// backend; client_backends counts the cluster's client sessions only, the
// ones max_connections limits (rca-08).

// notSelf is the session filter of these reads.
var notSelf = selfmonitor.ActivityExclusionSQL("")

// systemStatsSQLBase is the common prefix for system stats (all PG
// versions). active_backends is this database's, as idle_in_transaction
// already was (fleet mode: one database's snapshot never counts another's
// sessions). Both count client sessions only, as the connection states
// do: autovacuum workers, logical walsenders and parallel workers also
// carry the database's name and show as active, but none is a session of
// the application.
var systemStatsSQLBase = sageTag + `
SELECT
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'active' AND pid <> pg_backend_pid()
      AND backend_type = 'client backend'
      AND datname = current_database() AND ` + notSelf + `) AS active_backends,
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'idle in transaction' AND backend_type = 'client backend'
      AND datname = current_database() AND ` + notSelf + `) AS idle_in_transaction,
  (SELECT count(*) FROM pg_stat_activity
    WHERE pid <> pg_backend_pid()) AS total_backends,
  (SELECT setting::int FROM pg_settings
    WHERE name = 'max_connections') AS max_connections,
  (SELECT ` + cacheHitRatioExpr + ` FROM pg_stat_database
    WHERE datname = current_database()) AS cache_hit_ratio,
  COALESCE((SELECT deadlocks FROM pg_stat_database
    WHERE datname = current_database()), 0) AS deadlocks,
  COALESCE((SELECT blk_read_time FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blk_read_time,
  COALESCE((SELECT blk_write_time FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blk_write_time,
`

// idleInTxMaxColumn is how long this database's longest
// idle-in-transaction client session has been idle (rca-03: the longest,
// which one forgotten session sets, not the average; all PG versions).
var idleInTxMaxColumn = `
  COALESCE((SELECT max(EXTRACT(EPOCH FROM (now() - state_change)))::float8
    FROM pg_stat_activity
    WHERE state = 'idle in transaction' AND backend_type = 'client backend'
      AND datname = current_database() AND ` + notSelf + `), 0) AS idle_in_tx_max_seconds,
`

// systemCountersSQL is this database's lifetime counters the system
// interval is differenced from (collection-06; all supported versions:
// sessions is PG 14+, rca-03).
const systemCountersSQL = `
  COALESCE((SELECT blks_hit FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blks_hit,
  COALESCE((SELECT blks_read FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blks_read,
  COALESCE((SELECT xact_commit FROM pg_stat_database
    WHERE datname = current_database()), 0) AS xact_commit,
  COALESCE((SELECT xact_rollback FROM pg_stat_database
    WHERE datname = current_database()), 0) AS xact_rollback,
  COALESCE((SELECT temp_bytes FROM pg_stat_database
    WHERE datname = current_database()), 0) AS temp_bytes,
  COALESCE((SELECT sessions FROM pg_stat_database
    WHERE datname = current_database()), 0) AS sessions,
`

// checkpointEpochSQL is the checkpoint counters' epoch over the view the
// variant reads (alias v): the later of its stats_reset and the postmaster
// start (a crash discards the counters without touching stats_reset). Then
// the slots reserved from clients (forecast-08: superuser_reserved_connections
// plus reserved_connections, PG 16+, absent and so 0 before), the replica
// flag and the cluster's client sessions, every database's, as
// total_backends counts (client_backends, rca-08).
const checkpointEpochSQL = `
  GREATEST(COALESCE(v.stats_reset, '-infinity'::timestamptz),
    pg_postmaster_start_time()) AS checkpoint_stats_epoch,
  current_setting('superuser_reserved_connections')::int
    + COALESCE(current_setting('reserved_connections', true)::int, 0)
    AS reserved_connections,
  pg_is_in_recovery() AS is_replica,
  (SELECT count(*) FROM pg_stat_activity
    WHERE pid <> pg_backend_pid()
      AND backend_type = 'client backend') AS client_backends`

// The checkpoint counters are read split (forecast-03): a timed
// checkpoint is counted every checkpoint_timeout even when it is skipped
// on an idle server, so only the requested (WAL-triggered or explicit)
// count measures checkpoint pressure. total_checkpoints stays for its
// readers. checkpoints_done (num_done, performed checkpoints) is PG 18+.

// systemStatsSQL14 uses pg_stat_bgwriter (PG 14-16).
var systemStatsSQL14 = systemStatsSQLBase + systemCountersSQL + idleInTxMaxColumn + `
  v.checkpoints_timed + v.checkpoints_req AS total_checkpoints,
  v.checkpoints_timed AS checkpoints_timed,
  v.checkpoints_req AS checkpoints_requested,
  NULL::bigint AS checkpoints_done,` + checkpointEpochSQL + `
FROM pg_stat_bgwriter v`

// systemStatsSQL17 uses pg_stat_checkpointer (PG 17).
var systemStatsSQL17 = systemStatsSQLBase + systemCountersSQL + idleInTxMaxColumn + `
  v.num_timed + v.num_requested AS total_checkpoints,
  v.num_timed AS checkpoints_timed,
  v.num_requested AS checkpoints_requested,
  NULL::bigint AS checkpoints_done,` + checkpointEpochSQL + `
FROM pg_stat_checkpointer v`

// systemStatsSQL18 adds pg_stat_checkpointer.num_done (PG 18+).
var systemStatsSQL18 = systemStatsSQLBase + systemCountersSQL + idleInTxMaxColumn + `
  v.num_timed + v.num_requested AS total_checkpoints,
  v.num_timed AS checkpoints_timed,
  v.num_requested AS checkpoints_requested,
  v.num_done AS checkpoints_done,` + checkpointEpochSQL + `
FROM pg_stat_checkpointer v`

// systemStatsSQLFor picks the system stats read of a server version.
func systemStatsSQLFor(versionNum int) string {
	switch {
	case versionNum >= 180000:
		return systemStatsSQL18
	case versionNum >= 170000:
		return systemStatsSQL17
	default:
		return systemStatsSQL14
	}
}

// locksSQL is the lock wait graph of the current database (collection-01),
// bounded: pg_locks holds every lock of every session (thousands of
// AccessShareLocks on a busy OLTP server, most of all in a lock storm),
// and pg_blocking_pids takes every lock-manager partition lock, so it is
// called for at most 200 waiters. The rows are
//
//   - waiting locks (oldest wait first, at most 200), each with the pids
//     that block it, and
//   - one session row per blocker, idle-in-transaction client session and
//     the 5 client sessions holding back the xmin horizon most (age of
//     backend_xmin or backend_xid): the session's own virtualxid lock, or
//     locktype session for a blocker outside a transaction (a
//     session-level advisory lock).
//
// Ordered oldest xmin holders, blockers, waiters, then the other
// idle-in-transaction sessions, so a truncated graph keeps its roots;
// LIMIT 201 detects truncation (collectLocks keeps 200). The scope is
// G1-B17's: relation locks of this database (pg_class OIDs are
// per-database) and database-less locks of local backends. pg_sage's own
// sessions are left out (pid <> pg_backend_pid() and
// ActivityExclusionSQL); a pid of theirs can still appear in blocked_by.
// waitstart and query_id are PG 14+.
var locksSQL = sageTag + `
WITH d AS (SELECT oid FROM pg_database WHERE datname = current_database()),
act AS MATERIALIZED (
  SELECT a.pid, a.query, a.state, a.wait_event_type, a.wait_event,
         a.backend_start, a.query_start, a.xact_start, a.state_change, a.query_id,
         a.backend_type = 'client backend' AS client,
         GREATEST(age(a.backend_xmin), age(a.backend_xid))::bigint AS xmin_age
    FROM pg_stat_activity a, d
   WHERE a.datid = d.oid AND a.pid <> pg_backend_pid()
     AND ` + selfmonitor.ActivityExclusionSQL("a") + `),
lk AS MATERIALIZED (
  SELECT l.locktype, l.mode, l.granted, l.relation, l.database, l.pid, l.waitstart
    FROM pg_locks l
   WHERE NOT l.granted OR (l.locktype = 'virtualxid' AND l.mode = 'ExclusiveLock')),
waiting AS MATERIALIZED (
  SELECT l.locktype, l.mode, l.relation, l.database, l.pid, l.waitstart
    FROM lk l JOIN act a ON a.pid = l.pid, d
   WHERE NOT l.granted AND (l.database = d.oid OR l.database IS NULL)
   ORDER BY l.waitstart NULLS LAST, l.pid
   LIMIT 200),
waiters AS MATERIALIZED (
  SELECT w.*, pg_blocking_pids(w.pid) AS blocked_by FROM waiting w),
blockers AS (SELECT DISTINCT b.pid FROM waiters w, unnest(w.blocked_by) AS b(pid)),
oldest AS (
  SELECT pid FROM act WHERE client AND xmin_age IS NOT NULL
   ORDER BY xmin_age DESC, pid LIMIT 5),
graph AS (
  SELECT w.locktype, w.mode, false AS granted, c.relname, w.pid, w.blocked_by,
         w.waitstart
    FROM waiters w
    LEFT JOIN pg_class c ON c.oid = w.relation AND w.database = (SELECT oid FROM d)
  UNION ALL
  SELECT COALESCE(v.locktype, 'session'), COALESCE(v.mode, ''), true, NULL, a.pid,
         NULL, NULL
    FROM act a
    LEFT JOIN lk v ON v.pid = a.pid AND v.granted
   WHERE a.pid NOT IN (SELECT pid FROM waiting)
     AND (a.pid IN (SELECT pid FROM blockers) OR a.pid IN (SELECT pid FROM oldest)
          OR (a.client AND a.state LIKE 'idle in transaction%')))
SELECT g.locktype, g.mode, g.granted, g.relname,
       a.query, a.state, a.wait_event_type, a.wait_event, g.pid,
       a.backend_start, a.query_start, a.xact_start, a.xmin_age, g.blocked_by,
       a.query_id, a.state_change
  FROM graph g JOIN act a ON a.pid = g.pid
 ORDER BY CASE WHEN g.pid IN (SELECT pid FROM oldest) THEN 0
               WHEN g.pid IN (SELECT pid FROM blockers) THEN 1
               WHEN NOT g.granted THEN 2 ELSE 3 END,
          a.xmin_age DESC NULLS LAST, g.waitstart NULLS LAST, g.pid
 LIMIT 201`

// loadRatioSQL is the server's active client sessions over
// max_connections (the circuit breaker protects the server, so it is
// cluster-wide); pg_sage's own are not load it backs off from.
var loadRatioSQL = sageTag + `
SELECT count(*)::float /
       (SELECT setting::float FROM pg_settings WHERE name = 'max_connections')
       AS load_ratio
  FROM pg_stat_activity
 WHERE state = 'active' AND pid <> pg_backend_pid() AND ` + notSelf

// connectionStatesSQL is this database's client sessions by state.
var connectionStatesSQL = sageTag + `
SELECT COALESCE(state,'unknown'), count(*)::int,
       COALESCE(avg(EXTRACT(EPOCH FROM (now() - state_change)))::int, 0)
  FROM pg_stat_activity
 WHERE backend_type = 'client backend'
   AND datname = current_database() AND ` + notSelf + `
 GROUP BY state`

// waitLoadTag names the wait-load read in pg_stat_statements (one per
// cycle, market-01).
const waitLoadTag = "/* pg_sage collector wait_load */ "

// activeByWaitSQL is this database's active client sessions by wait class
// and event (market-01): COALESCE(wait_event_type, 'CPU') counts a session
// with no wait event as running. One aggregate per cycle, sampled each
// collector cycle (not active session history); it reads no query text
// and no pid, and pg_sage's own sessions are not the workload.
var activeByWaitSQL = waitLoadTag + `
SELECT COALESCE(wait_event_type, 'CPU'), COALESCE(wait_event, ''), count(*)::int
  FROM pg_stat_activity
 WHERE state = 'active' AND backend_type = 'client backend'
   AND pid <> pg_backend_pid()
   AND datname = current_database() AND ` + notSelf + `
 GROUP BY 1, 2
 ORDER BY 3 DESC, 1, 2`
