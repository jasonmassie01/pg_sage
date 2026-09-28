package schema

// Durable recommendation state machine (MASTER-SPEC §5.1 #5, Codex
// C04/C07/C15). sage.recommendation is the head row; identity is
// database + category + canonical target + action type + index
// fingerprint (C05), hashed into identity_key, with at most one live head
// per identity. Revisions and transitions are immutable: an UPDATE on
// either raises. Retention deletes terminal heads, and their revisions and
// history go with them (ON DELETE CASCADE).
const ddlRecommendation = `
CREATE TABLE IF NOT EXISTS sage.recommendation (
    id                bigserial PRIMARY KEY,
    identity_key      text NOT NULL CHECK (length(identity_key) BETWEEN 1 AND 64),
    database_name     text NOT NULL,
    category          text NOT NULL,
    target            text NOT NULL,
    action_type       text NOT NULL,
    index_fingerprint text NOT NULL DEFAULT '',
    finding_id        bigint REFERENCES sage.findings(id) ON DELETE SET NULL,
    state             text NOT NULL CHECK (state IN (
        'proposed', 'approved', 'applying', 'applied', 'verifying', 'verified',
        'reverted', 'inconclusive', 'superseded', 'failed', 'abandoned')),
    revision          integer NOT NULL CHECK (revision > 0),
    content_hash      text NOT NULL,
    approved_revision integer,
    approved_hash     text,
    approved_by       text,
    approved_at       timestamptz,
    attempt_count     integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    retry_budget      integer NOT NULL CHECK (retry_budget >= 0),
    next_attempt_at   timestamptz,
    lease_until       timestamptz,
    action_log_id     bigint REFERENCES sage.action_log(id) ON DELETE SET NULL,
    verdict           text,
    reason            text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((approved_revision IS NULL) = (approved_hash IS NULL)),
    CHECK ((approved_hash IS NULL) = (approved_by IS NULL)),
    CHECK (approved_revision IS NULL OR approved_revision <= revision),
    CHECK (state NOT IN ('approved', 'applying', 'applied', 'verifying')
           OR approved_hash IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS recommendation_one_live
    ON sage.recommendation (identity_key)
    WHERE state IN ('proposed', 'approved', 'applying', 'applied', 'verifying', 'failed');
CREATE INDEX IF NOT EXISTS recommendation_actionable
    ON sage.recommendation (database_name, state, next_attempt_at);
CREATE INDEX IF NOT EXISTS recommendation_identity
    ON sage.recommendation (identity_key, id DESC);
CREATE INDEX IF NOT EXISTS recommendation_finding
    ON sage.recommendation (finding_id) WHERE finding_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS recommendation_action
    ON sage.recommendation (action_log_id) WHERE action_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS recommendation_terminal_age
    ON sage.recommendation (updated_at)
    WHERE state IN ('verified', 'reverted', 'inconclusive', 'superseded', 'abandoned');
`

const ddlRecommendationRevision = `
CREATE TABLE IF NOT EXISTS sage.recommendation_revision (
    recommendation_id bigint NOT NULL
        REFERENCES sage.recommendation(id) ON DELETE CASCADE,
    revision          integer NOT NULL CHECK (revision > 0),
    content_hash      text NOT NULL,
    forward_sql       text NOT NULL CHECK (length(forward_sql) > 0),
    inverse_sql       text NOT NULL DEFAULT '',
    evidence          jsonb NOT NULL DEFAULT '{}'::jsonb,
    preconditions     jsonb NOT NULL DEFAULT '{}'::jsonb,
    policy_version    bigint,
    title             text NOT NULL DEFAULT '',
    severity          text NOT NULL DEFAULT '',
    object_type       text NOT NULL DEFAULT '',
    action_risk       text NOT NULL DEFAULT '',
    recommendation    text NOT NULL DEFAULT '',
    source            text NOT NULL CHECK (source IN ('analyzer', 'migrated')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recommendation_id, revision)
);
` + ddlImmutableFn + `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger
                    WHERE tgname = 'recommendation_revision_immutable'
                      AND tgrelid = 'sage.recommendation_revision'::regclass) THEN
        CREATE TRIGGER recommendation_revision_immutable
            BEFORE UPDATE ON sage.recommendation_revision
            FOR EACH ROW EXECUTE FUNCTION sage.recommendation_immutable();
    END IF;
END $$;
`

const ddlRecommendationTransition = `
CREATE TABLE IF NOT EXISTS sage.recommendation_transition (
    id                bigserial PRIMARY KEY,
    recommendation_id bigint NOT NULL
        REFERENCES sage.recommendation(id) ON DELETE CASCADE,
    from_state        text,
    to_state          text NOT NULL,
    revision          integer NOT NULL CHECK (revision > 0),
    actor             text NOT NULL,
    reason            text NOT NULL DEFAULT '',
    action_log_id     bigint,
    created_at        timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS recommendation_transition_history
    ON sage.recommendation_transition (recommendation_id, id);
` + ddlImmutableFn + `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger
                    WHERE tgname = 'recommendation_transition_immutable'
                      AND tgrelid = 'sage.recommendation_transition'::regclass) THEN
        CREATE TRIGGER recommendation_transition_immutable
            BEFORE UPDATE ON sage.recommendation_transition
            FOR EACH ROW EXECUTE FUNCTION sage.recommendation_immutable();
    END IF;
END $$;
`

const ddlImmutableFn = `
CREATE OR REPLACE FUNCTION sage.recommendation_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'sage.% rows are immutable', TG_TABLE_NAME;
END $$;
`

// ddlActionQueueRecommendation pins a queued approval to one revision.
const ddlActionQueueRecommendation = `
ALTER TABLE sage.action_queue
    ADD COLUMN IF NOT EXISTS recommendation_id bigint,
    ADD COLUMN IF NOT EXISTS recommendation_revision integer,
    ADD COLUMN IF NOT EXISTS content_hash text;
CREATE INDEX IF NOT EXISTS idx_action_queue_recommendation
    ON sage.action_queue (recommendation_id, status)
    WHERE recommendation_id IS NOT NULL;
`

// ddlRecommendationAll installs every recommendation object, in FK order.
const ddlRecommendationAll = ddlRecommendation + ddlRecommendationRevision +
	ddlRecommendationTransition + ddlActionQueueRecommendation
