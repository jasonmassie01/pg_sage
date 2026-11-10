package schema

// ddlApprovalCardNextCheck gives each approval card its next follow-up
// check (policy-06). The follow-up worker reads the cards due now, by
// (next_check_at, id), and pushes a card whose action has no verdict yet
// back by an eighth of its age, so cards still waiting never hold the head
// of the queue. The partial index replaces the (created_at, id) one. The
// catalog checks first mean a re-run takes no lock on the table;
// lock_timeout bounds the wait of the first run.
const ddlApprovalCardNextCheck = `
SET LOCAL lock_timeout = '5s';
DO $approval_card_next_check$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.approval_card_deliveries'::regclass
           AND attname = 'next_check_at' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.approval_card_deliveries
            ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT now();
    END IF;
    IF to_regclass('sage.idx_approval_card_next_check') IS NULL THEN
        CREATE INDEX idx_approval_card_next_check
            ON sage.approval_card_deliveries (next_check_at, id)
            WHERE followed_up_at IS NULL;
    END IF;
    -- Inside the block, checked first, so a boot without the old index
    -- takes no lock (config-04's precheck runs a DO block every boot).
    IF to_regclass('sage.idx_approval_card_followup') IS NOT NULL THEN
        DROP INDEX sage.idx_approval_card_followup;
    END IF;
END
$approval_card_next_check$;
`
