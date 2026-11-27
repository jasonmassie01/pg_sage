package collector

// SQL constants for each collection category.

// notTempRel is the catalog reads' predicate that leaves out temporary
// relations (collection-03): other sessions' and pg_sage's own. No other
// session can vacuum, analyze or index them, and they vanish with their
// session, so they are not the application's objects. Unlogged relations
// ('u') are, and stay. Every relation read carries it, so the reads
// cannot drift apart.
func notTempRel(alias string) string {
	return alias + ".relpersistence <> 't'"
}

// sageTag marks every collector query so pg_sage's own catalog reads
// (pg_stat_activity, pg_stat_user_tables, pg_replication_slots, …) are
// recognized and excluded by the self-monitoring filter. These queries
// don't reference the sage schema or the literal "pg_sage", so without
// the marker they leak into pg_stat_statements and surface as findings.
// The comment survives pg_stat_statements normalization (verified), so a
// query carrying it matches the filter's `ILIKE '%pg_sage%'`.
const sageTag = "/* pg_sage collector */ "

// queryStatsSelect aggregates pg_stat_statements to ONE row per queryid
// (agg). pg_stat_statements keys rows by (userid, dbid, queryid, toplevel),
// so the same statement run by two roles (or top-level and nested) would
// otherwise produce duplicate query_store samples with one captured_at
// (G1-B05). Means and the population stddev are recombined from per-row
// calls. The %[1]s and %[2]s verbs are the block read/write time
// expressions chosen by the capability probe (blockTimeFor) (PG17 renamed
// blk_*_time to shared_/local_blk_*_time).
//
// It reads the counters only (showtext false): pg_stat_statements(true)
// loads the whole query-text file whatever rows are kept (14 MB on
// lifeos), so texts come from the collector's cache and are read only for
// the candidates it lacks (statementTextsSQL, collection-11). pg_sage's own
// statements are left out in Go once their texts are known
// (IsStatementText).
const queryStatsSelect = sageTag + `
WITH agg AS (
SELECT COALESCE(queryid, 0) AS queryid,
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
       sum(shared_blks_hit)::bigint AS shared_blks_hit,
       sum(shared_blks_read)::bigint AS shared_blks_read,
       sum(shared_blks_dirtied)::bigint AS shared_blks_dirtied,
       sum(shared_blks_written)::bigint AS shared_blks_written,
       sum(temp_blks_read)::bigint AS temp_blks_read,
       sum(temp_blks_written)::bigint AS temp_blks_written,
       COALESCE(sum(%[1]s), 0)::float8 AS blk_read_time,
       COALESCE(sum(%[2]s), 0)::float8 AS blk_write_time`

const queryStatsWALColumns = `,
       sum(wal_records)::bigint AS wal_records, sum(wal_fpi)::bigint AS wal_fpi,
       sum(wal_bytes)::bigint AS wal_bytes`

const queryStatsPlanColumns = `,
       COALESCE(sum(total_plan_time), 0)::float8 AS total_plan_time,
       COALESCE(sum(total_plan_time) / NULLIF(sum(plans), 0), 0)::float8
         AS mean_plan_time`

// queryStatsFrom admits the candidates (collection-07): the bounded union
// of four arms over agg, each row tagged with the arms that admitted it.
// total is the top %[3]d by total time (the ranking before collection-07);
// mean and io are the top %[4]d by mean time (two calls or more) and by
// blocks read plus temp blocks written, both without the queryids known
// to be pg_sage's own ($2); pinned is every row of $1 (open verification
// targets and watches). Pinned rows come first, then the other rows the
// total ranking would not have admitted, then the rest by total time, so
// the Go cut at max_queries keeps them; the final %d is the row bound and
// %[6]s the qualified pg_stat_statements function. Every row also carries
// the database's statement totals over agg less pg_sage's known own
// statements ($2), whatever the admission keeps (evid-01a: the rollup's
// coverage); they ride on this read, no second statement.
const queryStatsFrom = `
  FROM %[6]s(false)
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid IS NOT NULL
 GROUP BY queryid
), arms AS (
  (SELECT queryid, 4 AS arm_rank, 'total' AS arm FROM agg
    ORDER BY total_exec_time DESC, queryid LIMIT %[3]d)
  UNION ALL
  (SELECT queryid, 2, 'mean' FROM agg
    WHERE calls >= 2 AND mean_exec_time > 0 AND queryid <> ALL($2::int8[])
    ORDER BY mean_exec_time DESC, queryid LIMIT %[4]d)
  UNION ALL
  (SELECT queryid, 3, 'io' FROM agg
    WHERE shared_blks_read + temp_blks_written > 0 AND queryid <> ALL($2::int8[])
    ORDER BY shared_blks_read + temp_blks_written DESC, queryid LIMIT %[4]d)
  UNION ALL
  SELECT queryid, 1, 'pinned' FROM agg WHERE queryid = ANY($1::int8[])
), admitted AS (
  SELECT queryid, min(arm_rank) AS first_rank,
         array_agg(arm ORDER BY arm_rank) AS admitted_by
    FROM arms GROUP BY queryid
)
SELECT agg.*, totals.stmt_exec_ms, totals.stmt_calls, totals.stmt_entries,
       admitted.admitted_by
  FROM agg JOIN admitted USING (queryid)
 CROSS JOIN (
  SELECT COALESCE(sum(total_exec_time), 0)::float8 AS stmt_exec_ms,
         COALESCE(sum(calls), 0)::bigint AS stmt_calls, count(*)::int AS stmt_entries
    FROM agg WHERE queryid <> ALL($2::int8[])) totals
 ORDER BY CASE admitted.first_rank WHEN 1 THEN 0 WHEN 4 THEN 2 ELSE 1 END,
          agg.total_exec_time DESC, agg.queryid
 LIMIT %d`

