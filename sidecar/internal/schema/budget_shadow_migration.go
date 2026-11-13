package schema

// ddlBudgetShadow (market-08) creates the workload budget tables:
//
//   - sage.budget_shadow, the warn-mode ledger: per database, budget,
//     budget definition (its match and limits: a changed budget starts a
//     new ledger) and UTC day, the statements a budget would have cancelled (each counted
//     once), the most connections it would have rejected at once, the
//     would-cancels of protected applications, the longest matching
//     statement and when the budget was first measured that day
//     (first_seen: the 7-day promotion window starts there);
//   - sage.workload_role_daily, per database, role and day, the role's
//     estimated p99 and longest statement time, the evidence of
//     statement_timeout proposals (14 days).
//
// Both are kept for retention.findings_days. IF NOT EXISTS makes a
// re-run take no lock; lock_timeout bounds the first run's wait.
const ddlBudgetShadow = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.budget_shadow (
    database_name     text NOT NULL,
    budget            text NOT NULL,
    definition        text NOT NULL DEFAULT '',
    day               date NOT NULL,
    would_cancel      bigint NOT NULL DEFAULT 0 CHECK (would_cancel >= 0),
    would_reject      integer NOT NULL DEFAULT 0 CHECK (would_reject >= 0),
    protected_cancels bigint NOT NULL DEFAULT 0 CHECK (protected_cancels >= 0),
    worst_ms          double precision NOT NULL DEFAULT 0 CHECK (worst_ms >= 0),
    first_seen        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, budget, definition, day)
);
CREATE INDEX IF NOT EXISTS budget_shadow_updated_at_idx
    ON sage.budget_shadow (updated_at);
CREATE TABLE IF NOT EXISTS sage.workload_role_daily (
    database_name   text NOT NULL,
    role_name       text NOT NULL,
    day             date NOT NULL,
    p99_estimate_ms double precision NOT NULL DEFAULT 0 CHECK (p99_estimate_ms >= 0),
    max_exec_ms     double precision NOT NULL DEFAULT 0 CHECK (max_exec_ms >= 0),
    calls           bigint NOT NULL DEFAULT 0 CHECK (calls >= 0),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, role_name, day)
);
CREATE INDEX IF NOT EXISTS workload_role_daily_updated_at_idx
    ON sage.workload_role_daily (updated_at);
`
