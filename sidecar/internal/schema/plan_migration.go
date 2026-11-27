package schema

// ddlPlans (plan-01a) creates the plan objects in the monitored database's
// sage schema, where sage.action_queue lives, so a step's queue item and
// its plan share a pool (internal/changeplan):
//
//   - sage.plan: one multi-step piece of work; plan_hash is the hash of
//     every hashed field (changeplan.PlanHash), pack_hash the evidence
//     pack it was built from (NULL: no pack).
//   - sage.plan_step: one typed step; the runtime columns (state onward)
//     are the engine's and outside the hash. The full state vocabulary is
//     set up front. on_unverifiable NULL means undeclared (halt and ask).
//   - sage.plan_step_edge: from must finish before to; both steps belong
//     to the edge's plan (composite foreign keys).
//   - sage.plan_approval: who approved which hash; at most one live
//     (unvoided) approval per plan.
//
// The link from a step to the ledger (queue_id, action_log_id,
// decision_id) is held here, so no chained table changes. CREATE ... IF
// NOT EXISTS only; lock_timeout bounds the catalog locks on the first run.
const ddlPlans = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.plan (
    id              bigserial PRIMARY KEY,
    database_id     integer,
    goal            text NOT NULL CHECK (char_length(goal) BETWEEN 1 AND 500),
    scope           jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(scope) = 'object'),
    origin          text NOT NULL
                        CHECK (origin IN ('generator', 'model', 'person', 'agent')),
    origin_ref      text NOT NULL DEFAULT '' CHECK (char_length(origin_ref) <= 200),
    pack_hash       text CHECK (pack_hash ~ '^[0-9a-f]{64}$'),
    model           text NOT NULL DEFAULT '' CHECK (char_length(model) <= 200),
    prompt_version  text NOT NULL DEFAULT '' CHECK (char_length(prompt_version) <= 100),
    status          text NOT NULL CHECK (status IN ('draft', 'proposed', 'approved',
                        'running', 'halted', 'completed', 'reverted', 'rejected',
                        'voided', 'cancelled')),
    plan_hash       text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    created_by      text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 200)
);
CREATE INDEX IF NOT EXISTS plan_updated ON sage.plan (updated_at);
CREATE INDEX IF NOT EXISTS plan_pack ON sage.plan (pack_hash) WHERE pack_hash IS NOT NULL;
CREATE TABLE IF NOT EXISTS sage.plan_step (
    id                  bigserial PRIMARY KEY,
    plan_id             bigint NOT NULL REFERENCES sage.plan (id) ON DELETE CASCADE,
    ordinal             integer NOT NULL CHECK (ordinal BETWEEN 1 AND 50),
    kind                text NOT NULL CHECK (kind IN ('action', 'operator', 'check')),
    action_type         text NOT NULL DEFAULT '' CHECK (char_length(action_type) <= 100),
    setting             text NOT NULL DEFAULT '' CHECK (char_length(setting) <= 100),
    proposed_sql        text NOT NULL DEFAULT '' CHECK (char_length(proposed_sql) <= 10000),
    rollback_sql        text NOT NULL DEFAULT '' CHECK (char_length(rollback_sql) <= 10000),
    finding_id          bigint,
    evidence_ids        text[],
    target_queryids     bigint[],
    metric              text NOT NULL CHECK (metric IN ('blocks_per_call', 'time_per_call',
                            'wal_per_tuple', 'readback')),
    expected_direction  text NOT NULL CHECK (expected_direction IN ('down', 'up', 'none')),
    expected_pct        numeric,
    expected_source     text NOT NULL
                            CHECK (expected_source IN ('hypopg', 'clone', 'canary', 'none')),
    guardrails          jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(guardrails) = 'array'),
    min_evidence        jsonb NOT NULL DEFAULT '{}'
                            CHECK (jsonb_typeof(min_evidence) = 'object'),
    deadline_minutes    integer NOT NULL CHECK (deadline_minutes BETWEEN 1 AND 43200),
    checkpoint          boolean NOT NULL DEFAULT false,
    on_improved         text NOT NULL CHECK (on_improved IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    on_no_change        text NOT NULL CHECK (on_no_change IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    on_regressed        text NOT NULL CHECK (on_regressed IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    on_inconclusive     text NOT NULL CHECK (on_inconclusive IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    on_unverifiable     text CHECK (on_unverifiable IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    on_expired          text NOT NULL CHECK (on_expired IN ('keep', 'revert_step',
                            'revert_to_checkpoint', 'halt_ask')),
    state               text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending',
                            'starting', 'queued', 'running', 'executed', 'judging', 'kept',
                            'reverting', 'reverted', 'skipped', 'failed',
                            'awaiting_operator', 'halted')),
    queue_id            bigint,
    action_log_id       bigint,
    decision_id         bigint,
    verdict             text CHECK (verdict IN ('improved', 'no_change', 'regressed',
                            'inconclusive', 'unverifiable', 'expired')),
    verdict_at          timestamptz,
    state_reason        text NOT NULL DEFAULT '' CHECK (char_length(state_reason) <= 1000),
    UNIQUE (plan_id, ordinal),
    UNIQUE (plan_id, id)
);
CREATE TABLE IF NOT EXISTS sage.plan_step_edge (
    plan_id         bigint NOT NULL REFERENCES sage.plan (id) ON DELETE CASCADE,
    from_step_id    bigint NOT NULL,
    to_step_id      bigint NOT NULL CHECK (to_step_id <> from_step_id),
    PRIMARY KEY (plan_id, from_step_id, to_step_id),
    FOREIGN KEY (plan_id, from_step_id) REFERENCES sage.plan_step (plan_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (plan_id, to_step_id) REFERENCES sage.plan_step (plan_id, id)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS plan_step_edge_to ON sage.plan_step_edge (plan_id, to_step_id);
CREATE TABLE IF NOT EXISTS sage.plan_approval (
    id                bigserial PRIMARY KEY,
    plan_id           bigint NOT NULL REFERENCES sage.plan (id) ON DELETE CASCADE,
    plan_hash         text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    approver_user_id  integer NOT NULL CHECK (approver_user_id > 0),
    decided_at        timestamptz NOT NULL DEFAULT now(),
    voided_at         timestamptz,
    void_reason       text CHECK (char_length(void_reason) BETWEEN 1 AND 200),
    CHECK ((voided_at IS NULL) = (void_reason IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS plan_approval_live
    ON sage.plan_approval (plan_id) WHERE voided_at IS NULL;
CREATE INDEX IF NOT EXISTS plan_approval_plan ON sage.plan_approval (plan_id, decided_at);
`
