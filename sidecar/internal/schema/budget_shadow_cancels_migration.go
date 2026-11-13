package schema

// ddlBudgetShadowActualCancels (market-08b) adds to sage.budget_shadow the
// real statement-timeout cancels of a budget's role that day, counted from
// the server log (SQLSTATE 57014, not a client or autovacuum cancel):
//
//   - actual_cancels, written in warn mode too, so the 7 promotion days
//     hold a pre-enforcement baseline for the revert prediction;
//   - cancel_source, 'log' while pg_sage reads the server log and 'none'
//     without one (an enforced timeout then cannot be auto-reverted, so it
//     needs approval);
//   - cancels_lower_bound, true when the log subscription dropped entries
//     that day, so the count may be short.
//
// ADD COLUMN IF NOT EXISTS with constant defaults takes no table rewrite;
// lock_timeout bounds the wait for the table's lock.
const ddlBudgetShadowActualCancels = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.budget_shadow
    ADD COLUMN IF NOT EXISTS actual_cancels bigint NOT NULL DEFAULT 0
        CONSTRAINT budget_shadow_actual_cancels_check CHECK (actual_cancels >= 0),
    ADD COLUMN IF NOT EXISTS cancel_source text NOT NULL DEFAULT 'none'
        CONSTRAINT budget_shadow_cancel_source_check
        CHECK (cancel_source IN ('none', 'log')),
    ADD COLUMN IF NOT EXISTS cancels_lower_bound boolean NOT NULL DEFAULT false;
`
