package schema

// ddlRolloutInstanceEffect (lab-03b) records how a fleet canary measured
// each database: effect_pct is the number the halt bar was compared with
// (the member's own v2 effect, or its change net of the fleet control),
// control_pct the untreated members' median change of the same queries,
// and effect_method which of the two, or flag_only when there was no v2
// number and the verdict flag alone decided. Rows from before stay NULL.
// ADD COLUMN IF NOT EXISTS takes no rewrite (no default); lock_timeout
// bounds the first run's wait.
const ddlRolloutInstanceEffect = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.rollout_instance
    ADD COLUMN IF NOT EXISTS effect_pct double precision,
    ADD COLUMN IF NOT EXISTS control_pct double precision,
    ADD COLUMN IF NOT EXISTS effect_method text
        CONSTRAINT rollout_instance_effect_method
        CHECK (effect_method IN ('v2_fleet_control', 'v2', 'flag_only'));
`
