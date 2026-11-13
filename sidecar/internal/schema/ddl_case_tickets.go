package schema

// ddlCaseTickets links a case to one ticket per ticket system (market-14),
// on the control database. A row without external_key is a claim held
// while the issue is created. status and status_category are read back
// from the ticket system as evidence only; sync_error is the class of
// the last failed read (unauthorized, not_found, integration_missing,
// unreachable). Deleting the integration keeps the link.
const ddlCaseTickets = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.case_tickets (
    id                  bigserial PRIMARY KEY,
    database            text NOT NULL,
    case_identity_key   text NOT NULL,
    system              text NOT NULL CHECK (system IN ('jira')),
    channel_id          int REFERENCES sage.notification_channels(id) ON DELETE SET NULL,
    external_key        text,
    url                 text,
    status              text NOT NULL DEFAULT 'creating',
    status_category     text NOT NULL DEFAULT '',
    sync_error          text NOT NULL DEFAULT '',
    resolved_comment_at timestamptz,
    created_by          text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_synced_at      timestamptz,
    UNIQUE (database, case_identity_key, system)
);
CREATE INDEX IF NOT EXISTS idx_case_tickets_sync
    ON sage.case_tickets (last_synced_at NULLS FIRST, id)
    WHERE external_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_tickets_channel
    ON sage.case_tickets (channel_id);
`
