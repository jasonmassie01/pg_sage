package schema

// ddlReviewCandidateOperator (review-02b): an operator candidate's fields
// (the command an operator runs, its revert, the landing setting and
// value, the provider), as one JSON object; NULL on every other
// candidate. The column has no default, so adding it rewrites no row.
const ddlReviewCandidateOperator = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.review_candidate ADD COLUMN IF NOT EXISTS operator jsonb
    CHECK (operator IS NULL OR jsonb_typeof(operator) = 'object');
`
