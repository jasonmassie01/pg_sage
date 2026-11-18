package schema

// ddlReviewCandidates (review-02a): the candidates a review's generators
// proposed, one row per (run, candidate). The id is the SHA-256 of the
// generator, kind, target and forward SQL, so it is stable across runs.
// A candidate goes with its run (ON DELETE CASCADE), so review_run's
// retention covers it. Status moves only from proposed (candidate.Store).
const ddlReviewCandidates = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_candidate (
    run_id          bigint NOT NULL REFERENCES sage.review_run (id) ON DELETE CASCADE,
    candidate_id    text NOT NULL CHECK (candidate_id ~ '^[0-9a-f]{64}$'),
    generator       text NOT NULL CHECK (length(generator) BETWEEN 1 AND 100),
    dimension       text NOT NULL CHECK (length(dimension) BETWEEN 1 AND 100),
    kind            text NOT NULL CHECK (length(kind) BETWEEN 1 AND 100),
    target          text NOT NULL CHECK (length(target) BETWEEN 1 AND 1000),
    action_type     text NOT NULL CHECK (length(action_type) BETWEEN 1 AND 100),
    forward_sql     text NOT NULL DEFAULT '',
    rollback_sql    text NOT NULL DEFAULT '',
    evidence_ids    text[] NOT NULL CHECK (cardinality(evidence_ids) > 0),
    prediction      jsonb NOT NULL DEFAULT '{}'::jsonb,
    reversible      boolean NOT NULL DEFAULT false,
    reason          text NOT NULL DEFAULT '',
    approval        text NOT NULL DEFAULT 'approval_required'
        CHECK (approval IN ('automatic', 'approval_required')),
    approval_reason text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'proposed'
        CHECK (status IN ('proposed', 'planned', 'rejected', 'superseded')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, candidate_id)
);
`
