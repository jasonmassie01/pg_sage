package changereview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxQueueRead bounds one read of a receipt queue.
const maxQueueRead = 1000

// seal applies edit (which sets the landing state, receipt verdict and
// receipt) to a complete review, stamps receipt_decided_at, moves the
// credit and stores the result with its receipt_hash, once.
func (st *Store) seal(ctx context.Context, id string,
	edit func(Review) (Review, error)) (Review, error) {
	for attempt := 0; attempt < transitionAttempts; attempt++ {
		cur, err := st.Get(ctx, id)
		if err != nil {
			return Review{}, err
		}
		if cur.State != StateComplete {
			return Review{}, invalid("review %s is %s, not complete", id, cur.State)
		}
		next, err := edit(cur)
		if err != nil {
			return Review{}, err
		}
		now := utc(st.now())
		next.ReceiptDecidedAt = &now
		c := NextCredit(cur.Credit(), CreditFacts{DBAVerdict: cur.DBAVerdict,
			Carried: cur.VerdictCarriedFrom != "", ReceiptVerdict: next.ReceiptVerdict,
			LandingState: next.LandingState}, ToilModel{Version: cur.Credit().ToilModelVersion,
			Minutes: cur.CreditedMinutes}, now)
		hash, err := ReceiptHash(next)
		if err != nil {
			return Review{}, fmt.Errorf("%w: seal the receipt: %v", ErrInvalid, err)
		}
		j, err := jsonColumns(next)
		if err != nil {
			return Review{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		out, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_seal v1 */
			UPDATE sage.change_review SET landing_state = $2, receipt_verdict = $3,
			receipt = $4, receipt_decided_at = $5, receipt_hash = $6, credit_state = $7,
			credited_minutes = $8, credit_final_at = $9, updated_at = now()
			WHERE id = $1::uuid AND state = 'complete' AND receipt_verdict = 'pending'
			  AND landing_state = $10 AND dba_verdict IS NOT DISTINCT FROM $11
			  AND credit_state = $12
			RETURNING `+selectFull, id, next.LandingState, next.ReceiptVerdict, j.receipt,
			now, hash, c.State, c.Minutes, c.FinalAt, cur.LandingState,
			nullText(cur.DBAVerdict), cur.CreditState))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Review{}, fmt.Errorf("%w: seal the receipt: %v", ErrUnavailable, err)
		}
		return out, nil
	}
	return Review{}, fmt.Errorf("%w: review %s kept changing", ErrUnavailable, id)
}

// ToilModel is the active 'change_review' unit of credit.
func (st *Store) ToilModel(ctx context.Context) (ToilModel, error) {
	var t ToilModel
	err := st.pool.QueryRow(ctx, `/* pg_sage change_review_toil v1 */
		SELECT model_version, round(base_minutes)::int FROM sage.toil_model
		WHERE action_type = 'change_review' AND effective_to IS NULL`).Scan(&t.Version,
		&t.Minutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToilModel{}, fmt.Errorf("%w: no active change_review toil model",
			ErrUnavailable)
	}
	if err != nil {
		return ToilModel{}, fmt.Errorf("%w: read the toil model: %v", ErrUnavailable, err)
	}
	return t, nil
}

// findReusable is the newest running or complete review with inputHash
// created after since.
func (st *Store) findReusable(ctx context.Context, inputHash string,
	since time.Time) (Review, bool, error) {
	r, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_dedupe v1 */
		SELECT `+selectFull+` FROM sage.change_review
		WHERE input_hash = $1 AND created_at > $2 AND state <> 'failed'
		ORDER BY created_at DESC LIMIT 1`, inputHash, since))
	return optionalReview(r, err, "dedupe")
}

// latestForPR is the newest review of prURL other than exclude.
func (st *Store) latestForPR(ctx context.Context, prURL, exclude string) (Review, bool,
	error) {
	r, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_latest_pr v1 */
		SELECT `+selectBrief+` FROM sage.change_review
		WHERE pr_url = $1 AND id <> $2::uuid ORDER BY created_at DESC LIMIT 1`, prURL,
		exclude))
	return optionalReview(r, err, "latest PR review")
}

// latestCompleteForPR is the newest complete review of prURL.
func (st *Store) latestCompleteForPR(ctx context.Context, prURL string) (Review, bool,
	error) {
	r, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_pr_complete v1 */
		SELECT `+selectBrief+` FROM sage.change_review
		WHERE pr_url = $1 AND state = 'complete' ORDER BY created_at DESC LIMIT 1`, prURL))
	return optionalReview(r, err, "latest complete PR review")
}

