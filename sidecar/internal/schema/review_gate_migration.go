package schema

// ddlReviewGate (review-05a): one row of sage.review_gate per evaluation of
// G-M1 (give the model more) or G-M2 (drop the model pass) for a normalized
// model id at an evaluation tick. UNIQUE (model, rule, tick) keeps two
// evaluations of one tick to one row. A G-M1 row is pending_approval until
// a person approves it (approved_by, approved_at, verdict granted); the
// CHECK refuses a granted row without an approval, so authority is never
// self-granted (CHECK-39). A revocation is its own row (revoked_at). The
// evidence is the counts the verdict read, with their Wilson intervals.
const ddlReviewGate = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_gate (
    id           bigserial PRIMARY KEY,
    model        text NOT NULL CHECK (length(model) BETWEEN 1 AND 200),
    rule         text NOT NULL CHECK (rule IN ('G-M1', 'G-M2')),
    tick         timestamptz NOT NULL,
    evaluated_at timestamptz NOT NULL DEFAULT now(),
    verdict      text NOT NULL CHECK (verdict IN ('not_eligible', 'eligible',
                     'pending_approval', 'granted', 'revoked', 'model_on', 'model_off')),
    reason       text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    evidence     jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(evidence) = 'object'),
    approved_by  text CHECK (length(approved_by) BETWEEN 1 AND 200),
    approved_at  timestamptz,
    revoked_at   timestamptz,
    UNIQUE (model, rule, tick),
    CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
    CHECK (verdict <> 'granted' OR approved_at IS NOT NULL),
    CHECK ((verdict = 'revoked') = (revoked_at IS NOT NULL)),
    CHECK ((rule = 'G-M2') = (verdict IN ('model_on', 'model_off')))
);
CREATE INDEX IF NOT EXISTS review_gate_latest ON sage.review_gate (rule, model, id DESC);
`
