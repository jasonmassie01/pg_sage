package schema

// ddlNotificationRuleClass lets a notification rule route by delivery
// class (ux-09a): page, ask or digest. NULL is a legacy rule of one event
// type. A class rule's event is "class:<class>", which no event type is.
const ddlNotificationRuleClass = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.notification_rules
    ADD COLUMN IF NOT EXISTS delivery_class text
        CHECK (delivery_class IS NULL OR delivery_class IN ('page', 'ask', 'digest'));
CREATE INDEX IF NOT EXISTS idx_notification_rules_class
    ON sage.notification_rules (delivery_class)
    WHERE enabled = true AND delivery_class IS NOT NULL;
`
