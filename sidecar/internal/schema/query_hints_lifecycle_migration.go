package schema

// ddlQueryHintsLifecycle adds the columns hint revalidation needs to
// survive a pg_stat_statements reset and to stop retiring hints by age
// (tuning-01):
//   - pgss_reset_at: pg_stat_statements_info.stats_reset at the last pass;
//     a newer value means the counters restarted, so the calls checkpoint
//     is re-baselined instead of reading as "no new executions".
//   - reverify_due: when the hint passed tuner.hint_retirement_days; the
//     hint stays installed until a re-verification decides (tuning-05).
//
// status gains the value 'dormant' (no CHECK constraint to change). The
// statements run in one implicit transaction, so SET LOCAL bounds the
// ALTERs' lock wait on a busy sage.query_hints.
const ddlQueryHintsLifecycle = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS pgss_reset_at timestamptz;
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS reverify_due timestamptz;
`
