package collector

// SQL constants for each collection category.

const queryStatsSQL = `
SELECT COALESCE(queryid, 0), query, calls,
       total_exec_time, mean_exec_time, min_exec_time, max_exec_time,
       stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
       temp_blks_read, temp_blks_written,
       0::float8 AS blk_read_time, 0::float8 AS blk_write_time
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
 ORDER BY total_exec_time DESC
 LIMIT %d`

const queryStatsWithWALSQL = `
SELECT COALESCE(queryid, 0), query, calls,
       total_exec_time, mean_exec_time, min_exec_time, max_exec_time,
       stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
       temp_blks_read, temp_blks_written,
       0::float8 AS blk_read_time, 0::float8 AS blk_write_time,
       wal_records, wal_fpi, wal_bytes
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
 ORDER BY total_exec_time DESC
 LIMIT %d`

const queryStatsWithPlanTimeSQL = `
SELECT COALESCE(queryid, 0), query, calls,
       total_exec_time, mean_exec_time, min_exec_time, max_exec_time,
       stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
       temp_blks_read, temp_blks_written,
       0::float8 AS blk_read_time, 0::float8 AS blk_write_time,
       COALESCE(total_plan_time, 0) AS total_plan_time,
       COALESCE(mean_plan_time, 0) AS mean_plan_time
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
 ORDER BY total_exec_time DESC
 LIMIT %d`

const queryStatsWithWALAndPlanTimeSQL = `
SELECT COALESCE(queryid, 0), query, calls,
       total_exec_time, mean_exec_time, min_exec_time, max_exec_time,
       stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read, shared_blks_dirtied, shared_blks_written,
       temp_blks_read, temp_blks_written,
       0::float8 AS blk_read_time, 0::float8 AS blk_write_time,
       wal_records, wal_fpi, wal_bytes,
       COALESCE(total_plan_time, 0) AS total_plan_time,
       COALESCE(mean_plan_time, 0) AS mean_plan_time
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
 ORDER BY total_exec_time DESC
 LIMIT %d`

const tableStatsSQL = `
SELECT s.schemaname, s.relname,
       COALESCE(s.seq_scan, 0), COALESCE(s.seq_tup_read, 0),
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_fetch, 0),
       COALESCE(s.n_tup_ins, 0), COALESCE(s.n_tup_upd, 0),
       COALESCE(s.n_tup_del, 0), COALESCE(s.n_tup_hot_upd, 0),
       COALESCE(s.n_live_tup, 0), COALESCE(s.n_dead_tup, 0),
       s.last_vacuum, s.last_autovacuum, s.last_analyze, s.last_autoanalyze,
       s.vacuum_count, s.autovacuum_count, s.analyze_count, s.autoanalyze_count,
       pg_total_relation_size(c.oid) AS total_bytes,
       pg_table_size(c.oid) AS table_bytes,
       COALESCE(pg_indexes_size(c.oid), 0) AS index_bytes,
       c.relpersistence
  FROM pg_stat_user_tables s
  JOIN pg_class c ON c.relname = s.relname
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = s.schemaname
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND (s.schemaname, s.relname) > ($1, $1)
 ORDER BY s.schemaname, s.relname
 LIMIT $2`

const indexStatsSQL = `
SELECT s.schemaname, s.relname, s.indexrelname,
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_read, 0),
       COALESCE(s.idx_tup_fetch, 0),
       pg_relation_size(i.indexrelid) AS index_bytes,
       ix.indisunique, ix.indisprimary, ix.indisvalid,
       pg_get_indexdef(i.indexrelid) AS indexdef,
       am.amname AS index_type
  FROM pg_stat_user_indexes s
  JOIN pg_statio_user_indexes i ON i.indexrelid = s.indexrelid
  JOIN pg_index ix ON ix.indexrelid = s.indexrelid
  JOIN pg_am am ON am.oid = (
       SELECT c.relam FROM pg_class c WHERE c.oid = s.indexrelid
  )
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
 ORDER BY s.schemaname, s.relname, s.indexrelname`

const foreignKeysSQL = `
SELECT cl.relname AS table_name,
       cl2.relname AS referenced_table,
       a.attname AS fk_column,
       con.conname AS constraint_name
  FROM pg_constraint con
  JOIN pg_class cl ON cl.oid = con.conrelid
  JOIN pg_class cl2 ON cl2.oid = con.confrelid
  JOIN pg_namespace n ON n.oid = cl.relnamespace
  JOIN pg_attribute a ON a.attrelid = con.conrelid
       AND a.attnum = ANY(con.conkey)
 WHERE con.contype = 'f'
   AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
 ORDER BY cl.relname, con.conname`

