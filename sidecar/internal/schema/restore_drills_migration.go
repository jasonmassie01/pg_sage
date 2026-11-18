package schema

// ddlRestoreDrills is the restore drill record (drill-01a): one row per
// drill pg_sage ran, with the clone it restored, the measured RTO and RPO
// and each invariant's result. D7 reads the newest passed drill of a
// database through restore_drills_passed. It lives where clone receipts
// live: the governance control database when one is set, else the
// database's own sage schema.
const ddlRestoreDrills = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.restore_drills (
    id             bigserial PRIMARY KEY,
    database_id    integer,
    database_name  text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    provider       text NOT NULL CHECK (length(provider) BETWEEN 1 AND 200),
    kind           text NOT NULL,
    receipt_id     bigint,
    requested_at   timestamptz NOT NULL,
    ready_at       timestamptz,
    finished_at    timestamptz NOT NULL,
    recovery_point timestamptz,
    rto_seconds    double precision CHECK (rto_seconds >= 0),
    rpo_seconds    double precision CHECK (rpo_seconds >= 0),
    invariants     jsonb NOT NULL DEFAULT '[]'::jsonb,
    passed         boolean NOT NULL,
    failure        text,
    cost_label     text,
    CHECK (finished_at >= requested_at)
);
CREATE INDEX IF NOT EXISTS restore_drills_passed
    ON sage.restore_drills (database_name, finished_at DESC) WHERE passed;
CREATE INDEX IF NOT EXISTS restore_drills_database
    ON sage.restore_drills (database_name, requested_at DESC);
`
