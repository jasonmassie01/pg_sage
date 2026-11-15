package schema

// ddlMultixactMembers (guard-02a): sage.multixact_member_samples holds
// the runway monitor's readings of the multixact member space, one row
// per database and reading: next_mxid and next_offset as of the last
// checkpoint (checkpoint_at), the oldest multixact, the live multixacts
// and how many were sampled, and the estimated members in use with its
// standard error. The member consumption rate is read from successive
// rows. The monitor prunes rows older than 90 days on every pass.
const ddlMultixactMembers = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.multixact_member_samples (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database        text NOT NULL CHECK (length(database) BETWEEN 1 AND 200),
    sampled_at      timestamptz NOT NULL DEFAULT now(),
    checkpoint_at   timestamptz NOT NULL,
    next_mxid       bigint NOT NULL CHECK (next_mxid BETWEEN 0 AND 4294967295),
    next_offset     bigint NOT NULL CHECK (next_offset BETWEEN 0 AND 4294967295),
    oldest_mxid     bigint NOT NULL CHECK (oldest_mxid BETWEEN 0 AND 4294967295),
    live_multixacts bigint NOT NULL CHECK (live_multixacts >= 0),
    sampled         integer NOT NULL CHECK (sampled >= 0),
    members_est     double precision NOT NULL CHECK (members_est >= 0),
    stderr          double precision NOT NULL CHECK (stderr >= 0)
);
CREATE INDEX IF NOT EXISTS multixact_member_samples_database_idx
    ON sage.multixact_member_samples (database, sampled_at);
CREATE INDEX IF NOT EXISTS multixact_member_samples_sampled_at_idx
    ON sage.multixact_member_samples (sampled_at);
`
