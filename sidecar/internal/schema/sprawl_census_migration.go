package schema

// ddlSprawlCensus (fleetpol-03) adds the monthly sprawl census of a
// meta-db fleet. sprawl_census_runs holds one run per calendar month (the
// unique period makes two leaders' runs one run) with the fleet's reuse
// share: of the agent-created databases old enough to judge, how many had
// real activity seven or more days after creation. sprawl_census holds the
// run's rows: owner, creator, creation time, last real activity, size,
// monthly cost at operator rates (NULL is unpriced) and the TTL proposal,
// which is advice for a person (no delete action exists). sprawl_activity
// keeps each observe-only member's newest pg_stat_statements reading, so
// the census can tell real activity from none between sweeps. The census
// writer prunes runs older than 400 days; their rows go with them.
const ddlSprawlCensus = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.sprawl_census_runs (
    id             bigserial PRIMARY KEY,
    period         date NOT NULL UNIQUE,
    holder         text NOT NULL DEFAULT '',
    run_at         timestamptz NOT NULL,
    finished_at    timestamptz,
    databases      int NOT NULL DEFAULT 0 CHECK (databases >= 0),
    agent_created  int NOT NULL DEFAULT 0 CHECK (agent_created >= 0),
    reuse_eligible int NOT NULL DEFAULT 0 CHECK (reuse_eligible >= 0),
    reused         int NOT NULL DEFAULT 0 CHECK (reused >= 0),
    reuse_share    double precision CHECK (reuse_share BETWEEN 0 AND 1),
    proposals      int NOT NULL DEFAULT 0 CHECK (proposals >= 0),
    unreachable    int NOT NULL DEFAULT 0 CHECK (unreachable >= 0)
);
CREATE INDEX IF NOT EXISTS sprawl_census_runs_run_at_idx ON sage.sprawl_census_runs (run_at);
CREATE TABLE IF NOT EXISTS sage.sprawl_census (
    run_id           bigint NOT NULL REFERENCES sage.sprawl_census_runs (id) ON DELETE CASCADE,
    run_at           timestamptz NOT NULL,
    database_key     text NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('full', 'observe_only', 'discovered')),
    status           text NOT NULL
        CHECK (status IN ('measured', 'unmeasured', 'unreachable', 'not_enrolled')),
    creator          text NOT NULL DEFAULT '',
    owner            text NOT NULL DEFAULT '',
    created_at       timestamptz,
    last_activity_at timestamptz,
    activity_basis   text NOT NULL DEFAULT '',
    reused           boolean,
    size_bytes       bigint CHECK (size_bytes >= 0),
    size_note        text NOT NULL DEFAULT '',
    cost_month       double precision CHECK (cost_month >= 0),
    cost_note        text NOT NULL DEFAULT '',
    proposal         text,
    note             text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, database_key)
);
CREATE INDEX IF NOT EXISTS sprawl_census_proposal_idx ON sage.sprawl_census (database_key)
    WHERE proposal IS NOT NULL;
CREATE TABLE IF NOT EXISTS sage.sprawl_activity (
    database_key     text PRIMARY KEY,
    workload_calls   bigint CHECK (workload_calls >= 0),
    size_bytes       bigint CHECK (size_bytes >= 0),
    baseline_at      timestamptz NOT NULL,
    read_at          timestamptz NOT NULL,
    last_activity_at timestamptz
);
`
