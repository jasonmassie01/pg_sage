package schema

// ddlGitopsManagedBy (cli-02a) marks the rows the GitOps directory
// reconciler owns: managed_by on agent principals and policy sets
// ('api' for every row made before or through the API, 'config' for one a
// file under policy.directory made or adopted), and 'config' as a third
// owner of a per-database grant beside apiauth-08d's 'manual' and 'idp'.
// The reconciler creates, changes or retires only its own rows. The
// A widening a file asks for is a pending principal change no user
// requested (requested_by_user 0, requested_by 'config:<file>@<sha256>').
// The ADD COLUMNs carry a constant default (no rewrite); the CHECK swaps
// (a grant's source, set_retired as a policy-set event, the config
// requester) run once, guarded by the new constraint's name, and
// lock_timeout bounds the first run's wait.
const ddlGitopsManagedBy = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.guard_principals
    ADD COLUMN IF NOT EXISTS managed_by text NOT NULL DEFAULT 'api'
        CONSTRAINT guard_principals_managed_by CHECK (managed_by IN ('api', 'config'));
ALTER TABLE sage.policy_sets
    ADD COLUMN IF NOT EXISTS managed_by text NOT NULL DEFAULT 'api'
        CONSTRAINT policy_sets_managed_by CHECK (managed_by IN ('api', 'config'));
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                 WHERE conname = 'user_database_grants_source_v2'
                   AND conrelid = 'sage.user_database_grants'::regclass) THEN
    ALTER TABLE sage.user_database_grants
        DROP CONSTRAINT IF EXISTS user_database_grants_source_check;
    ALTER TABLE sage.user_database_grants ADD CONSTRAINT user_database_grants_source_v2
        CHECK (source IN ('manual', 'idp', 'config'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                 WHERE conname = 'policy_set_events_event_v2'
                   AND conrelid = 'sage.policy_set_events'::regclass) THEN
    ALTER TABLE sage.policy_set_events DROP CONSTRAINT IF EXISTS policy_set_events_event_check;
    ALTER TABLE sage.policy_set_events ADD CONSTRAINT policy_set_events_event_v2
        CHECK (event IN ('set_saved', 'exception_set', 'exception_expired', 'set_retired'));
  END IF;
  IF to_regclass('sage.guard_principal_changes') IS NOT NULL AND NOT EXISTS (
       SELECT 1 FROM pg_constraint WHERE conname = 'guard_principal_changes_requester_v2'
         AND conrelid = 'sage.guard_principal_changes'::regclass) THEN
    ALTER TABLE sage.guard_principal_changes
        DROP CONSTRAINT IF EXISTS guard_principal_changes_requested_by_user_check;
    ALTER TABLE sage.guard_principal_changes ADD CONSTRAINT guard_principal_changes_requester_v2
        CHECK (requested_by_user > 0
               OR (requested_by_user = 0 AND requested_by LIKE 'config:%'));
  END IF;
END $$;
`
