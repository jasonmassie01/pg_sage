package schema

// Change Review receipts. All idempotent.
//
// landing_checked_at is when a review's catalog post-conditions were last
// seen not to hold: at completion (the review time check) and then by the
// receipts job each cycle; the lower end of a catalog landing bracket. A
// review whose post-conditions held at review time is not applicable from
// completion. The job writes it without touching updated_at (the
// retention clock). change_review_landing_rotation serves the awaiting
// reviews checked longest ago first (never checked first), so every one
// gets its turn however many wait.
//
// change_review_shadow_due holds the flagged reviews whose shadow
// auto-clear score is settled by their DBA verdict and receipt but not yet
// recorded, so the job scores them without scanning the table; its
// predicate is receipt.shadowDueSQL's and must stay identical to it.
const ddlChangeReviewReceipts = `
ALTER TABLE sage.change_review ADD COLUMN IF NOT EXISTS landing_checked_at timestamptz;
CREATE INDEX IF NOT EXISTS change_review_landing_rotation
    ON sage.change_review (landing_checked_at NULLS FIRST, completed_at)
    WHERE landing_state = 'awaiting';
CREATE INDEX IF NOT EXISTS change_review_shadow_due
    ON sage.change_review (completed_at)
    WHERE shadow_auto_clear AND shadow_score = 'unscored'
      AND (receipt_verdict = 'regressed'
           OR dba_verdict IN ('override', 'concur_with_edits')
           OR (dba_verdict = 'concur'
               AND receipt_verdict IN ('clean', 'clean_low_traffic')));
`
