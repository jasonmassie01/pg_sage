package schema

// Sage SRE M5 actions. All idempotent.
//
// sage.sre_action_proposals: one durable action proposal per
// investigation and action class, linked to the investigation (deleted
// with it by retention), with the target identity it was derived from,
// its repair contract, policy verdict, approval item, execution and
// recovery record. At most one proposal per database executes at a time.
//
// sage.chatops_identities maps an authenticated chat user to a pg_sage
// user (deleting the user removes the mapping); sage.chatops_replay
// records processed callbacks so a delivery is acted on once.
//
// The investigation event chain gains the action event types. Other
// milestones add event types in parallel, so the check is rebuilt as the
// union of every event-type check present, under the M3 name.
const ddlSREActions = ddlSREActionProposals + ddlChatOps + ddlSREActionEventTypes

const ddlSREActionProposals = `
CREATE TABLE IF NOT EXISTS sage.sre_action_proposals (
    id               uuid PRIMARY KEY,
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    action_class     text NOT NULL CHECK (action_class IN ('cancel_backend')),
    family           text NOT NULL DEFAULT '' CHECK (length(family) <= 64),
    node_id          text NOT NULL DEFAULT '' CHECK (length(node_id) <= 64),
    state            text NOT NULL CHECK (state IN ('ineligible', 'proposed',
        'requested', 'executing', 'executed', 'refused', 'failed', 'uncertain',
        'denied', 'expired')),
    reason           text NOT NULL DEFAULT '' CHECK (length(reason) <= 64),
    detail           text NOT NULL DEFAULT '' CHECK (length(detail) <= 2048),
    evidence_ids     uuid[] NOT NULL DEFAULT '{}',
    target           jsonb CHECK (target IS NULL OR jsonb_typeof(target) = 'object'),
    target_sha256    bytea CHECK (target_sha256 IS NULL OR octet_length(target_sha256) = 32),
    baseline         jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(baseline) = 'object'),
    contract         jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(contract) = 'object'),
    policy           jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(policy) = 'object'),
    queue_id         integer,
    finding_id       bigint,
    action_log_id    bigint,
    requested_by     text NOT NULL DEFAULT '' CHECK (length(requested_by) <= 128),
    requested_at     timestamptz,
    decided_by       integer,
    decided_at       timestamptz,
    executed_at      timestamptz,
    recovery         jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(recovery) = 'object' AND octet_length(recovery::text) <= 65536),
    recovery_state   text NOT NULL DEFAULT '' CHECK (recovery_state IN ('', 'observing',
        'recovered', 'not_recovered', 'inconclusive')),
    next_sample_at   timestamptz,
    version          bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at       timestamptz NOT NULL,
    UNIQUE (deployment_id, database_id, investigation_id, action_class),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sre_action_proposals_state
    ON sage.sre_action_proposals (deployment_id, database_id, state, updated_at);
CREATE INDEX IF NOT EXISTS idx_sre_action_proposals_recovery
    ON sage.sre_action_proposals (deployment_id, database_id, next_sample_at)
    WHERE recovery_state = 'observing';
CREATE UNIQUE INDEX IF NOT EXISTS idx_sre_action_proposals_one_executing
    ON sage.sre_action_proposals (deployment_id, database_id)
    WHERE state = 'executing';
`

const ddlChatOps = `
CREATE TABLE IF NOT EXISTS sage.chatops_identities (
    id               serial PRIMARY KEY,
    provider         text NOT NULL CHECK (provider IN ('slack', 'telegram')),
    team_id          text NOT NULL DEFAULT '' CHECK (length(team_id) <= 64),
    external_user_id text NOT NULL CHECK (length(external_user_id) BETWEEN 1 AND 128),
    user_id          integer NOT NULL REFERENCES sage.users (id) ON DELETE CASCADE,
    created_by       integer,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, team_id, external_user_id)
);
CREATE TABLE IF NOT EXISTS sage.chatops_replay (
    provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 32),
    nonce    text NOT NULL CHECK (length(nonce) BETWEEN 1 AND 256),
    seen_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, nonce)
);
CREATE INDEX IF NOT EXISTS idx_chatops_replay_seen ON sage.chatops_replay (seen_at);
`

const ddlSREActionEventTypes = `
DO $$
DECLARE
    wanted  text[] := ARRAY['action_proposed', 'action_requested', 'action_decided',
        'action_recheck', 'action_refused', 'action_executed', 'action_failed',
        'recovery_sample', 'recovery_verdict'];
    allowed text[] := '{}';
    checks  int := 0;
    named   boolean := false;
    c       record;
BEGIN
    FOR c IN SELECT conname, pg_get_constraintdef(oid) AS def FROM pg_constraint
             WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%' LOOP
        checks := checks + 1;
        named := named OR c.conname = 'sre_events_event_type_m3';
        allowed := allowed || ARRAY(SELECT (regexp_matches(c.def,
            '''([a-z0-9_]+)''', 'g'))[1]);
    END LOOP;
    IF checks = 1 AND named AND wanted <@ allowed THEN
        RETURN;
    END IF;
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%' LOOP
        EXECUTE format('ALTER TABLE sage.sre_events DROP CONSTRAINT %I', c.conname);
    END LOOP;
    EXECUTE (SELECT format('ALTER TABLE sage.sre_events ADD CONSTRAINT '
        'sre_events_event_type_m3 CHECK (event_type IN (%s)) NOT VALID',
        string_agg(quote_literal(v), ', ' ORDER BY v))
        FROM (SELECT DISTINCT unnest(allowed || wanted) AS v) u);
    ALTER TABLE sage.sre_events VALIDATE CONSTRAINT sre_events_event_type_m3;
END $$;
`
