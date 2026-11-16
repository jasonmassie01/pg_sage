package schema

// ddlPlanApprovalModes (plan-03a) records how a plan was approved and adds
// the per-step approval:
//
//   - sage.plan_approval.mode: all (every step except the never-automatic
//     ones), up_to (ordinals 1..up_to_ordinal except those) or step (no step
//     by itself); rows written before this step were approve-all, so the
//     default is all. via is where the person decided (ui, api, chat, cli);
//     NULL on rows written before this step.
//   - sage.plan_step_approval: one person's approval of one step at the plan
//     hash they were shown, for step-by-step plans and never-automatic
//     steps. At most one live approval per step; an edit voids them all.
//   - sage.plan_step.resumed_at: when a person resumed the plan past this
//     step's halt; the engine no longer halts on it.
//
// ADD COLUMN IF NOT EXISTS with constant defaults (no table rewrite), the
// cross-column check catalog-guarded, the new table IF NOT EXISTS;
// lock_timeout bounds the catalog locks on the first run.
const ddlPlanApprovalModes = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.plan_approval
    ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'all'
        CHECK (mode IN ('all', 'up_to', 'step')),
    ADD COLUMN IF NOT EXISTS up_to_ordinal integer CHECK (up_to_ordinal BETWEEN 1 AND 50),
    ADD COLUMN IF NOT EXISTS via text CHECK (via IN ('ui', 'api', 'chat', 'cli'));
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plan_approval_up_to_v1'
                 AND conrelid = 'sage.plan_approval'::regclass) THEN
    ALTER TABLE sage.plan_approval ADD CONSTRAINT plan_approval_up_to_v1
        CHECK ((mode = 'up_to') = (up_to_ordinal IS NOT NULL));
  END IF;
END $$;
ALTER TABLE sage.plan_step ADD COLUMN IF NOT EXISTS resumed_at timestamptz;
CREATE TABLE IF NOT EXISTS sage.plan_step_approval (
    id                bigserial PRIMARY KEY,
    plan_id           bigint NOT NULL,
    step_id           bigint NOT NULL,
    plan_hash         text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    approver_user_id  integer NOT NULL CHECK (approver_user_id > 0),
    via               text NOT NULL CHECK (via IN ('ui', 'api', 'chat', 'cli')),
    decided_at        timestamptz NOT NULL DEFAULT now(),
    voided_at         timestamptz,
    void_reason       text CHECK (char_length(void_reason) BETWEEN 1 AND 200),
    CHECK ((voided_at IS NULL) = (void_reason IS NULL)),
    FOREIGN KEY (plan_id, step_id) REFERENCES sage.plan_step (plan_id, id)
        ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS plan_step_approval_live
    ON sage.plan_step_approval (step_id) WHERE voided_at IS NULL;
CREATE INDEX IF NOT EXISTS plan_step_approval_plan
    ON sage.plan_step_approval (plan_id, step_id);
`
