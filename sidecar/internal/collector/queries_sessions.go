package collector

import "github.com/pg-sage/sidecar/internal/selfmonitor"

// Session-based snapshot inputs (perf v1.8.3, perf-selfexcl). pg_sage's
// own sessions (application_name pg_sage: its pools, probes, EXPLAINs and
// index builds) are not the workload: active and idle-in-transaction
// counts, locks, connection states, churn and the load circuit breaker
// leave them out with selfmonitor.ActivityExclusionSQL. Connection slots
// (total_backends) are capacity and still count every backend.

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

// systemStatsSQL14 uses pg_stat_bgwriter (PG 14-16).
var systemStatsSQL14 = systemStatsSQLBase + `
  (SELECT checkpoints_timed + checkpoints_req
    FROM pg_stat_bgwriter) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica`

// systemStatsSQL17 uses pg_stat_checkpointer (PG 17+).
var systemStatsSQL17 = systemStatsSQLBase + `
  (SELECT num_timed + num_requested
    FROM pg_stat_checkpointer) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica`

// locksSQL is scoped to the current database (G1-B17): relation locks
// must belong to this database (pg_class OIDs are per-database, so a
// foreign lock would resolve to the wrong relname), and database-less
// locks (transactionid, virtualxid) only count for local backends.
// pg_sage's own sessions' locks are left out (a lock without a session,
// a prepared transaction's, stays).
var locksSQL = sageTag + `
SELECT l.locktype, l.mode, l.granted,
       c.relname,
       a.query, a.state,
       a.wait_event_type, a.wait_event,
       l.pid,
       a.backend_start, a.query_start
  FROM pg_locks l
 CROSS JOIN (SELECT oid FROM pg_database WHERE datname = current_database()) d
  LEFT JOIN pg_stat_activity a ON a.pid = l.pid
  LEFT JOIN pg_class c ON c.oid = l.relation AND l.database = d.oid
 WHERE l.pid <> pg_backend_pid()
   AND (l.database = d.oid OR (l.database IS NULL AND a.datid = d.oid))
   AND ` + selfmonitor.ActivityExclusionSQL("a") + `
 ORDER BY l.granted, l.pid`

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

// connectionChurnSQL is this database's client sessions started in the
// last 5 minutes (pg_sage's pools reconnect on their own schedule).
var connectionChurnSQL = sageTag + `
SELECT count(*)::int FROM pg_stat_activity
 WHERE backend_start > now() - interval '5 minutes'
   AND backend_type = 'client backend'
   AND datname = current_database() AND ` + notSelf
