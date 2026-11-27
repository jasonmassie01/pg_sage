package schema

// ddlRolloutRunInterrupted persists a fleet canary's run state as it runs
// (executor-04b): the run records its ordered targets, a run a stopped
// sidecar left in canary becomes interrupted at the next startup, and an
// instance takes applying (re-verified, being changed), awaiting_verdict
// (changed, its database's verdict still open) and interrupted (the run
// stopped before that verdict was recorded). The catalog is read first,
// so once applied a re-run takes no lock; lock_timeout bounds the first.
const ddlRolloutRunInterrupted = `
SET LOCAL lock_timeout = '5s';
DO $rollout_run_interrupted$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.rollout_run'::regclass AND attname = 'targets'
           AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.rollout_run
            ADD COLUMN targets jsonb NOT NULL DEFAULT '[]'::jsonb;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.rollout_run'::regclass
           AND conname = 'rollout_run_state_check'
           AND pg_get_constraintdef(oid) LIKE '%interrupted%'
    ) THEN
        ALTER TABLE sage.rollout_run DROP CONSTRAINT IF EXISTS rollout_run_state_check;
        ALTER TABLE sage.rollout_run ADD CONSTRAINT rollout_run_state_check
            CHECK (state IN ('canary', 'expanding', 'halted', 'complete', 'failed',
                'interrupted'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.rollout_instance'::regclass
           AND conname = 'rollout_instance_status_check'
           AND pg_get_constraintdef(oid) LIKE '%awaiting_verdict%'
    ) THEN
        ALTER TABLE sage.rollout_instance
            DROP CONSTRAINT IF EXISTS rollout_instance_status_check;
        ALTER TABLE sage.rollout_instance ADD CONSTRAINT rollout_instance_status_check
            CHECK (status IN ('not_locally_verified', 'applied', 'rolled_back',
                'rollback_failed', 'rollback_unavailable', 'failed',
                'verification_pending', 'applied_unverified', 'applying',
                'awaiting_verdict', 'interrupted'));
    END IF;
END
$rollout_run_interrupted$;
`
