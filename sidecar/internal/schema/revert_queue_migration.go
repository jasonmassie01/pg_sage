package schema

// ddlActionQueueRevertsAction names the action a "Revert action N" queue
// item reverts (policy-05): an automatic rollback the standing policy or
// a hard stop withheld is offered to a person instead of dead-ending as
// rollback_skipped. The column is additive and nullable; the catalog check
// means a re-run takes no lock, and lock_timeout bounds the first run.
const ddlActionQueueRevertsAction = `
SET LOCAL lock_timeout = '5s';
DO $action_queue_reverts_action$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.action_queue'::regclass
           AND attname = 'reverts_action_id' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.action_queue
            ADD COLUMN reverts_action_id bigint
                REFERENCES sage.action_log(id) ON DELETE SET NULL;
    END IF;
    IF to_regclass('sage.idx_action_queue_reverts_pending') IS NULL THEN
        CREATE INDEX idx_action_queue_reverts_pending
            ON sage.action_queue (reverts_action_id)
            WHERE reverts_action_id IS NOT NULL AND status = 'pending';
    END IF;
END
$action_queue_reverts_action$;
`
