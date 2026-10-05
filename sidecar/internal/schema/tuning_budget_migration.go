package schema

// Tuning agent daily budget (roadmap 2.2). Idempotent.
//
// sage.tuning_budget_day is the model spend the tuning agent charged per
// database and UTC day, so its daily token cap survives restarts (an
// in-memory counter granted a fresh day on every restart). One row per
// database and day, incremented after each case the agent asks about.
const ddlTuningBudgetDay = `
CREATE TABLE IF NOT EXISTS sage.tuning_budget_day (
    database_name text NOT NULL DEFAULT current_database(),
    utc_day       date NOT NULL,
    tokens_used   bigint NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    requests_used bigint NOT NULL DEFAULT 0 CHECK (requests_used >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, utc_day)
);
CREATE INDEX IF NOT EXISTS idx_tuning_budget_day_updated
    ON sage.tuning_budget_day (updated_at);
`
