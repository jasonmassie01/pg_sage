package schema

// ddlUsersDisabledAt adds sage.users.disabled_at (apiauth-01). A user with
// it set cannot sign in or use a session; deactivation is the offboarding
// step that keeps the account's history attributable. The catalog check
// first means a re-run takes no lock on sage.users; lock_timeout bounds the
// wait of the first run.
const ddlUsersDisabledAt = `
SET LOCAL lock_timeout = '5s';
DO $users_disabled_at$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.users'::regclass
           AND attname = 'disabled_at' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.users ADD COLUMN disabled_at timestamptz;
    END IF;
END
$users_disabled_at$;
`
