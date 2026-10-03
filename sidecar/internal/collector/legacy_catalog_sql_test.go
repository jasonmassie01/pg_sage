package collector

// The v1.8.2 catalog SQL, kept verbatim as the golden reference: the
// rewritten collector must return the same rows (sizes aside, which are
// estimates outside the exact top-N) without its per-relation cost.

const legacyTableStatsSQL = sageTag + `
SELECT s.schemaname, s.relname,
       COALESCE(s.seq_scan, 0), COALESCE(s.seq_tup_read, 0),
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_fetch, 0),
       COALESCE(s.n_tup_ins, 0), COALESCE(s.n_tup_upd, 0),
       COALESCE(s.n_tup_del, 0), COALESCE(s.n_tup_hot_upd, 0),
       COALESCE(s.n_live_tup, 0), COALESCE(s.n_dead_tup, 0),
       s.last_vacuum, s.last_autovacuum, s.last_analyze, s.last_autoanalyze,
       s.vacuum_count, s.autovacuum_count, s.analyze_count, s.autoanalyze_count,
       COALESCE(pg_total_relation_size(c.oid), 0) AS total_bytes,
       COALESCE(pg_table_size(c.oid), 0) AS table_bytes,
       COALESCE(pg_indexes_size(c.oid), 0) AS index_bytes,
       c.relpersistence::text,
       age(c.relfrozenxid) AS xid_age, s.relid
  FROM pg_stat_user_tables s
  JOIN pg_class c ON c.oid = s.relid
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND s.relid > $1
 ORDER BY s.relid
 LIMIT $2`

const legacyIndexStatsSQL = sageTag + `
SELECT s.schemaname, s.relname, s.indexrelname,
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_read, 0),
       COALESCE(s.idx_tup_fetch, 0),
       COALESCE(pg_relation_size(s.indexrelid), 0) AS index_bytes,
       ix.indisunique, ix.indisprimary, ix.indisvalid,
       COALESCE(pg_get_indexdef(s.indexrelid), '') AS indexdef,
       COALESCE(am.amname, 'unknown') AS index_type, s.indexrelid,
       (to_jsonb(s) ->> 'last_idx_scan')::timestamptz AS last_idx_scan
  FROM pg_stat_user_indexes s
  JOIN pg_index ix ON ix.indexrelid = s.indexrelid
  JOIN pg_class ic ON ic.oid = s.indexrelid
  JOIN pg_am am ON am.oid = ic.relam
 WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND s.indexrelid > $1
 ORDER BY s.indexrelid
 LIMIT $2`

const legacySequencesSQL = sageTag + `
SELECT schemaname, sequencename, data_type, last_value, min_value, max_value,
       increment_by, cycle, pct_used
  FROM (
    SELECT s.*, row_number() OVER (ORDER BY pct_used DESC, schemaname, sequencename) AS rn
      FROM (
    SELECT schemaname, sequencename, data_type,
           COALESCE(last_value, 0) AS last_value, min_value, max_value,
           increment_by, cycle,
           CASE WHEN max_value = min_value THEN 0
                WHEN increment_by > 0
                THEN round((last_value::numeric - min_value::numeric) /
                           (max_value::numeric - min_value::numeric) * 100, 2)
                ELSE round((max_value::numeric - last_value::numeric) /
                           (max_value::numeric - min_value::numeric) * 100, 2)
           END AS pct_used
      FROM pg_sequences
     WHERE schemaname NOT IN ('pg_catalog', 'information_schema', 'sage')
       AND last_value IS NOT NULL
      ) s
  ) r
 WHERE pct_used >= $1 OR rn <= $2
 ORDER BY pct_used DESC, schemaname, sequencename
 LIMIT $3`
