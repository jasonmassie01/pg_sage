package schema

// ddlAdvisorRuns persists each configuration sub-advisor's schedule
// (advisors-02). The in-memory last-run time was lost on every restart (a
// restart re-spent the model budget) and was set even when every
// sub-advisor failed. One row per database and sub-advisor: a run counts
// only on success; a failure backs off. The row is the claim two
// concurrent cycles take before one model call. CREATE IF NOT EXISTS
// takes no lock on a re-run; lock_timeout bounds the first.
const ddlAdvisorRuns = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.advisor_runs (
    database_name        text NOT NULL DEFAULT current_database(),
    sub_advisor          text NOT NULL,
    last_success_at      timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0
        CHECK (consecutive_failures >= 0),
    next_attempt_at      timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, sub_advisor)
);
`
