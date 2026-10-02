package schema

// Sage SRE M7 earned autonomy (AI-SRE-SPEC §7.3, §10: sre_family_autonomy,
// sre_eval_runs). Levels are per deployment, incident family and action
// class; L4 is reserved and the database refuses it. A promotion moves one
// level and needs a human approval; history and outcomes are append-only.
// The fleet canary records each instance of a rollout. Installed beside the
// sre_* coordination tables (the meta database when configured, otherwise
// the monitored database). Additive and idempotent.
const ddlSREAutonomy = `
CREATE TABLE IF NOT EXISTS sage.sre_family_autonomy (
    deployment_id uuid NOT NULL,
    family        text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class  text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    level         smallint NOT NULL CHECK (level BETWEEN 0 AND 3),
    version       bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    evidence      jsonb NOT NULL DEFAULT '{}'::jsonb,
    changed_by    text NOT NULL CHECK (length(changed_by) BETWEEN 1 AND 200),
    change_reason text NOT NULL CHECK (length(change_reason) BETWEEN 1 AND 1000),
    changed_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, family, action_class)
);

CREATE TABLE IF NOT EXISTS sage.sre_autonomy_proposals (
    deployment_id   uuid NOT NULL,
    id              uuid NOT NULL,
    family          text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class    text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    from_level      smallint NOT NULL CHECK (from_level BETWEEN 0 AND 2),
    to_level        smallint NOT NULL CHECK (to_level BETWEEN 1 AND 3),
    evidence        jsonb NOT NULL,
    evidence_sha256 bytea NOT NULL CHECK (octet_length(evidence_sha256) = 32),
    status          text NOT NULL CHECK (status IN
        ('pending', 'approved', 'rejected', 'expired', 'superseded')),
    proposed_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at      timestamptz NOT NULL,
    decided_by      text,
    decided_at      timestamptz,
    decision_note   text CHECK (length(decision_note) <= 2000),
    PRIMARY KEY (deployment_id, id),
    CHECK (to_level = from_level + 1),
    CHECK (expires_at > proposed_at),
    CHECK ((status = 'pending') = (decided_at IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS sre_autonomy_one_pending
    ON sage.sre_autonomy_proposals (deployment_id, family, action_class)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS sage.sre_autonomy_events (
    id            bigserial PRIMARY KEY,
    deployment_id uuid NOT NULL,
    family        text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class  text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    event_type    text NOT NULL CHECK (event_type IN (
        'promotion_proposed', 'promotion_approved', 'promotion_rejected',
        'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
        'cap_cleared', 'auto_executed')),
    from_level    smallint CHECK (from_level BETWEEN 0 AND 3),
    to_level      smallint CHECK (to_level BETWEEN 0 AND 3),
    actor         text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    reason        text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    database_name text,
    proposal_id   uuid,
    action_log_id bigint,
    evidence      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS sre_autonomy_events_pair
    ON sage.sre_autonomy_events (deployment_id, family, action_class, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS sre_autonomy_one_auto_executed
    ON sage.sre_autonomy_events (deployment_id, database_name, action_log_id)
    WHERE event_type = 'auto_executed';

CREATE TABLE IF NOT EXISTS sage.sre_autonomy_outcomes (
    id            bigserial PRIMARY KEY,
    deployment_id uuid NOT NULL,
    database_name text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    action_log_id bigint,
    family        text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    action_class  text NOT NULL CHECK (action_class ~ '^[a-z][a-z0-9_]{0,63}$'),
    level         smallint NOT NULL CHECK (level BETWEEN 0 AND 3),
    result        text NOT NULL CHECK (result IN
        ('verified_recovery', 'not_recovered', 'harmful', 'safety_violation')),
    source        text NOT NULL CHECK (source IN
        ('executor', 'operator', 'game_day', 'rollout', 'bench')),
    actor         text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    detail        text CHECK (length(detail) <= 2000),
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX IF NOT EXISTS sre_autonomy_outcome_per_action
    ON sage.sre_autonomy_outcomes (deployment_id, database_name, action_log_id, source)
    WHERE action_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sre_autonomy_outcomes_family
    ON sage.sre_autonomy_outcomes (deployment_id, family, recorded_at DESC);

CREATE TABLE IF NOT EXISTS sage.sre_packet_reviews (
    deployment_id    uuid NOT NULL,
    database_name    text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    investigation_id uuid NOT NULL,
    family           text NOT NULL CHECK (family ~ '^[a-z][a-z0-9_]{0,63}$'),
    verdict          text NOT NULL CHECK (verdict IN ('accepted', 'rejected')),
    reviewer         text NOT NULL CHECK (length(reviewer) BETWEEN 1 AND 200),
    note             text CHECK (length(note) <= 2000),
    reviewed_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_name, investigation_id)
);
CREATE INDEX IF NOT EXISTS sre_packet_reviews_family
    ON sage.sre_packet_reviews (deployment_id, family, reviewed_at);

CREATE TABLE IF NOT EXISTS sage.sre_eval_runs (
    deployment_id  uuid NOT NULL,
    id             uuid NOT NULL,
    source         text NOT NULL CHECK (source IN ('bench', 'game_day')),
    schema_version text NOT NULL,
    generated_at   timestamptz NOT NULL,
    ingested_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    ingested_by    text NOT NULL CHECK (length(ingested_by) BETWEEN 1 AND 200),
    database_name  text,
    report_sha256  bytea NOT NULL CHECK (octet_length(report_sha256) = 32),
    gated_arms     jsonb NOT NULL DEFAULT '[]'::jsonb,
    cells          jsonb NOT NULL,
    PRIMARY KEY (deployment_id, id),
    UNIQUE (deployment_id, report_sha256),
    CHECK ((source = 'game_day') = (database_name IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS sre_eval_runs_latest
    ON sage.sre_eval_runs (deployment_id, source, generated_at DESC);

CREATE TABLE IF NOT EXISTS sage.sre_game_days (
    deployment_id uuid NOT NULL,
    id            uuid NOT NULL,
    database_name text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    provider      text NOT NULL,
    clone_id      text,
    families      text[] NOT NULL,
    status        text NOT NULL CHECK (status IN
        ('running', 'completed', 'failed', 'destroy_failed')),
    started_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at   timestamptz,
    error         text CHECK (length(error) <= 2000),
    eval_run_id   uuid,
    PRIMARY KEY (deployment_id, id),
    CHECK ((status = 'running') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS sre_game_days_latest
    ON sage.sre_game_days (deployment_id, database_name, started_at DESC);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sre_autonomy_events_append_only'
                   AND tgrelid = 'sage.sre_autonomy_events'::regclass) THEN
        CREATE TRIGGER sre_autonomy_events_append_only
            BEFORE UPDATE ON sage.sre_autonomy_events
            FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sre_autonomy_outcomes_append_only'
                   AND tgrelid = 'sage.sre_autonomy_outcomes'::regclass) THEN
        CREATE TRIGGER sre_autonomy_outcomes_append_only
            BEFORE UPDATE ON sage.sre_autonomy_outcomes
            FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();
    END IF;
END $$;

ALTER TABLE sage.rollout_run
    ADD COLUMN IF NOT EXISTS halt_reason text,
    ADD COLUMN IF NOT EXISTS family text,
    ADD COLUMN IF NOT EXISTS action_class text,
    ADD COLUMN IF NOT EXISTS started_by text;

CREATE TABLE IF NOT EXISTS sage.rollout_instance (
    run_evidence_id text NOT NULL REFERENCES sage.rollout_run (evidence_id),
    instance_id     text NOT NULL,
    ordinal         integer NOT NULL CHECK (ordinal > 0),
    status          text NOT NULL CHECK (status IN ('not_locally_verified', 'applied',
        'rolled_back', 'rollback_failed', 'rollback_unavailable', 'failed')),
    evidence_id     text,
    action_log_id   bigint,
    regression_pct  double precision,
    detail          text CHECK (length(detail) <= 2000),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (run_evidence_id, instance_id)
);
`
