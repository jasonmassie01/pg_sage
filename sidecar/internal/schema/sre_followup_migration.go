package schema

// sre-08: the diagnosis follow-up. sage.notification_log records the
// incident a delivery belongs to (ref_id) and the id the provider gave the
// message (provider_message_id, the Telegram message_id), so a follow-up
// reaches exactly the channels the page reached and replies to it. Both
// are nullable: older rows and events of no incident keep NULL. The index
// serves the follow-up's page lookup. Additive and idempotent; lock_timeout
// bounds the wait for notification_log's lock.
const ddlNotificationLogRefs = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.notification_log
    ADD COLUMN IF NOT EXISTS ref_id text,
    ADD COLUMN IF NOT EXISTS provider_message_id text;
CREATE INDEX IF NOT EXISTS idx_notification_log_ref
    ON sage.notification_log (ref_id) WHERE ref_id IS NOT NULL;
`

// ddlSREIncidentFollowup is the follow-up claim, one row per incident in
// the SRE store's database: whichever sidecar inserts it first sends the
// incident's diagnosis, and a later re-run finds it and sends nothing.
// delivered_at stays NULL when the delivery failed (no retry). The index
// serves the notifier's search for an incident's settled conclusions and
// live runs. After ddlSREInvestigator (source_incident_id, concluded_at).
const ddlSREIncidentFollowup = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.sre_incident_followup (
    incident_id      text PRIMARY KEY CHECK (length(incident_id) BETWEEN 1 AND 256),
    investigation_id uuid NOT NULL,
    claimed_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivered_at     timestamptz
);
CREATE INDEX IF NOT EXISTS sre_investigation_incident
    ON sage.sre_investigations (deployment_id, database_id, source_incident_id, concluded_at)
    WHERE source_incident_id IS NOT NULL;
`
