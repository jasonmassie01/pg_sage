package schema

// ddlQueryHintsStatusCheck constrains sage.query_hints.status to the hint
// lifecycle (tuning-06 adds 'rejected' and 'expired'; docs/hint-verification.md
// "Hint lifecycle"). It is added NOT VALID: rows older releases wrote are not
// scanned or refused, while every new or updated row is checked. A later
// release validates it. The statements run in one implicit transaction, so
// SET LOCAL bounds the ALTER's lock wait on a busy sage.query_hints.
const ddlQueryHintsStatusCheck = `
SET LOCAL lock_timeout = '5s';
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
         WHERE conrelid = 'sage.query_hints'::regclass
           AND conname = 'query_hints_status_check') THEN
        ALTER TABLE sage.query_hints ADD CONSTRAINT query_hints_status_check
            CHECK (status IN ('proposed', 'active', 'dormant', 'retired', 'broken',
                              'rolled_back', 'rejected', 'expired')) NOT VALID;
    END IF;
END $$;
`
