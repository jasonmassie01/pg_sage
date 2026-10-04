package schema

// Cheap observer (roadmap phase 3, perf 2026-10-04): the Trust page must
// not read whole history tables.
//
//   - The Trust view's family safety read counted harmful outcomes by
//     fetching every outcome of the 30-day window (20,000 in the small
//     perf gate, twice per view); the partial index holds only harmful
//     and unsafe outcomes, which are rare.
//   - The Trust page's shadow summary counts every shadow decision by
//     class; the narrow covering index serves it in group order without
//     reading the wide decision rows (SQL, prediction, reasons).
//
// Plain CREATE INDEX, as ddlPerfIndexes: bootstrap runs once at startup
// under its advisory lock, on pg_sage's own tables. Idempotent.
const ddlObserverIndexes = `
CREATE INDEX IF NOT EXISTS idx_sre_autonomy_outcomes_harmful
    ON sage.sre_autonomy_outcomes (deployment_id, database_name, family, recorded_at DESC)
    WHERE result IN ('harmful', 'safety_violation');
CREATE INDEX IF NOT EXISTS idx_shadow_decision_class_summary
    ON sage.shadow_decision (family, action_class)
    INCLUDE (status, score, counted, recorded_at);
`