// carrySource is the newest decided review of prURL with contentHash,
// other than exclude: the verdict a new revision may carry over.
func (st *Store) carrySource(ctx context.Context, prURL, contentHash,
	exclude string) (Review, bool, error) {
	r, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_carry v1 */
		SELECT `+selectBrief+` FROM sage.change_review
		WHERE pr_url = $1 AND content_hash = $2 AND id <> $3::uuid
		  AND state = 'complete' AND dba_verdict IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, prURL, contentHash, exclude))
	return optionalReview(r, err, "carry-over source")
}

// waitingAtLeast reports whether an earlier complete revision of r's PR
// with a verdict at least as severe as r's still waits for a DBA verdict
// (through change_review_pr_created).
func (st *Store) waitingAtLeast(ctx context.Context, r Review) (bool, error) {
	severe := []string{VerdictRisky}
	if r.Verdict == VerdictReview {
		severe = append(severe, VerdictReview)
	}
	var waiting bool
	err := st.pool.QueryRow(ctx, `/* pg_sage change_review_paged v1 */
		SELECT EXISTS (SELECT 1 FROM sage.change_review
		WHERE pr_url = $1 AND created_at < $2 AND id <> $3::uuid AND state = 'complete'
		  AND dba_verdict IS NULL AND verdict = ANY($4::text[]))`, r.PRURL, r.CreatedAt,
		r.ID, severe).Scan(&waiting)
	if err != nil {
		return false, fmt.Errorf("%w: read the PR's earlier revisions: %v", ErrUnavailable,
			err)
	}
	return waiting, nil
}

func optionalReview(r Review, err error, what string) (Review, bool, error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Review{}, false, nil
	case err != nil:
		return Review{}, false, fmt.Errorf("%w: read the %s: %v", ErrUnavailable, what, err)
	}
	return r, true, nil
}

// ListAwaiting is up to limit complete reviews waiting to land: those
// never checked first (oldest completion first), then those checked
// longest ago, so a cycle that cannot take them all leaves the rest for
// the next one (through change_review_landing_rotation).
func (st *Store) ListAwaiting(ctx context.Context, limit int) ([]Review, error) {
	if limit < 1 || limit > maxQueueRead {
		return nil, invalid("limit must be 1-%d, got %d", maxQueueRead, limit)
	}
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_awaiting v2 */
		SELECT `+selectBrief+` FROM sage.change_review
		WHERE landing_state = 'awaiting'
		ORDER BY landing_checked_at NULLS FIRST, completed_at LIMIT $1`, limit)
	return listResult(rows, err)
}

// ListReceiptsDue is up to limit landed reviews with a pending receipt
// that landed at or before landedBy (the window has closed), oldest first
// (through change_review_receipt_due).
func (st *Store) ListReceiptsDue(ctx context.Context, landedBy time.Time,
	limit int) ([]Review, error) {
	if limit < 1 || limit > maxQueueRead {
		return nil, invalid("limit must be 1-%d, got %d", maxQueueRead, limit)
	}
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_receipts_due v1 */
		SELECT `+selectBrief+` FROM sage.change_review
		WHERE landing_state = 'landed' AND receipt_verdict = 'pending' AND landed_at <= $1
		ORDER BY landed_at LIMIT $2`, landedBy, limit)
	return listResult(rows, err)
}
