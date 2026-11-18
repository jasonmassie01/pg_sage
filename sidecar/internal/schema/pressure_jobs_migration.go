package schema

// ddlPressureJobs is the backfill job registry (guard-01b): a job names
// the application_name its sessions run under, the table it writes and
// whether pg_sage may propose cancelling its backend after two consecutive
// stop verdicts (always with a person's approval). A job is active until
// expires_at; the store keeps one active job per database and name. It
// lives in the monitored database's own sage schema, beside the approval
// queue its proposals land in.
const ddlPressureJobs = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.pressure_jobs (
    id               bigserial PRIMARY KEY,
    database         text NOT NULL CHECK (length(database) BETWEEN 1 AND 200),
    application_name text NOT NULL CHECK (length(application_name) BETWEEN 1 AND 63),
    table_name       text CHECK (table_name IS NULL OR length(table_name) BETWEEN 1 AND 260),
    registered_by    text NOT NULL CHECK (length(registered_by) BETWEEN 1 AND 200),
    enforce          boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    CONSTRAINT pressure_jobs_expiry CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS pressure_jobs_active
    ON sage.pressure_jobs (database, expires_at);
`
