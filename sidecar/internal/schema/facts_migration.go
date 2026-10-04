package schema

// Binding facts (roadmap 2.3). All idempotent.
//
// sage.facts holds the typed facts of the monitored database: what the
// subject is (an index, table, schema or replication slot, by a validated
// pattern), what is true of it (owned by the application's migrations, a
// test fixture, a slot's consumer, append-only, a table's window), who
// proposed it (the model, a detector or an operator) with the evidence
// cited, and the operator's decision. One row per (type, subject kind,
// subject): proposals of the same fact dedupe onto it. Only confirmed
// facts bind, and they only narrow or redirect what pg_sage does.
//
// sage.fact_card_deliveries records the fact cards sent to chat channels
// (Confirm / Reject buttons): the SHA-256 of the single-use token, the
// fact and the content hash the decision is bound to. It lives in the
// notification control database next to sage.approval_card_deliveries.
const ddlFacts = `
CREATE TABLE IF NOT EXISTS sage.facts (
    id               bigserial PRIMARY KEY,
    fact_type        text NOT NULL CHECK (fact_type IN ('owned_by_app_migrations',
                         'test_fixture', 'slot_consumer', 'append_only', 'table_window')),
    subject_kind     text NOT NULL CHECK (subject_kind IN ('index', 'table', 'schema',
                         'slot')),
    subject          text NOT NULL CHECK (length(subject) BETWEEN 1 AND 300),
    value            jsonb NOT NULL DEFAULT '{}'::jsonb,
    source           text NOT NULL CHECK (source IN ('model', 'operator', 'detector')),
    proposed_by      text NOT NULL DEFAULT '' CHECK (length(proposed_by) <= 200),
    evidence         jsonb NOT NULL DEFAULT '[]'::jsonb,
    rationale        text NOT NULL DEFAULT '' CHECK (length(rationale) <= 2000),
    status           text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed',
                         'confirmed', 'rejected', 'expired')),
    decided_by       text CHECK (length(decided_by) <= 200),
    decided_at       timestamptz,
    decision_note    text NOT NULL DEFAULT '' CHECK (length(decision_note) <= 1000),
    expires_at       timestamptz,
    expired_reason   text NOT NULL DEFAULT '' CHECK (length(expired_reason) <= 500),
    proposals        integer NOT NULL DEFAULT 1 CHECK (proposals >= 1),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    last_verified_at timestamptz,
    CHECK (status NOT IN ('confirmed', 'rejected')
           OR (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS facts_subject_key
    ON sage.facts (fact_type, subject_kind, subject);
CREATE INDEX IF NOT EXISTS facts_status_updated
    ON sage.facts (status, updated_at);
CREATE TABLE IF NOT EXISTS sage.fact_card_deliveries (
    id               bigserial PRIMARY KEY,
    token_sha256     bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    channel_id       integer NOT NULL CHECK (channel_id > 0),
    database_name    text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 128),
    fact_id          bigint NOT NULL CHECK (fact_id > 0),
    fact_hash        text NOT NULL CHECK (length(fact_hash) BETWEEN 1 AND 128),
    title            text NOT NULL DEFAULT '' CHECK (length(title) <= 512),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    used_at          timestamptz,
    used_by          integer,
    decision         text CHECK (decision IN ('confirm', 'reject')),
    message_id       bigint NOT NULL DEFAULT 0,
    CHECK ((used_at IS NULL) = (decision IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_fact_card_created
    ON sage.fact_card_deliveries (created_at);
`
