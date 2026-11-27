package schema

// ddlUsersPasswordLifecycle adds sage.users.must_change_password and
// password_changed_at (apiauth-03). An admin reset and a generated
// bootstrap password set the flag; the session middleware then allows only
// the password change, /auth/me and logout until the user picks a new
// password. Existing accounts keep false, so an upgrade forces nobody.
// The catalog check first means a re-run takes no lock on sage.users;
// lock_timeout bounds the wait of the first run.
const ddlUsersPasswordLifecycle = `
SET LOCAL lock_timeout = '5s';
DO $users_password_lifecycle$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.users'::regclass
           AND attname = 'must_change_password' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.users
            ADD COLUMN IF NOT EXISTS must_change_password boolean NOT NULL DEFAULT false;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.users'::regclass
           AND attname = 'password_changed_at' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.users ADD COLUMN IF NOT EXISTS password_changed_at timestamptz;
    END IF;
END
$users_password_lifecycle$;
`
