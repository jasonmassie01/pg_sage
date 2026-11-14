package schema

// ddlDatabasesActionCredential (security-09b) gives a meta-DB member an
// optional action identity: a second role on the member's own database,
// stored as the role name and its sealed password (sage-enc:v1 under the
// AAD sage.databases|<id>|action_password). Host, port and database are
// never stored for it, so it cannot point anywhere else. Both columns are
// set or neither is, and the action role is never the observation role.
// The checks are added NOT VALID and validated in the same step (the new
// columns are NULL on every existing row); the catalog guards keep a
// second boot from taking the table lock.
const ddlDatabasesActionCredential = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS action_username TEXT,
    ADD COLUMN IF NOT EXISTS action_password_enc BYTEA;
DO $do$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'sage.databases'::regclass
                   AND conname = 'databases_action_credential_pair') THEN
        ALTER TABLE sage.databases ADD CONSTRAINT databases_action_credential_pair
            CHECK ((action_username IS NULL) = (action_password_enc IS NULL)) NOT VALID;
        ALTER TABLE sage.databases VALIDATE CONSTRAINT databases_action_credential_pair;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'sage.databases'::regclass
                   AND conname = 'databases_action_role_distinct') THEN
        ALTER TABLE sage.databases ADD CONSTRAINT databases_action_role_distinct
            CHECK (action_username <> username) NOT VALID;
        ALTER TABLE sage.databases VALIDATE CONSTRAINT databases_action_role_distinct;
    END IF;
END $do$;
`
