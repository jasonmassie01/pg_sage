package schema

// ddlNotificationManagedBy marks the notification channels and rules the
// YAML alerting reconciler owns (alerting-06b): managed_by is 'api' for
// rows written through the API and 'config' for rows the reconciler
// writes from the alerting: block, and config_hash is the hash of the
// config the row was written from, so an unchanged block writes nothing.
// The API refuses to change a 'config' row; the reconciler touches only
// those. The partial indexes keep its scan off the API rows.
const ddlNotificationManagedBy = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.notification_channels
    ADD COLUMN IF NOT EXISTS managed_by text NOT NULL DEFAULT 'api'
        CHECK (managed_by IN ('api', 'config')),
    ADD COLUMN IF NOT EXISTS config_hash text;
ALTER TABLE sage.notification_rules
    ADD COLUMN IF NOT EXISTS managed_by text NOT NULL DEFAULT 'api'
        CHECK (managed_by IN ('api', 'config')),
    ADD COLUMN IF NOT EXISTS config_hash text;
CREATE INDEX IF NOT EXISTS idx_notification_channels_config
    ON sage.notification_channels (name) WHERE managed_by = 'config';
CREATE INDEX IF NOT EXISTS idx_notification_rules_config
    ON sage.notification_rules (channel_id) WHERE managed_by = 'config';
`
