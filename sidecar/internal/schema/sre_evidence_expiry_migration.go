package schema

// earned-06 (2026-11-11): the trust ledger's history records the one
// warning that an earned level holds only until its evidence expires
// (evidence_expiring). Redefines sre_autonomy_events_type_v2 as the
// superset of every earlier definition plus the new type; it runs after
// every earlier redefinition (ddlSREAutonomyRootAuthority last), whose
// guards look for their own types, which this keeps. Idempotent: guarded
// on the new type, so a re-run takes no lock; lock_timeout bounds the
// first run's wait for the table.
const ddlSREAutonomyEvidenceExpiring = `
SET LOCAL lock_timeout = '5s';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_events_type_v2'
                     AND conrelid = 'sage.sre_autonomy_events'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%evidence_expiring%') THEN
        ALTER TABLE sage.sre_autonomy_events
            DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2;
        ALTER TABLE sage.sre_autonomy_events
            ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
                'promotion_proposed', 'promotion_approved', 'promotion_rejected',
                'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
                'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override',
                'database_scoped', 'grandfathered', 'root_authority_granted',
                'root_authority_revoked', 'evidence_expiring'));
    END IF;
END $$;
`
