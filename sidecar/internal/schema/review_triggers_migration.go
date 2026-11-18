package schema

// ddlReviewTriggers is Sage Review's durable trigger queue (review-04a):
// one row of sage.review_trigger per event that makes a review worth
// running now (or the calendar), kept across restarts and leader changes.
// A trigger moves fired -> claimed (a leader holds it until lease_until)
// -> consumed (the run it started) or coalesced (joined the run of
// coalesced_into); an expired claim goes back to fired. dedupe_key makes
// firing idempotent per database and kind. sage.review_trigger_state keeps
// what a polling source last saw (the instance facts), so a restart does
// not read an old size as a change. Both live in the reviewed database's
// own sage schema, beside sage.review_run.
const ddlReviewTriggers = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_trigger (
    id             bigserial PRIMARY KEY,
    database_name  text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    kind           text NOT NULL CHECK (kind IN ('enrollment', 'pre_upgrade',
                       'unresolved_regression', 'cost_growth_anomaly', 'large_migration',
                       'instance_change', 'calendar')),
    dedupe_key     text NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 300),
    detail         jsonb NOT NULL DEFAULT '{}'::jsonb
                       CHECK (jsonb_typeof(detail) = 'object'),
    fired_at       timestamptz NOT NULL DEFAULT now(),
    state          text NOT NULL DEFAULT 'fired'
                       CHECK (state IN ('fired', 'claimed', 'consumed', 'coalesced')),
    claimed_by     text CHECK (claimed_by IS NULL OR length(claimed_by) BETWEEN 1 AND 200),
    lease_until    timestamptz,
    run_id         bigint REFERENCES sage.review_run (id) ON DELETE SET NULL,
    coalesced_into bigint REFERENCES sage.review_trigger (id) ON DELETE SET NULL,
    done_at        timestamptz,
    CHECK ((state = 'claimed') = (lease_until IS NOT NULL AND claimed_by IS NOT NULL)),
    CHECK ((state IN ('consumed', 'coalesced')) = (done_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS review_trigger_dedupe
    ON sage.review_trigger (database_name, kind, dedupe_key);
CREATE INDEX IF NOT EXISTS review_trigger_pending
    ON sage.review_trigger (database_name, fired_at, id) WHERE state IN ('fired', 'claimed');
CREATE INDEX IF NOT EXISTS review_trigger_run
    ON sage.review_trigger (run_id) WHERE run_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS sage.review_trigger_state (
    database_name text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    source        text NOT NULL CHECK (source IN ('instance_change')),
    facts         jsonb NOT NULL CHECK (jsonb_typeof(facts) = 'object'),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, source)
);
`
