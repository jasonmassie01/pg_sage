package schema

// ddlExposureRemediations (fleetpol-02b) records what each exposure revoke
// took away: the table's ACL before the revoke and the privileges revoked
// from PUBLIC, one row per action_log row. The stored re-GRANT
// (sage.action_log.rollback_sql) grants back only those privileges, so a
// rollback still restores PUBLIC's access after the ACL changed again.
const ddlExposureRemediations = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.exposure_remediations (
    action_log_id bigint PRIMARY KEY,
    object        text NOT NULL CHECK (length(object) BETWEEN 1 AND 512),
    prior_acl jsonb NOT NULL DEFAULT '[]'::jsonb,
    revoked jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_exposure_remediations_object
    ON sage.exposure_remediations (object, created_at DESC);
`
