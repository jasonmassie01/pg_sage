package schema

// ddlNotificationLogDatabase names the database and the dedup key of each
// delivery in sage.notification_log (alerting-06c), so the one
// notification log tells a fleet operator which database a delivery was
// for. Rows written before this step keep NULL, read as "unknown"; a
// fleet-wide event also writes NULL. The index serves the log read
// filtered by database, newest first.
const ddlNotificationLogDatabase = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.notification_log
    ADD COLUMN IF NOT EXISTS database text,
    ADD COLUMN IF NOT EXISTS dedup_key text;
CREATE INDEX IF NOT EXISTS idx_notification_log_database_sent
    ON sage.notification_log (database, sent_at DESC);
`
