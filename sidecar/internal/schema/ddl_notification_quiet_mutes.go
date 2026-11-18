package schema

// ddlNotificationQuietAndMutes (ux-09b) adds:
//   - a channel's own quiet hours (quiet_start, quiet_end HH:MM in
//     quiet_tz); NULL keeps the global alerting.quiet_hours_* pair;
//   - sage.notification_mutes, one row per database and class (the empty
//     class is every class), active while until is in the future; a mute lasts at
//     most 90 days;
//   - item_refs on the delivery log ("queue:<id>"), and snoozed_at on the
//     queue, so the weekly line joins a message to the decision it led to.
//
// Every change is additive and nullable (no rewrite), under lock_timeout.
const ddlNotificationQuietAndMutes = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.notification_channels
    ADD COLUMN IF NOT EXISTS quiet_start text,
    ADD COLUMN IF NOT EXISTS quiet_end text,
    ADD COLUMN IF NOT EXISTS quiet_tz text;
CREATE TABLE IF NOT EXISTS sage.notification_mutes (
    id         bigserial PRIMARY KEY,
    database   text NOT NULL CHECK (length(database) BETWEEN 1 AND 256),
    class      text NOT NULL DEFAULT ''
               CHECK (class IN ('', 'page', 'ask', 'digest')),
    until      timestamptz NOT NULL,
    actor      text NOT NULL CHECK (length(actor) BETWEEN 1 AND 256),
    reason     text NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (until > created_at AND until <= created_at + interval '90 days')
);
CREATE UNIQUE INDEX IF NOT EXISTS notification_mutes_database_class
    ON sage.notification_mutes (database, class);
ALTER TABLE sage.notification_log
    ADD COLUMN IF NOT EXISTS item_refs text[];
ALTER TABLE sage.action_queue
    ADD COLUMN IF NOT EXISTS snoozed_at timestamptz;
`
