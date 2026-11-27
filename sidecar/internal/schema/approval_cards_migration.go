package schema

// Approval cards (roadmap 1.5). All idempotent.
//
// sage.approval_card_deliveries records each approval card sent to a chat
// channel: the SHA-256 of its single-use button token (the token itself is
// never stored), the queue item and card content hash it approves, when it
// expires, who used it with which decision, the chat message it was used
// from, and whether the verification verdict was posted back. It lives in
// the notification control database next to the channels. The follow-up
// queue's index comes with its next_check_at column
// (ddlApprovalCardNextCheck).
//
// sage.action_queue gains the operator's snooze: until when, by whom and
// why. A snoozed item stays pending; autonomy does not take it over until
// the snooze ends, when the card is sent again.
const ddlApprovalCards = `
CREATE TABLE IF NOT EXISTS sage.approval_card_deliveries (
    id               bigserial PRIMARY KEY,
    token_sha256     bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    channel_id       integer NOT NULL CHECK (channel_id > 0),
    database_name    text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 128),
    queue_id         integer NOT NULL CHECK (queue_id > 0),
    card_hash        text NOT NULL CHECK (length(card_hash) BETWEEN 1 AND 128),
    title            text NOT NULL DEFAULT '' CHECK (length(title) <= 512),
    summary          text NOT NULL DEFAULT '' CHECK (length(summary) <= 512),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    used_at          timestamptz,
    used_by          integer,
    decision         text CHECK (decision IN ('approve', 'deny', 'snooze')),
    message_id       bigint NOT NULL DEFAULT 0,
    followed_up_at   timestamptz,
    followup_verdict text NOT NULL DEFAULT '' CHECK (length(followup_verdict) <= 64),
    CHECK ((used_at IS NULL) = (decision IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_approval_card_closed
    ON sage.approval_card_deliveries (followed_up_at) WHERE followed_up_at IS NOT NULL;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS snoozed_until timestamptz;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS snoozed_by integer;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS snooze_reason text;
CREATE INDEX IF NOT EXISTS idx_action_queue_snoozed
    ON sage.action_queue (snoozed_until) WHERE snoozed_until IS NOT NULL;
`

// ddlApprovalFollowupClaim adds the follow-up claim (fleet-03): the leader
// epoch that claimed a card's follow-up and when, so a leader handover in
// the middle of a cycle cannot post the same follow-up twice.
// Catalog-first, so a re-run takes no lock.
const ddlApprovalFollowupClaim = `
SET LOCAL lock_timeout = '5s';
DO $approval_followup_claim$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.approval_card_deliveries'::regclass
           AND attname = 'followup_claim_epoch' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.approval_card_deliveries
            ADD COLUMN followup_claim_epoch bigint;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.approval_card_deliveries'::regclass
           AND attname = 'followup_claimed_at' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.approval_card_deliveries
            ADD COLUMN followup_claimed_at timestamptz;
    END IF;
END
$approval_followup_claim$;
`
