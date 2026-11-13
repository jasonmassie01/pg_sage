package schema

// ddlUserDatabaseGrants stores per-database grants (apiauth-08): one
// viewer or operator grant per user and database. database_name is the
// fleet's database name; it has no foreign key because databases named in
// the YAML config never appear in sage.databases, so the API checks the
// name against the fleet when a grant is written. A grant goes with its
// user.
const ddlUserDatabaseGrants = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.user_database_grants (
    user_id        INT NOT NULL REFERENCES sage.users(id) ON DELETE CASCADE,
    database_name  TEXT NOT NULL CHECK (database_name <> ''),
    role           TEXT NOT NULL CHECK (role IN ('viewer', 'operator')),
    granted_by     INT NOT NULL DEFAULT 0,
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, database_name)
);
`
