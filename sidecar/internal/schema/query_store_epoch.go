package schema

// ddlQueryStoreStatsEpoch records the pg_stat_statements statistics epoch
// of each query_store sample so windows that span a statistics reset or a
// server restart are refused even after counters regrow (R10). Existing
// rows keep NULL (unknown epoch): windows mixing them with new samples are
// treated as incomparable, never as measured.
const ddlQueryStoreStatsEpoch = `
ALTER TABLE sage.query_store
    ADD COLUMN IF NOT EXISTS stats_epoch timestamptz;
`
