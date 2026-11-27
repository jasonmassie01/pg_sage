package schema

// ddlReviewViews (review-01b) adds sage.review_view: a person's first
// opening of a finished review, one row per run and user (the primary
// key, so two tabs record one row). It is the evidence that an owner read
// a review. Rows go with their run (ON DELETE CASCADE), so the run's
// retention covers them. user_id is the control database's sage.users id;
// it has no foreign key because a fleet keeps users in one database and
// reviews in each graded database's own sage schema.
const ddlReviewViews = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_view (
    run_id    bigint NOT NULL REFERENCES sage.review_run (id) ON DELETE CASCADE,
    user_id   bigint NOT NULL CHECK (user_id > 0),
    viewed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, user_id)
);
`
