package schema

// ddlPlanOperatorSteps (plan-02d) gives an operator step ("you run this")
// its columns on sage.plan_step:
//
//   - operator_command / operator_revert: what the operator is asked to
//     run, and what undoes it (pg_sage runs neither);
//   - landing: the pg_settings check that sees the change land (a setting
//     and its expected value), NULL when the managed change is the check;
//   - managed_change_id: the linked sage.managed_change_proposals row,
//     whose applied status lands the step;
//   - landed_at: when the change landed; the verdict deadline counts from
//     it.
//
// Each column is added with a constant default (no table rewrite); every
// stored row stays legal; lock_timeout bounds the catalog lock.
const ddlPlanOperatorSteps = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS operator_command text NOT NULL DEFAULT ''
    CHECK (char_length(operator_command) <= 10000);
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS operator_revert text NOT NULL DEFAULT ''
    CHECK (char_length(operator_revert) <= 10000);
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS landing jsonb
    CHECK (landing IS NULL OR jsonb_typeof(landing) = 'object');
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS managed_change_id bigint;
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS landed_at timestamptz;
`
