package schema

// Roadmap 2.4 owner addition A (2026-10-04): the trust ledger's history
// records a family earning or losing model-root authority
// (root_authority_granted, root_authority_revoked, class model_root)
// with the report that decided it. Redefines sre_autonomy_events_type_v2
// as the superset of every earlier definition plus the two types; it
// runs after the trust ledger migration (whose guard looks for
// 'grandfathered', which this keeps), so the bootstrap settles on it.
// Idempotent: guarded on the new type.
const ddlSREAutonomyRootAuthority = `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_events_type_v2'
                     AND conrelid = 'sage.sre_autonomy_events'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%root_authority_revoked%') THEN
        ALTER TABLE sage.sre_autonomy_events
            DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2;
        ALTER TABLE sage.sre_autonomy_events
            ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
                'promotion_proposed', 'promotion_approved', 'promotion_rejected',
                'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
                'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override',
                'database_scoped', 'grandfathered', 'root_authority_granted',
                'root_authority_revoked'));
    END IF;
END $$;
`
