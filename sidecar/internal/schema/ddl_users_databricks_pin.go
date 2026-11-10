package schema

// ddlUsersDatabricksPin adds sage.users.databricks_user_pin (databricks-07).
// In Databricks Apps mode the account is keyed on the forwarded email, which
// the IdP may reassign; the column holds the hex SHA-256 of the workspace and
// the forwarded IdP user value first seen for the account, never the raw
// value. NULL means unpinned. The catalog check first means a re-run takes
// no lock on sage.users; lock_timeout bounds the wait of the first run.
const ddlUsersDatabricksPin = `
SET LOCAL lock_timeout = '5s';
DO $users_databricks_pin$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.users'::regclass
           AND attname = 'databricks_user_pin' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.users ADD COLUMN databricks_user_pin text;
    END IF;
END
$users_databricks_pin$;
`
