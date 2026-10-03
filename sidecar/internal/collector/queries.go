package collector

// SQL constants for each collection category.

// sageTag marks every collector query so pg_sage's own catalog reads
// (pg_stat_activity, pg_stat_user_tables, pg_replication_slots, …) are
// recognized and excluded by the self-monitoring filter. These queries
// don't reference the sage schema or the literal "pg_sage", so without
// the marker they leak into pg_stat_statements and surface as findings.
// The comment survives pg_stat_statements normalization (verified), so a
// query carrying it matches the filter's `ILIKE '%pg_sage%'`.
const sageTag = "/* pg_sage */ "

// queryStatsSelect aggregates pg_stat_statements to ONE row per queryid.
// pg_stat_statements keys rows by (userid, dbid, queryid, toplevel), so the
// same statement run by two roles (or top-level and nested) would otherwise
// produce duplicate query_store samples with one captured_at (G1-B05).
// Means and the population stddev are recombined from per-row calls.
// The two %s verbs are the block read/write time expressions chosen by
// blockTimeColumns (PG17 renamed blk_*_time to shared_/local_blk_*_time).
const queryStatsSelect = sageTag + `
SELECT COALESCE(queryid, 0) AS queryid,
       (array_agg(query ORDER BY calls DESC))[1] AS query,
       sum(calls)::bigint AS calls,
       sum(total_exec_time)::float8 AS total_exec_time,
       COALESCE(sum(total_exec_time) / NULLIF(sum(calls), 0), 0)::float8 AS mean_exec_time,
       COALESCE(min(min_exec_time), 0)::float8 AS min_exec_time,
       COALESCE(max(max_exec_time), 0)::float8 AS max_exec_time,
       COALESCE(sqrt(GREATEST(
         sum(calls * (stddev_exec_time ^ 2 + mean_exec_time ^ 2)) / NULLIF(sum(calls), 0)
         - (sum(total_exec_time) / NULLIF(sum(calls), 0)) ^ 2, 0)), 0)::float8
         AS stddev_exec_time,
       sum(rows)::bigint AS rows,
       sum(shared_blks_hit)::bigint, sum(shared_blks_read)::bigint,
       sum(shared_blks_dirtied)::bigint, sum(shared_blks_written)::bigint,
       sum(temp_blks_read)::bigint, sum(temp_blks_written)::bigint,
       COALESCE(sum(%s), 0)::float8 AS blk_read_time,
       COALESCE(sum(%s), 0)::float8 AS blk_write_time`

const queryStatsWALColumns = `,
       sum(wal_records)::bigint, sum(wal_fpi)::bigint, sum(wal_bytes)::bigint`

const queryStatsPlanColumns = `,
       COALESCE(sum(total_plan_time), 0)::float8 AS total_plan_time,
       COALESCE(sum(total_plan_time) / NULLIF(sum(plans), 0), 0)::float8
         AS mean_plan_time`

const queryStatsFrom = `
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
   AND COALESCE(query, '') NOT ILIKE '%%pg_sage%%'
   AND COALESCE(query, '') !~* '(^|[^[:alnum:]_])("?sage"?)[[:space:]]*\.'
 GROUP BY queryid
 ORDER BY sum(total_exec_time) DESC
 LIMIT %d`

const queryStatsSQL = queryStatsSelect + queryStatsFrom

const queryStatsWithWALSQL = queryStatsSelect + queryStatsWALColumns + queryStatsFrom

const queryStatsWithPlanTimeSQL = queryStatsSelect + queryStatsPlanColumns + queryStatsFrom

const queryStatsWithWALAndPlanTimeSQL = queryStatsSelect + queryStatsWALColumns +
	queryStatsPlanColumns + queryStatsFrom

