package schema

// ddlObserveOnlyMembers (fleetpol-01d) adds the member tier: a full member
// gets a runtime (collector, analyzer, executor, pools) and counts against
// the 50-member cap; an observe_only member gets none of that and is
// visited by the fleet sweeper with one short-lived read-only connection
// per round (at most 2,000 of them). Every existing row reads as full, so
// nothing a v2.x fleet runs changes. observe_sweeps keeps each member's
// newest sweep; observe_rounds keeps the rounds (time, counts, peak
// connections) for the round-time number.
const ddlObserveOnlyMembers = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.databases
    ADD COLUMN IF NOT EXISTS tier text NOT NULL DEFAULT 'full'
        CHECK (tier IN ('full', 'observe_only'));
CREATE INDEX IF NOT EXISTS databases_tier_idx ON sage.databases (tier, enabled);
CREATE TABLE IF NOT EXISTS sage.observe_rounds (
    id             bigserial PRIMARY KEY,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    members        int NOT NULL DEFAULT 0 CHECK (members >= 0),
    ok             int NOT NULL DEFAULT 0 CHECK (ok >= 0),
    unreachable    int NOT NULL DEFAULT 0 CHECK (unreachable >= 0),
    failed         int NOT NULL DEFAULT 0 CHECK (failed >= 0),
    promoted       int NOT NULL DEFAULT 0 CHECK (promoted >= 0),
    max_concurrent int NOT NULL DEFAULT 0 CHECK (max_concurrent >= 0)
);
CREATE INDEX IF NOT EXISTS observe_rounds_started_idx ON sage.observe_rounds (started_at);
CREATE TABLE IF NOT EXISTS sage.observe_sweeps (
    database_id int PRIMARY KEY REFERENCES sage.databases (id) ON DELETE CASCADE,
    round_id    bigint NOT NULL,
    swept_at    timestamptz NOT NULL,
    status      text NOT NULL CHECK (status IN ('ok', 'unreachable', 'failed')),
    error       text NOT NULL DEFAULT '',
    letter      text NOT NULL DEFAULT '',
    grade       jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(grade) = 'object'),
    duration_ms int NOT NULL DEFAULT 0 CHECK (duration_ms >= 0)
);
`
