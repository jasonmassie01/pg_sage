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
`
