package schema

// ddlSelfConfigDerivations (footprint-04) keeps each soak pass's raw
// derivation on sage.config_derived_setting (a cost-like rule promotes
// their p90) and lets the derivation ledger record a "clamped" event (a
// promoted value moved inside narrowed bounds). The catalog checks first
// mean a re-run takes no lock; lock_timeout bounds the wait of the first
// run. The ledger is small, so validating the new check is cheap.
const ddlSelfConfigDerivations = `
SET LOCAL lock_timeout = '5s';
DO $self_config_derivations$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.config_derived_setting'::regclass
           AND attname = 'derivations' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.config_derived_setting
            ADD COLUMN derivations jsonb NOT NULL DEFAULT '[]'::jsonb;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'config_derivation_event_v2'
           AND conrelid = 'sage.config_derivation'::regclass
    ) THEN
        ALTER TABLE sage.config_derivation
            DROP CONSTRAINT IF EXISTS config_derivation_event_check;
        ALTER TABLE sage.config_derivation ADD CONSTRAINT config_derivation_event_v2
            CHECK (event IN ('shadow', 'promoted', 'applied', 'held', 'cleared',
                'operator_set', 'resumed', 'pinned', 'unpinned', 'clamped'));
    END IF;
END
$self_config_derivations$;
`
