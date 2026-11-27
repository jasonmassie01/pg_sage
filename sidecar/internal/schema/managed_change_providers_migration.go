package schema

// ddlManagedChangeProviders (advisors-03) widens the provider check of
// sage.managed_change_proposals to AlloyDB (database flags) and Azure
// flexible server (server parameters), whose findings now become
// operator-approved proposals. Existing rows already satisfy the wider
// check. The drop and re-add take ACCESS EXCLUSIVE in the same
// transaction (a small table), bounded by lock_timeout. Idempotent: a
// check that already names azure is left alone.
const ddlManagedChangeProviders = `
SET LOCAL lock_timeout = '5s';
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
         WHERE conrelid = 'sage.managed_change_proposals'::regclass
           AND conname = 'managed_change_proposals_provider_check'
           AND pg_catalog.pg_get_constraintdef(oid) LIKE '%''azure''%') THEN
        ALTER TABLE sage.managed_change_proposals
            DROP CONSTRAINT IF EXISTS managed_change_proposals_provider_check;
        ALTER TABLE sage.managed_change_proposals
            ADD CONSTRAINT managed_change_proposals_provider_check
            CHECK (provider IN ('rds', 'aurora', 'cloud-sql', 'alloydb', 'azure')) NOT VALID;
        ALTER TABLE sage.managed_change_proposals
            VALIDATE CONSTRAINT managed_change_proposals_provider_check;
    END IF;
END $$;
`
