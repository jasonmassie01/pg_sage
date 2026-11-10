package schema

// ddlFactsMissingSince adds sage.facts.missing_since (facts-08): when
// re-verification first found a fact's subject absent, kept across
// restarts. Re-verification clears it when the subject is back; an expiry
// by absence keeps it (so the confirmation can come back with the
// subject), every other expiry clears it. A nullable column without a
// default: no table rewrite on PG 14-18. The catalog check first means a
// re-run takes no lock on sage.facts; lock_timeout bounds the first run.
const ddlFactsMissingSince = `
SET LOCAL lock_timeout = '5s';
DO $facts_missing_since$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.facts'::regclass
           AND attname = 'missing_since' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.facts ADD COLUMN missing_since timestamptz;
    END IF;
END
$facts_missing_since$;
`
