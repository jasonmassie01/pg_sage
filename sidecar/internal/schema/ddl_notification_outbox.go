package schema

// ddlNotificationOutbox is the notification outbox and the per-channel
// alert state of the converged notification stack (alerting-06), on the
// control database. An entry waits until next_attempt_at (retry backoff,
// quiet hours, cooldown); claimed_until is the lease of the sidecar
// sending it. A state row is what one channel was last told about one
// alert of one database.
const ddlNotificationOutbox = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.notification_outbox (
    id              bigserial PRIMARY KEY,
    database        text NOT NULL DEFAULT '',
    dedup_key       text NOT NULL,
    channel_id      int NOT NULL REFERENCES sage.notification_channels(id)
                    ON DELETE CASCADE,
    event           text NOT NULL,
    payload         jsonb NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    last_error      text,
    claimed_until   timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_due
    ON sage.notification_outbox (next_attempt_at, id)
    WHERE status IN ('pending', 'sending');
CREATE INDEX IF NOT EXISTS idx_notification_outbox_channel
    ON sage.notification_outbox (channel_id);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_key
    ON sage.notification_outbox (database, dedup_key, channel_id)
    WHERE status = 'pending';
CREATE TABLE IF NOT EXISTS sage.notification_state (
    database      text NOT NULL DEFAULT '',
    dedup_key     text NOT NULL,
    channel_id    int NOT NULL REFERENCES sage.notification_channels(id)
                  ON DELETE CASCADE,
    open          boolean NOT NULL DEFAULT false,
    severity      text NOT NULL DEFAULT '',
    first_sent_at timestamptz,
    last_sent_at  timestamptz,
    resolved_at   timestamptz,
    reminders     int NOT NULL DEFAULT 0,
    payload       jsonb,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database, dedup_key, channel_id)
);
CREATE INDEX IF NOT EXISTS idx_notification_state_channel
    ON sage.notification_state (channel_id);
CREATE INDEX IF NOT EXISTS idx_notification_state_open
    ON sage.notification_state (last_sent_at) WHERE open;
`
