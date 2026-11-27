package schema

// ddlRolloutInstanceStatus widens the fleet canary's instance statuses
// (executor-04): verification_pending (applied; the verdict was still
// open at the target's monitor deadline, so nothing was rolled back) and
// applied_unverified (applied, then a rollout error, and its rollback
// failed too). The catalog is read first, so once applied a re-run takes
// no lock on sage.rollout_instance; lock_timeout bounds the first run.
const ddlRolloutInstanceStatus = `
SET LOCAL lock_timeout = '5s';
DO $rollout_instance_status$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.rollout_instance'::regclass
           AND conname = 'rollout_instance_status_check'
           AND pg_get_constraintdef(oid) LIKE '%applied_unverified%'
           AND pg_get_constraintdef(oid) LIKE '%verification_pending%'
    ) THEN
        RETURN;
    END IF;
    ALTER TABLE sage.rollout_instance
        DROP CONSTRAINT IF EXISTS rollout_instance_status_check;
    ALTER TABLE sage.rollout_instance
        ADD CONSTRAINT rollout_instance_status_check CHECK (status IN (
            'not_locally_verified', 'applied', 'rolled_back', 'rollback_failed',
            'rollback_unavailable', 'failed', 'verification_pending',
            'applied_unverified'));
END
$rollout_instance_status$;
`
