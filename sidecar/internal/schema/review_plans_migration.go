package schema

// ddlReviewPlans (review-03b): a review ends as one plan-01 plan, and the
// review records what plan-01 does not.
//
//   - sage.review_plan: the run's one plan (plan_id NULL when the review
//     proposed no change), its hash and step counts by origin. The run id
//     is the key, so a re-run of a review finds its plan instead of
//     building another.
//   - sage.plan_step_origin: who proposed each plan step (generator, model
//     or person, with the generator's name or the model id), its evidence
//     ids, the ladder prediction and the measurement spec handed to the
//     verifier. A step of a model-origin plan without a row here counts as
//     model-proposed (changeplan's origin hold fails closed).
//   - sage.review_advice: every hypothesis or candidate that did not become
//     a step, with the reason (no template, unknown object, ladder error,
//     next review, ...).
//   - sage.review_model_authority: the flag review-05a's G-M1 sets for a
//     model id once a person approves it. Without a live row, a model's
//     steps need their own approval whatever their class's trust.
const ddlReviewPlans = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_plan (
    run_id          bigint PRIMARY KEY REFERENCES sage.review_run (id) ON DELETE CASCADE,
    plan_id         bigint REFERENCES sage.plan (id) ON DELETE SET NULL,
    plan_hash       text CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    generator_steps integer NOT NULL DEFAULT 0 CHECK (generator_steps >= 0),
    model_steps     integer NOT NULL DEFAULT 0 CHECK (model_steps >= 0),
    hypotheses      integer NOT NULL DEFAULT 0 CHECK (hypotheses >= 0),
    advice          integer NOT NULL DEFAULT 0 CHECK (advice >= 0),
    message         text NOT NULL DEFAULT '' CHECK (length(message) <= 1000),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS review_plan_plan ON sage.review_plan (plan_id);
CREATE TABLE IF NOT EXISTS sage.plan_step_origin (
    step_id      bigint PRIMARY KEY REFERENCES sage.plan_step (id) ON DELETE CASCADE,
    plan_id      bigint NOT NULL REFERENCES sage.plan (id) ON DELETE CASCADE,
    run_id       bigint REFERENCES sage.review_run (id) ON DELETE SET NULL,
    origin       text NOT NULL CHECK (origin IN ('generator', 'model', 'person')),
    origin_ref   text NOT NULL CHECK (length(origin_ref) BETWEEN 1 AND 200),
    evidence_ids text[] NOT NULL DEFAULT '{}',
    prediction   jsonb NOT NULL DEFAULT '{}'::jsonb,
    measurement  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS plan_step_origin_plan ON sage.plan_step_origin (plan_id);
CREATE TABLE IF NOT EXISTS sage.review_advice (
    id           bigserial PRIMARY KEY,
    run_id       bigint NOT NULL REFERENCES sage.review_run (id) ON DELETE CASCADE,
    ordinal      integer NOT NULL CHECK (ordinal >= 1),
    origin       text NOT NULL CHECK (origin IN ('generator', 'model', 'person')),
    origin_ref   text NOT NULL DEFAULT '' CHECK (length(origin_ref) <= 200),
    kind         text NOT NULL CHECK (length(kind) BETWEEN 1 AND 100),
    target       text NOT NULL DEFAULT '' CHECK (length(target) <= 1000),
    title        text NOT NULL DEFAULT '' CHECK (length(title) <= 1000),
    reason       text NOT NULL CHECK (reason IN ('no_template', 'no_contract',
                     'unknown_object', 'invalid_target', 'no_evidence', 'sql_refused',
                     'ladder_error', 'no_ladder_tier', 'below_minimum', 'not_measurable',
                     'next_review', 'advice_only')),
    detail       text NOT NULL CHECK (length(detail) BETWEEN 1 AND 2000),
    evidence_ids text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, ordinal)
);
CREATE TABLE IF NOT EXISTS sage.review_model_authority (
    model      text PRIMARY KEY CHECK (length(model) BETWEEN 1 AND 200),
    granted_by text NOT NULL CHECK (length(granted_by) BETWEEN 1 AND 200),
    granted_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    reason     text NOT NULL DEFAULT '' CHECK (length(reason) <= 1000)
);
`
