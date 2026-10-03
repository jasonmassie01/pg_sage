package schema

// ddlQueryStoreStatsEpoch records the pg_stat_statements statistics epoch
// of each query_store sample so windows that span a statistics reset or a
// server restart are refused even after counters regrow (R10). Existing
// rows keep NULL (unknown epoch): windows mixing them with new samples are
// treated as incomparable, never as measured.
// The catalog is checked first: ALTER TABLE takes ACCESS EXCLUSIVE before it
// looks for the column, so on every bootstrap it would queue behind any
// long reader of the table, and every writer behind it.
const ddlQueryStoreStatsEpoch = `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
                   WHERE attrelid = 'sage.query_store'::regclass
                     AND attname = 'stats_epoch' AND NOT attisdropped) THEN
        ALTER TABLE sage.query_store ADD COLUMN stats_epoch timestamptz;
    END IF;
END
$$;
`
