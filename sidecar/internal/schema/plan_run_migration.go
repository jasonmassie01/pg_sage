package schema

// ddlPlanStepRun (plan-02a) adds the plan engine's run columns:
//
//   - sage.plan_step.claimed_by / claimed_until: the engine that is
//     running the step and until when its claim holds (an engine never
//     transitions a step another live engine claimed);
//   - sage.plan_step.attempt: how many times the step was started (read by
//     plan-02e's resume);
//   - sage.plan_step.executed_at: when its action ran, from which the
//     step's verdict deadline counts;
//   - sage.plan.pause_reason: why a running plan is not advancing (the
//     emergency stop), empty otherwise.
//
// The partial index finds the plans the engine advances. ADD COLUMN IF NOT
// EXISTS with constant defaults only (no table rewrite); lock_timeout bounds
// the catalog locks on the first run.
const ddlPlanStepRun = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.plan_step
    ADD COLUMN IF NOT EXISTS claimed_by text CHECK (char_length(claimed_by) <= 200),
    ADD COLUMN IF NOT EXISTS claimed_until timestamptz,
    ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    ADD COLUMN IF NOT EXISTS executed_at timestamptz;
ALTER TABLE sage.plan
    ADD COLUMN IF NOT EXISTS pause_reason text NOT NULL DEFAULT ''
        CHECK (char_length(pause_reason) <= 200);
CREATE INDEX IF NOT EXISTS plan_runnable ON sage.plan (id)
    WHERE status IN ('approved', 'running');
`
