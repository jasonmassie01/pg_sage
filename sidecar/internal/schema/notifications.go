package schema

const ddlNotificationChannels = `
CREATE TABLE IF NOT EXISTS sage.notification_channels (
    id          SERIAL PRIMARY KEY,
    name        TEXT UNIQUE NOT NULL,
    type        TEXT NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}',
    enabled     BOOLEAN DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT now(),
    created_by  INT
);
`

const ddlNotificationRules = `
CREATE TABLE IF NOT EXISTS sage.notification_rules (
    id           SERIAL PRIMARY KEY,
    channel_id   INT REFERENCES sage.notification_channels(id)
                 ON DELETE CASCADE,
    event        TEXT NOT NULL,
    min_severity TEXT DEFAULT 'warning',
    enabled      BOOLEAN DEFAULT true,
    created_at   TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_rules_event_enabled
    ON sage.notification_rules (event, min_severity)
    WHERE enabled = true;
`

const ddlNotificationLog = `
CREATE TABLE IF NOT EXISTS sage.notification_log (
    id          SERIAL PRIMARY KEY,
    channel_id  INT REFERENCES sage.notification_channels(id)
                ON DELETE SET NULL,
    event       TEXT NOT NULL,
    subject     TEXT NOT NULL,
    body        TEXT,
    status      TEXT NOT NULL DEFAULT 'pending',
    error       TEXT,
    sent_at     TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_log_sent_at
    ON sage.notification_log (sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_notification_log_channel_sent
    ON sage.notification_log (channel_id, sent_at DESC)
    WHERE channel_id IS NOT NULL;
`
