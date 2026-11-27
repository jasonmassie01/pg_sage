package schema

// ddlActionQueueSchedule lets an operator approval outside the maintenance
// window wait for the next opening (policy-04): status 'scheduled' (status
// is text, so no CHECK changes), who scheduled it and when, when it may
// start and when that window closes. The partial index serves the runner's
// due-row claim. Idempotent: every column and the index are guarded.
const ddlActionQueueSchedule = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.action_queue
    ADD COLUMN IF NOT EXISTS scheduled_by int,
    ADD COLUMN IF NOT EXISTS scheduled_at timestamptz,
    ADD COLUMN IF NOT EXISTS run_after timestamptz,
    ADD COLUMN IF NOT EXISTS window_closes_at timestamptz;
CREATE INDEX IF NOT EXISTS idx_action_queue_scheduled_due
    ON sage.action_queue (run_after)
    WHERE status = 'scheduled';
`
