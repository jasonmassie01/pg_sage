package schema

// ddlUserDatabaseGrantsSource records who owns each per-database grant
// (apiauth-08d): 'manual' for an admin's grant, 'idp' for one the SSO
// group mapping (oauth.database_grant_mapping) wrote at login. Grants from
// before the column read 'manual'. A login replaces only the user's 'idp'
// rows; manual rows are never touched by it.
const ddlUserDatabaseGrantsSource = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.user_database_grants
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual'
    CHECK (source IN ('manual', 'idp'));
`
