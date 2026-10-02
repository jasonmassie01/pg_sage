package schema

// Sage SRE M3 model turn: the investigation event chain records the
// model's accepted review, its rejections (with the reason it fell back
// to the deterministic result) and disagreements with a conclusive
// graph. The M2 inline event-type check is replaced by a named one that
// also allows these types. Idempotent; the new check is added NOT VALID
// and then validated, so existing rows are checked without holding an
// ACCESS EXCLUSIVE lock for the scan.
const ddlSREModelEvents = `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_events_event_type_m3'
                     AND conrelid = 'sage.sre_events'::regclass) THEN
        ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_check;
        ALTER TABLE sage.sre_events ADD CONSTRAINT sre_events_event_type_m3
            CHECK (event_type IN ('created', 'claimed', 'step', 'transition',
                'concluded', 'pinned', 'unpinned', 'evidence_purged',
                'model_reviewed', 'model_rejected', 'model_disagreed')) NOT VALID;
        ALTER TABLE sage.sre_events VALIDATE CONSTRAINT sre_events_event_type_m3;
    END IF;
END $$;
`

// ddlSREReasoningBudget adds a thinking model's reasoning allowance to
// the model budget ledger, separate from the answer tokens: what a turn
// reserved and what the provider reported. Rows written before it hold
// no reasoning. Idempotent.
const ddlSREReasoningBudget = `
ALTER TABLE sage.sre_budget_reservations
    ADD COLUMN IF NOT EXISTS reasoning_reserved bigint NOT NULL DEFAULT 0
        CHECK (reasoning_reserved >= 0),
    ADD COLUMN IF NOT EXISTS reasoning_used bigint CHECK (reasoning_used >= 0);
`
