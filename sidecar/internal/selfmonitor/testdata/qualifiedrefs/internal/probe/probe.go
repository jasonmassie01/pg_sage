package probe

// Fixture for the strict qualified-refs check: one bare reference (line 7)
// and one marked prose literal (line 12), which passes.

const bareSQL = `SELECT calls
  FROM pg_stat_statements`

const qualifiedSQL = `SELECT calls FROM "ext"."pg_stat_statements"`

//qualifiedrefs:prose tool description for the model
const description = "Plan-only EXPLAIN of a statement named by its pg_stat_statements queryid"
