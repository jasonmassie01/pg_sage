package schema

// Sage SRE M7 carry-over (coordinator decision 2026-10-02): M7 gates new
// autonomy and keeps the autonomy pg_sage already had under decided
// policy. A ledger row says whether its level was carried over from that
// policy (with the decision that granted it) or set by the ledger itself
// (default, promotion, downgrade). The history gains the carry-over and
// mandatory deadline-override events. Additive and idempotent on top of
// ddlSREAutonomy.
const ddlSREAutonomyCarryOver = `
ALTER TABLE sage.sre_family_autonomy
    ADD COLUMN IF NOT EXISTS provenance text NOT NULL DEFAULT 'ledger',
    ADD COLUMN IF NOT EXISTS carried_ref text;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_family_autonomy_provenance'
                     AND conrelid = 'sage.sre_family_autonomy'::regclass) THEN
        ALTER TABLE sage.sre_family_autonomy
            ADD CONSTRAINT sre_family_autonomy_provenance CHECK (
                (provenance = 'ledger' AND carried_ref IS NULL) OR
                (provenance = 'carried_over' AND carried_ref IS NOT NULL
                 AND length(carried_ref) BETWEEN 1 AND 500));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_events_type_v2'
                     AND conrelid = 'sage.sre_autonomy_events'::regclass) THEN
        ALTER TABLE sage.sre_autonomy_events
            DROP CONSTRAINT IF EXISTS sre_autonomy_events_event_type_check;
        ALTER TABLE sage.sre_autonomy_events
            ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
                'promotion_proposed', 'promotion_approved', 'promotion_rejected',
                'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
                'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override'));
    END IF;
END $$;
`