// blockTimeColumnsSQL finds which block-time columns the installed
// pg_stat_statements view exposes (depends on the extension version, not
// only the server version).
const blockTimeColumnsSQL = sageTag + `
SELECT attname::text FROM pg_attribute
 WHERE attrelid = to_regclass('pg_stat_statements')
   AND attnum > 0 AND NOT attisdropped
   AND attname IN ('shared_blk_read_time', 'blk_read_time')`

// tableStatsSQL pages pg_class by oid and reads each table's counters
// with the pg_stat_get_* functions pg_stat_user_tables is built from
// (same filters and NULL handling). The view groups every table after the
// cursor on every page (measured.md M1: 114 ms per 1,000-row page on
// lifeos). Sizes are relpages estimates (heap + TOAST; summed index
// pages): no relation is opened or stat()ed here; the collector measures
// the top-N exactly afterwards.
const tableStatsSQL = sageTag + `
SELECT n.nspname, c.relname,
       pg_stat_get_numscans(c.oid), pg_stat_get_tuples_returned(c.oid),
       COALESCE(ix.idx_scan, 0),
       COALESCE(ix.idx_tup_fetch + pg_stat_get_tuples_fetched(c.oid), 0),
       pg_stat_get_tuples_inserted(c.oid), pg_stat_get_tuples_updated(c.oid),
       pg_stat_get_tuples_deleted(c.oid), pg_stat_get_tuples_hot_updated(c.oid),
       pg_stat_get_live_tuples(c.oid), pg_stat_get_dead_tuples(c.oid),
       pg_stat_get_last_vacuum_time(c.oid), pg_stat_get_last_autovacuum_time(c.oid),
       pg_stat_get_last_analyze_time(c.oid), pg_stat_get_last_autoanalyze_time(c.oid),
       pg_stat_get_vacuum_count(c.oid), pg_stat_get_autovacuum_count(c.oid),
       pg_stat_get_analyze_count(c.oid), pg_stat_get_autoanalyze_count(c.oid),
       (c.relpages::int8 + COALESCE(t.relpages, 0)::int8)
         * current_setting('block_size')::int8 AS table_bytes,
       COALESCE(ix.pages, 0) * current_setting('block_size')::int8 AS index_bytes,
       c.relpersistence::text,
       age(c.relfrozenxid) AS xid_age, c.oid
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
  LEFT JOIN LATERAL (
       SELECT sum(pg_stat_get_numscans(i.indexrelid))::int8 AS idx_scan,
              sum(pg_stat_get_tuples_fetched(i.indexrelid))::int8 AS idx_tup_fetch,
              sum(ic.relpages)::int8 AS pages
         FROM pg_index i
         JOIN pg_class ic ON ic.oid = i.indexrelid
        WHERE i.indrelid = c.oid) ix ON true
 WHERE c.oid > $1
   AND c.relkind IN ('r', 'm', 'p')
   AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND n.nspname !~ '^pg_toast'
 ORDER BY c.oid
 LIMIT $2`

// indexStatsSQL reads the index counters without the per-index
// pg_relation_size and pg_get_indexdef (measured.md M3: 31 ms per page,
// 35k definitions re-derived every minute). Sizes are relpages estimates;
// def_version identifies the catalog rows a cached definition was read
// from (index row and file, table row, schema row).
const indexStatsSQL = sageTag + `
SELECT s.schemaname, s.relname, s.indexrelname,
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_read, 0),
       COALESCE(s.idx_tup_fetch, 0),
       ic.relpages::int8 * current_setting('block_size')::int8 AS index_bytes,
       ix.indisunique, ix.indisprimary, ix.indisvalid,
       COALESCE(am.amname, 'unknown') AS index_type, s.indexrelid,
       -- last_idx_scan exists from PG16; read it by name so older servers
       -- (and a mis-detected version) return NULL instead of failing.
       (to_jsonb(s) ->> 'last_idx_scan')::timestamptz AS last_idx_scan,
       ic.xmin::text || ':' || ic.relfilenode::text || ':' ||
         tc.xmin::text || ':' || n.xmin::text AS def_version
  FROM pg_stat_user_indexes s
  JOIN pg_index ix ON ix.indexrelid = s.indexrelid
  JOIN pg_class ic ON ic.oid = s.indexrelid
  JOIN pg_class tc ON tc.oid = s.relid
  JOIN pg_namespace n ON n.oid = tc.relnamespace
  JOIN pg_am am ON am.oid = ic.relam
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND s.indexrelid > $1
 ORDER BY s.indexrelid
 LIMIT $2`

