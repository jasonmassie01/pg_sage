package schema

// Change Review receipts. All idempotent.
//
// landing_checked_at is when the receipts job last saw a review's catalog
// post-conditions not yet hold: the lower end of a catalog landing bracket.
// A review whose post-conditions already hold the first time it is checked
// (landing_checked_at still NULL) changed nothing: not applicable. The job
// writes it without touching updated_at (the retention clock).
//
// change_review_shadow_due holds the flagged reviews whose shadow
// auto-clear score is settled by their DBA verdict and receipt but not yet
// recorded, so the job scores them without scanning the table; its
// predicate is receipt.shadowDueSQL's and must stay identical to it.
const ddlChangeReviewReceipts = `
ALTER TABLE sage.change_review ADD COLUMN IF NOT EXISTS landing_checked_at timestamptz;
CREATE INDEX IF NOT EXISTS change_review_shadow_due
    ON sage.change_review (completed_at)
    WHERE shadow_auto_clear AND shadow_score = 'unscored'
      AND (receipt_verdict = 'regressed'
           OR dba_verdict IN ('override', 'concur_with_edits')
           OR (dba_verdict = 'concur'
               AND receipt_verdict IN ('clean', 'clean_low_traffic')));
`