const queryStatsSQL = queryStatsSelect + queryStatsFrom

const queryStatsWithWALSQL = queryStatsSelect + queryStatsWALColumns + queryStatsFrom

const queryStatsWithPlanTimeSQL = queryStatsSelect + queryStatsPlanColumns + queryStatsFrom

const queryStatsWithWALAndPlanTimeSQL = queryStatsSelect + queryStatsWALColumns +
	queryStatsPlanColumns + queryStatsFrom

// tableStatsSQL pages pg_class by oid and reads each table's counters
// with the pg_stat_get_* functions pg_stat_user_tables is built from
// (same filters and NULL handling). The view groups every table after the
// cursor on every page (measured.md M1: 114 ms per 1,000-row page on
// lifeos). Sizes are relpages estimates (heap + TOAST; summed index
// pages): no relation is opened or stat()ed here; the collector measures
// the top-N exactly afterwards. xid_age is the older of the heap's and its
// TOAST table's age(relfrozenxid); xid_age and mxid_age skip a zero
// (non-normal) XID, whose age() is 2147483647, and are -1 when unknown:
// a partitioned parent has no storage, so both of its horizons are 0.
// reloptions rides on the same page: the deterministic rules need each
// table's own storage parameters whether or not the advisor's
// configuration snapshot runs (rules-02).
// mod_since_analyze and analyze_threshold are what autoanalyze compares:
// the rows changed since the last analyze, and autovacuum_analyze_threshold
// + autovacuum_analyze_scale_factor x reltuples (a never-counted -1 as 0),
// each taken from the table's reloptions when set there (rules-10).
var tableStatsSQL = sageTag + `
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
       c.relpersistence::text, c.relkind::text,
       COALESCE(GREATEST(
         CASE WHEN c.relfrozenxid <> '0'::xid THEN age(c.relfrozenxid) END,
         CASE WHEN t.relfrozenxid <> '0'::xid THEN age(t.relfrozenxid) END),
         -1)::int8 AS xid_age,
       COALESCE(CASE WHEN c.relminmxid <> '0'::xid THEN mxid_age(c.relminmxid) END,
         -1)::int8 AS mxid_age,
       pg_stat_get_blocks_fetched(c.oid) - pg_stat_get_blocks_hit(c.oid) AS heap_blks_read,
       pg_stat_get_mod_since_analyze(c.oid) AS mod_since_analyze,
       COALESCE(ao.threshold, current_setting('autovacuum_analyze_threshold')::float8)
         + COALESCE(ao.scale_factor, current_setting('autovacuum_analyze_scale_factor')::float8)
           * GREATEST(c.reltuples, 0)::float8 AS analyze_threshold,
       c.reloptions, c.oid
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
  LEFT JOIN LATERAL (
       SELECT max(o.option_value) FILTER (
                WHERE o.option_name = 'autovacuum_analyze_threshold')::float8 AS threshold,
              max(o.option_value) FILTER (
                WHERE o.option_name = 'autovacuum_analyze_scale_factor')::float8 AS scale_factor
         FROM pg_options_to_table(c.reloptions) o) ao ON true
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
   AND ` + notTempRel("c") + `
 ORDER BY c.oid
 LIMIT $2`

// indexStatsSQL reads the index counters without the per-index
// pg_relation_size and pg_get_indexdef (measured.md M3: 31 ms per page,
// 35k definitions re-derived every minute). Sizes are relpages estimates;
// def_version identifies the catalog rows a cached definition was read
// from (index row and file, table row, schema row).
var indexStatsSQL = sageTag + `
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
         tc.xmin::text || ':' || n.xmin::text AS def_version,
       -- the partitioned index this one is attached to (rules-03)
       COALESCE((SELECT inh.inhparent FROM pg_inherits inh
                  WHERE inh.inhrelid = s.indexrelid), 0::oid) AS parent_index_oid
  FROM pg_stat_user_indexes s
  JOIN pg_index ix ON ix.indexrelid = s.indexrelid
  JOIN pg_class ic ON ic.oid = s.indexrelid
  JOIN pg_class tc ON tc.oid = s.relid
  JOIN pg_namespace n ON n.oid = tc.relnamespace
  JOIN pg_am am ON am.oid = ic.relam
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND ` + notTempRel("ic") + ` AND ` + notTempRel("tc") + `
   AND s.indexrelid > $1
 ORDER BY s.indexrelid
 LIMIT $2`