const foreignKeysSQL = sageTag + `
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

// cacheHitRatioExpr is the buffer cache hit ratio as a FRACTION in
// [0,1] (G1-B08/C01: it used to be a percent compared against fractional
// thresholds). No block accesses yields NULL, i.e. unknown, never 0.
const cacheHitRatioExpr = `round(sum(blks_hit)::numeric /
       NULLIF(sum(blks_hit) + sum(blks_read), 0), 4)`

// systemStatsSQLBase is the common prefix for system stats (all PG versions).
const systemStatsSQLBase = sageTag + `
SELECT
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'active' AND pid <> pg_backend_pid()) AS active_backends,
  (SELECT count(*) FROM pg_stat_activity
    WHERE state = 'idle in transaction'
      AND datname = current_database()) AS idle_in_transaction,
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
const systemStatsSQL14 = systemStatsSQLBase + `
  (SELECT checkpoints_timed + checkpoints_req
    FROM pg_stat_bgwriter) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica`

// systemStatsSQL17 uses pg_stat_checkpointer (PG 17+).
const systemStatsSQL17 = systemStatsSQLBase + `
  (SELECT num_timed + num_requested
    FROM pg_stat_checkpointer) AS total_checkpoints,
  pg_is_in_recovery() AS is_replica`

// locksSQL is scoped to the current database (G1-B17): relation locks
// must belong to this database (pg_class OIDs are per-database, so a
// foreign lock would resolve to the wrong relname), and database-less
// locks (transactionid, virtualxid) only count for local backends.
const locksSQL = sageTag + `
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
 ORDER BY l.granted, l.pid`

const replicationReplicasSQL = sageTag + `
SELECT client_addr::text, state,
       sent_lsn::text, write_lsn::text, flush_lsn::text, replay_lsn::text,
       write_lag::text, flush_lag::text, replay_lag::text,
       sync_state
  FROM pg_stat_replication`

// replicationSlotsSQL must work on both primaries and hot standbys.
// pg_current_wal_lsn() raises "recovery is in progress" on a standby,
// so we pick the receive LSN there instead. restart_lsn is NULL for a
// slot that has not reserved WAL yet, which would make the diff NULL
// and break the scan into a non-nullable int64 — COALESCE guards it.
const replicationSlotsSQL = sageTag + `
SELECT slot_name, slot_type, active,
       COALESCE(
         pg_wal_lsn_diff(
           CASE WHEN pg_is_in_recovery()
                THEN pg_last_wal_receive_lsn()
                ELSE pg_current_wal_lsn() END,
           restart_lsn),
         0) AS retained_bytes
  FROM pg_replication_slots`

// pg_stat_io (PG16+): I/O statistics by backend type.
const ioStatsSQL = sageTag + `
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
const partitionInheritanceSQL = sageTag + `
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

// pg_prepared_xacts: two-phase commit transactions that survive
// connection drops and server restarts. They hold xmin and locks
// indefinitely and are invisible to pg_stat_activity.
const preparedXactsSQL = sageTag + `
SELECT gid, prepared, owner, database,
       age(transaction) AS xid_age
  FROM pg_prepared_xacts
 ORDER BY prepared`

const loadRatioSQL = sageTag + `
SELECT count(*)::float /
       (SELECT setting::float FROM pg_settings WHERE name = 'max_connections')
       AS load_ratio
  FROM pg_stat_activity
 WHERE state = 'active' AND pid <> pg_backend_pid()`
