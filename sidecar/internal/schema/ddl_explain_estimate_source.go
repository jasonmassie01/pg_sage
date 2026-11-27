package schema

// ddlExplainEstimateSource (rca-04) renames the on-demand plain EXPLAIN's
// rows from 'auto_explain' to 'explain_estimate': they are estimates that
// auto_explain never saw. The executed plans of the auto_explain log stay
// 'auto_explain_log'. Idempotent: a second run finds no old rows. The row
// lock wait is bounded so a busy cache cannot hold up a bootstrap.
const ddlExplainEstimateSource = `
SET LOCAL lock_timeout = '5s';
UPDATE sage.explain_cache SET source = 'explain_estimate'
 WHERE source = 'auto_explain';
`
