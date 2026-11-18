package schema

// ddlReviewFleet is Sage Review's fleet tiering ledger (review-04b), on
// the control database. sage.review_fleet_pass is one tiered pass of a
// fleet scope in a calendar month (UTC): its triage mode, its fleet-level
// review and its per-tier token counts. sage.review_fleet_member is one
// member's row in a pass: its deterministic rollup, then the forward-only
// states rollup -> triaged -> deep_scheduled -> reviewed. A triaged
// member waiting for room carries deferred_to, the month it may be
// scheduled in. The step also adds fleet_outlier to sage.review_trigger's
// kinds (the member's deep review is fired through its trigger queue); the
// kind check is swapped only while it lacks the kind.
const ddlReviewFleet = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_fleet_pass (
    id                 bigserial PRIMARY KEY,
    fleet_scope        text NOT NULL CHECK (length(fleet_scope) BETWEEN 1 AND 200),
    period             text NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    status             text NOT NULL DEFAULT 'running'
                           CHECK (status IN ('running', 'done', 'halted', 'failed')),
    members            integer NOT NULL CHECK (members >= 0),
    outliers           integer NOT NULL DEFAULT 0 CHECK (outliers >= 0),
    scheduled          integer NOT NULL DEFAULT 0 CHECK (scheduled >= 0),
    deferred           integer NOT NULL DEFAULT 0 CHECK (deferred >= 0),
    triage_mode        text CHECK (triage_mode IN ('model', 'deterministic', 'mixed')),
    triage_error_class text NOT NULL DEFAULT '',
    triage             jsonb NOT NULL DEFAULT '{}'::jsonb
                           CHECK (jsonb_typeof(triage) = 'object'),
    fleet_review       jsonb NOT NULL DEFAULT '{}'::jsonb
                           CHECK (jsonb_typeof(fleet_review) = 'object'),
    tier_tokens        jsonb NOT NULL DEFAULT '{}'::jsonb
                           CHECK (jsonb_typeof(tier_tokens) = 'object'),
    error              text NOT NULL DEFAULT '',
    started_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    CHECK ((status = 'running') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS review_fleet_pass_period
    ON sage.review_fleet_pass (fleet_scope, period, id);
CREATE TABLE IF NOT EXISTS sage.review_fleet_member (
    pass_id          bigint NOT NULL REFERENCES sage.review_fleet_pass (id) ON DELETE CASCADE,
    database_name    text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    state            text NOT NULL DEFAULT 'rollup'
                         CHECK (state IN ('rollup', 'triaged', 'deep_scheduled', 'reviewed')),
    rollup           jsonb NOT NULL CHECK (jsonb_typeof(rollup) = 'object'),
    score            double precision,
    reason           text NOT NULL DEFAULT '',
    cites            text[] NOT NULL DEFAULT '{}',
    source           text NOT NULL DEFAULT '',
    deferred_to      text CHECK (deferred_to ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    scheduled_period text CHECK (scheduled_period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    deep_tokens      integer CHECK (deep_tokens >= 0),
    trigger_id       bigint,
    run_id           bigint,
    scheduled_at     timestamptz,
    reviewed_at      timestamptz,
    PRIMARY KEY (pass_id, database_name),
    CHECK ((state IN ('deep_scheduled', 'reviewed')) = (scheduled_at IS NOT NULL)),
    CHECK ((state = 'reviewed') = (reviewed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS review_fleet_member_database
    ON sage.review_fleet_member (database_name, pass_id DESC);
CREATE INDEX IF NOT EXISTS review_fleet_member_waiting
    ON sage.review_fleet_member (pass_id)
    WHERE state IN ('triaged', 'deep_scheduled');
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.review_trigger'::regclass
           AND conname = 'review_trigger_kind_check'
           AND pg_get_constraintdef(oid) LIKE '%fleet_outlier%') THEN
        ALTER TABLE sage.review_trigger DROP CONSTRAINT IF EXISTS review_trigger_kind_check;
        ALTER TABLE sage.review_trigger ADD CONSTRAINT review_trigger_kind_check
            CHECK (kind IN ('enrollment', 'pre_upgrade', 'unresolved_regression',
                'cost_growth_anomaly', 'large_migration', 'instance_change', 'calendar',
                'fleet_outlier'));
    END IF;
END $$;
`
