package schema

// ddlPlanCardDeliveries (plan-03b) records the plan cards sent to chat
// channels, modeled on sage.fact_card_deliveries: one row per card per
// channel, keyed by the SHA-256 of its single-use token. A card names the
// plan and the plan hash it showed, the decisions it offered (always
// reject; approve_all, approve_up_to and approve_next as the plan's risk
// tiers allow from chat) and, with approve_up_to, the last ordinal that
// button covers. Once used it holds who decided what from which message.
//
// CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS only;
// lock_timeout bounds the catalog locks on the first run.
const ddlPlanCardDeliveries = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.plan_card_deliveries (
    id               bigserial PRIMARY KEY,
    token_sha256     bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    channel_id       integer NOT NULL CHECK (channel_id > 0),
    database_name    text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 128),
    plan_id          bigint NOT NULL CHECK (plan_id > 0),
    plan_hash        text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    title            text NOT NULL DEFAULT '' CHECK (length(title) <= 512),
    offered_modes    text[] NOT NULL
                         CHECK (offered_modes <@ ARRAY['approve_all', 'approve_up_to',
                                'approve_next', 'reject']::text[]
                                AND 'reject' = ANY (offered_modes)),
    up_to_ordinal    integer CHECK (up_to_ordinal BETWEEN 1 AND 50),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    used_at          timestamptz,
    used_by          integer,
    decision         text CHECK (decision IN ('approve_all', 'approve_up_to',
                                              'approve_next', 'reject')),
    message_id       bigint NOT NULL DEFAULT 0,
    CHECK ((used_at IS NULL) = (decision IS NULL)),
    CHECK (('approve_up_to' = ANY (offered_modes)) = (up_to_ordinal IS NOT NULL)),
    CHECK (decision IS NULL OR decision = ANY (offered_modes))
);
CREATE INDEX IF NOT EXISTS idx_plan_card_created
    ON sage.plan_card_deliveries (created_at);
`
