package schema

// ddlPlanStepApproval (plan-02b) gives the plan engine the never-automatic
// step:
//
//   - sage.plan_step.state gains awaiting_approval: a step approve-all
//     never covers (a restart-bound setting, a rewrite, dropped data, ...)
//     waits there for its own approval. The inline CHECK is replaced once
//     by the versioned plan_step_state_v2 (catalog-guarded, so later boots
//     take no table lock);
//   - sage.plan_step.waiting_since: when a pending step first waited on a
//     plan cap or a peak window; its deadline counts from it.
//
// Every existing state stays legal, so no stored row is touched;
// lock_timeout bounds the catalog locks on the first run.
const ddlPlanStepApproval = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS waiting_since timestamptz;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plan_step_state_v2'
                 AND conrelid = 'sage.plan_step'::regclass) THEN
    ALTER TABLE sage.plan_step DROP CONSTRAINT IF EXISTS plan_step_state_check;
    ALTER TABLE sage.plan_step ADD CONSTRAINT plan_step_state_v2 CHECK (state IN (
        'pending', 'starting', 'queued', 'running', 'executed', 'judging', 'kept',
        'reverting', 'reverted', 'skipped', 'failed', 'awaiting_operator', 'halted',
        'awaiting_approval'));
  END IF;
END $$;
`
