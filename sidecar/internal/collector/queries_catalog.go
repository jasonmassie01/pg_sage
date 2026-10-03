package collector

// Catalog SQL for the bounded reads (perf fix phase): the per-relation
// work is limited to the exact top-N, changed index definitions, and one
// lock-budgeted page of sequences per transaction.

// sequenceLockBudgetSQL is a quarter of the shared lock table,
// max_locks_per_transaction x (max_connections + max_prepared_transactions):
// the same budget the SRE sequence_runway probe uses. Reading a sequence's
// last value holds its lock until the transaction ends; 20,000 such locks
// made other sessions fail with "out of shared memory" on default servers.
const sequenceLockBudgetSQL = `(current_setting('max_locks_per_transaction')::int8 *
        (current_setting('max_connections')::int8 +
         current_setting('max_prepared_transactions')::int8) / 4)`

// sequencePageSQL reads one oid page of sequences (measured.md M4: the
// pg_sequences view read all 12,044 in one transaction, holding 12,012
// locks). Same filters and arithmetic as the v1.8.2 query: consumption in
// the direction of travel over [min, max], numeric to avoid overflow,
// other sessions' temporary sequences and sage's own skipped, last values
// read only where the role may (unreadable ones are counted). page_limit
// is the page size after the lock-budget clamp.
const sequencePageSQL = sageTag + `
SELECT v.seqrelid, v.nspname, v.relname, v.data_type, v.readable,
       v.last_value, v.seqmin, v.seqmax, v.seqincrement, v.seqcycle,
       CASE WHEN v.last_value IS NULL THEN NULL
            WHEN v.seqmax = v.seqmin THEN 0
            WHEN v.seqincrement > 0
            THEN round((v.last_value::numeric - v.seqmin::numeric) /
                       (v.seqmax::numeric - v.seqmin::numeric) * 100, 2)
            ELSE round((v.seqmax::numeric - v.last_value::numeric) /
                       (v.seqmax::numeric - v.seqmin::numeric) * 100, 2)
       END AS pct_used,
       v.page_limit
  FROM (
    SELECT p.*, CASE WHEN p.readable
                     THEN pg_sequence_last_value(p.seqrelid::regclass) END AS last_value
      FROM (
        SELECT q.seqrelid, n.nspname::text, c.relname::text,
               q.seqtypid::regtype::text AS data_type,
               has_sequence_privilege(q.seqrelid, 'SELECT,USAGE') AS readable,
               q.seqmin, q.seqmax, q.seqincrement, q.seqcycle,
               LEAST($2::int8, ` + sequenceLockBudgetSQL + `) AS page_limit
          FROM pg_sequence q
          JOIN pg_class c ON c.oid = q.seqrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE q.seqrelid > $1
           AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
           AND NOT pg_is_other_temp_schema(n.oid)
         ORDER BY q.seqrelid
         LIMIT LEAST($2::int8, ` + sequenceLockBudgetSQL + `)
      ) p
  ) v
 ORDER BY v.seqrelid`

// exactTableSizesSQL measures the top-N tables (pg_total_relation_size is
// exactly table + indexes, so it is not called). A dropped oid yields NULL.
const exactTableSizesSQL = sageTag + `
SELECT o, pg_table_size(o), pg_indexes_size(o)
  FROM unnest($1::oid[]) AS o`

// exactIndexSizesSQL measures the top-N indexes.
const exactIndexSizesSQL = sageTag + `
SELECT o, pg_relation_size(o) FROM unnest($1::oid[]) AS o`

// indexDefsSQL reads definitions for new or changed indexes only.
const indexDefsSQL = sageTag + `
SELECT o, COALESCE(pg_get_indexdef(o), '') FROM unnest($1::oid[]) AS o`

// indexDefWatermarkSQL moves when a catalog row an index definition is
// rendered from may have changed without touching the index's or table's
// pg_class row: column renames (pg_attribute), schema renames
// (pg_namespace), function renames in expressions (pg_proc).
const indexDefWatermarkSQL = sageTag + `
SELECT (pg_stat_get_tuples_updated('pg_catalog.pg_attribute'::regclass) +
        pg_stat_get_tuples_updated('pg_catalog.pg_namespace'::regclass) +
        pg_stat_get_tuples_updated('pg_catalog.pg_proc'::regclass))::int8`

// databaseSizeSQL stat()s every file of the database (measured.md M7:
// 147-177 ms on lifeos), so it runs every DBSizeRefreshInterval, apart
// from the system stats. A variable so tests can make it slow.
var databaseSizeSQL = sageTag + `SELECT pg_database_size(current_database())`
