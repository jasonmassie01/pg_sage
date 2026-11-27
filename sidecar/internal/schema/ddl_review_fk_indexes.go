package schema

// ddlReviewForeignKeyIndexes indexes the two Sage Review foreign keys that
// were added without one (T6 round 1, for review-03b and review-04a), so
// deleting a parent row never scans the child table once per row:
//
//   - sage.plan_step_origin(run_id) -> sage.review_run, ON DELETE SET NULL:
//     purging old review runs had to read every step origin per run.
//   - sage.review_trigger(coalesced_into) -> sage.review_trigger, ON DELETE
//     SET NULL: deleting a trigger read the whole trigger queue.
//
// Both columns are NULL for most rows, so the indexes are partial; a
// foreign key check (col = $1) implies col IS NOT NULL and uses them.
const ddlReviewForeignKeyIndexes = `
SET LOCAL lock_timeout = '5s';
CREATE INDEX IF NOT EXISTS plan_step_origin_run
    ON sage.plan_step_origin (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS review_trigger_coalesced
    ON sage.review_trigger (coalesced_into) WHERE coalesced_into IS NOT NULL;
`