// foreignKeysSQL reads one row per constraint (collection-04): the schema
// of both sides, the referencing columns in conkey order, and the oid.
// Partition clones (conparentid <> 0, one per partition on either side)
// are left out: the constraint on the partitioned parent stands for them.
var foreignKeysSQL = sageTag + `
SELECT n.nspname AS schemaname, cl.relname AS table_name,
       rn.nspname AS ref_schemaname, cl2.relname AS referenced_table,
       con.conname AS constraint_name, con.oid,
       ARRAY(SELECT a.attname::text
               FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
               JOIN pg_attribute a ON a.attrelid = con.conrelid
                    AND a.attnum = k.attnum
              ORDER BY k.ord) AS columns
  FROM pg_constraint con
  JOIN pg_class cl ON cl.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = cl.relnamespace
  JOIN pg_class cl2 ON cl2.oid = con.confrelid
  JOIN pg_namespace rn ON rn.oid = cl2.relnamespace
 WHERE con.contype = 'f'
   AND con.conparentid = 0
   AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND ` + notTempRel("cl") + ` AND ` + notTempRel("cl2") + `
 ORDER BY cl.relname, con.conname, n.nspname`

// cacheHitRatioExpr is the buffer cache hit ratio as a FRACTION in
// [0,1] (G1-B08/C01: it used to be a percent compared against fractional
// thresholds). No block accesses yields NULL, i.e. unknown, never 0.
const cacheHitRatioExpr = `round(sum(blks_hit)::numeric /
       NULLIF(sum(blks_hit) + sum(blks_read), 0), 4)`

const replicationReplicasSQL = sageTag + `
SELECT COALESCE(application_name, '') AS application_name, client_addr::text, state,
       sent_lsn::text, write_lsn::text, flush_lsn::text, replay_lsn::text,
       write_lag::text, flush_lag::text, replay_lag::text,
       sync_state
  FROM pg_stat_replication`

// replicationSlotsSQL must work on both primaries and hot standbys.
// pg_current_wal_lsn() raises "recovery is in progress" on a standby,
// so we pick the receive LSN there instead. restart_lsn is NULL for a
// slot that has not reserved WAL yet, which would make the diff NULL
// and break the scan into a non-nullable int64 — COALESCE guards it.
// The columns that depend on the major version (wal_status and
// safe_wal_size PG13+; inactive_since, synced and invalidation_reason
// PG17+) are read by name through to_jsonb, so one statement runs on
// every server and a missing column reads as NULL. confirmed_flush_lsn
// (logical slots; NULL for physical ones) and its byte lag feed the CDC
// stall clock (market-13).
const replicationSlotsSQL = sageTag + `
SELECT s.slot_name, s.slot_type, s.active,
       COALESCE(
         pg_wal_lsn_diff(
           CASE WHEN pg_is_in_recovery()
                THEN pg_last_wal_receive_lsn()
                ELSE pg_current_wal_lsn() END,
           s.restart_lsn),
         0) AS retained_bytes,
       to_jsonb(s) ->> 'wal_status' AS wal_status,
       (to_jsonb(s) ->> 'safe_wal_size')::int8 AS safe_wal_size,
       COALESCE((to_jsonb(s) ->> 'temporary')::bool, false) AS temporary,
       (to_jsonb(s) ->> 'inactive_since')::timestamptz AS inactive_since,
       COALESCE((to_jsonb(s) ->> 'synced')::bool, false) AS synced,
       to_jsonb(s) ->> 'invalidation_reason' AS invalidation_reason,
       s.confirmed_flush_lsn::text AS confirmed_flush_lsn,
       pg_wal_lsn_diff(
         CASE WHEN pg_is_in_recovery()
              THEN pg_last_wal_receive_lsn()
              ELSE pg_current_wal_lsn() END,
         s.confirmed_flush_lsn)::int8 AS confirmed_flush_lag_bytes,
       COALESCE(s.database = current_database(), false) AS in_current_database
  FROM pg_replication_slots s`

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
var partitionInheritanceSQL = sageTag + `
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
   AND ` + notTempRel("c") + ` AND ` + notTempRel("p") + `
 ORDER BY parent_schema, parent_table, child_schema, child_table`

// pg_prepared_xacts: two-phase commit transactions that survive
// connection drops and server restarts. They hold xmin and locks
// indefinitely and are invisible to pg_stat_activity. Only this
// database's (rca-01): another database's prepared transaction does not
// hold back this database's vacuum, and is not this snapshot's to blame.
const preparedXactsSQL = sageTag + `
SELECT gid, prepared, owner, database,
       age(transaction) AS xid_age
  FROM pg_prepared_xacts
 WHERE database = current_database()
 ORDER BY prepared`