// systemStatsSQLBase is the common prefix for system stats (all PG versions).
const systemStatsSQLBase = `
SELECT
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'active' AND pid <> pg_backend_pid()) AS active_backends,
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'idle in transaction') AS idle_in_transaction,
  (SELECT count(*) FROM pg_stat_activity
    WHERE pid <> pg_backend_pid()) AS total_backends,
  (SELECT setting::int FROM pg_settings
    WHERE name = 'max_connections') AS max_connections,
  COALESCE(
    (SELECT round(
       sum(blks_hit)::numeric /
       NULLIF(sum(blks_hit) + sum(blks_read), 0) * 100, 2)
     FROM pg_stat_database
     WHERE datname = current_database()), 0) AS cache_hit_ratio,
  COALESCE((SELECT deadlocks FROM pg_stat_database
    WHERE datname = current_database()), 0) AS deadlocks,
  COALESCE((SELECT blk_read_time FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blk_read_time,
  COALESCE((SELECT blk_write_time FROM pg_stat_database
    WHERE datname = current_database()), 0) AS blk_write_time,
`

// systemStatsSQL14 uses pg_stat_bgwriter (PG 14-16).
const systemStatsSQL14 = systemStatsSQLBase + `
  (SELECT checkpoints_timed + checkpoints_req
    FROM pg_stat_bgwriter) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica,
  pg_database_size(current_database()) AS db_size_bytes`

// systemStatsSQL17 uses pg_stat_checkpointer (PG 17+).
const systemStatsSQL17 = systemStatsSQLBase + `
  (SELECT num_timed + num_requested
    FROM pg_stat_checkpointer) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica,
  pg_database_size(current_database()) AS db_size_bytes`

const locksSQL = `
SELECT l.locktype, l.mode, l.granted,
       c.relname,
       a.query, a.state,
       a.wait_event_type, a.wait_event,
       l.pid,
       a.backend_start, a.query_start
  FROM pg_locks l
  LEFT JOIN pg_class c ON c.oid = l.relation
  LEFT JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE l.pid <> pg_backend_pid()
 ORDER BY l.granted, l.pid`

const sequencesSQL = `
SELECT schemaname, sequencename, data_type,
       COALESCE(last_value, 0), max_value, increment_by,
       CASE WHEN max_value > 0 AND last_value IS NOT NULL
            THEN round((last_value::numeric / max_value) * 100, 2)
            ELSE 0
       END AS pct_used
  FROM pg_sequences
 WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
 ORDER BY pct_used DESC`

const replicationReplicasSQL = `
SELECT client_addr::text, state,
       sent_lsn::text, write_lsn::text, flush_lsn::text, replay_lsn::text,
       write_lag::text, flush_lag::text, replay_lag::text,
       sync_state
  FROM pg_stat_replication`

const replicationSlotsSQL = `
SELECT slot_name, slot_type, active,
       pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS retained_bytes
  FROM pg_replication_slots`

// pg_stat_io (PG16+): I/O statistics by backend type.
const ioStatsSQL = `
SELECT backend_type, object, context,
       COALESCE(reads, 0), COALESCE(read_time, 0),
       COALESCE(writes, 0), COALESCE(write_time, 0),
       COALESCE(writebacks, 0), COALESCE(writeback_time, 0),
       COALESCE(extends, 0), COALESCE(extend_time, 0),
       COALESCE(hits, 0), COALESCE(evictions, 0),
       COALESCE(reuses, 0), COALESCE(fsyncs, 0),
       COALESCE(fsync_time, 0)
  FROM pg_stat_io
 WHERE reads > 0 OR writes > 0 OR hits > 0
 ORDER BY reads + writes DESC
 LIMIT 100`

// Partition inheritance: maps child tables to their parent.
const partitionInheritanceSQL = `
SELECT c.relname AS child_table,
       n.nspname AS child_schema,
       p.relname AS parent_table,
       pn.nspname AS parent_schema
  FROM pg_inherits i
  JOIN pg_class c ON c.oid = i.inhrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_class p ON p.oid = i.inhparent
  JOIN pg_namespace pn ON pn.oid = p.relnamespace
 WHERE n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
 ORDER BY parent_schema, parent_table, child_schema, child_table`

const loadRatioSQL = `
SELECT count(*)::float /
       (SELECT setting::float FROM pg_settings WHERE name = 'max_connections')
       AS load_ratio
  FROM pg_stat_activity
 WHERE state = 'active' AND pid <> pg_backend_pid()`
