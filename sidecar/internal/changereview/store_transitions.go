package changereview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// transitionAttempts bounds re-reads when a concurrent writer changed a
// fact a transition depends on (a receipt decided while a DBA decides).
const transitionAttempts = 4

// checkDBAVerdict validates a DBA decision.
func checkDBAVerdict(v DBAVerdict) error {
	if err := checkActor(v.Actor); err != nil {
		return err
	}
	switch v.Verdict {
	case DBAConcur, DBAConcurWithEdits, DBAOverride:
	default:
		return invalid("verdict %q is not concur, concur_with_edits or override", v.Verdict)
	}
	switch v.Direction {
	case "", DirectionTooStrict, DirectionTooLenient:
	default:
		return invalid("direction %q is not too_strict or too_lenient", v.Direction)
	}
	if utf8.RuneCountInString(v.Reason) > MaxReasonRunes || !utf8.ValidString(v.Reason) {
		return invalid("reason must be valid text of at most %d characters", MaxReasonRunes)
	}
	if v.Verdict == DBAOverride && (strings.TrimSpace(v.Reason) == "" || v.Direction == "") {
		return invalid("an override needs a reason and a direction (too_strict or " +
			"too_lenient)")
	}
	return nil
}

// Decide records a DBA verdict on a complete review that has none, and
// moves its credit (credit.go). A second verdict is ErrAlreadyDecided; a
// running or failed review is ErrInvalid.
func (st *Store) Decide(ctx context.Context, id string, v DBAVerdict) (Review, error) {
	if err := checkDBAVerdict(v); err != nil {
		return Review{}, err
	}
	toil, err := st.ToilModel(ctx)
	if err != nil {
		return Review{}, err
	}
	for attempt := 0; attempt < transitionAttempts; attempt++ {
		cur, err := st.Get(ctx, id)
		if err != nil {
			return Review{}, err
		}
		if cur.State != StateComplete {
			return Review{}, invalid("review %s is %s; only a complete review takes a "+
				"verdict", id, cur.State)
		}
		if cur.DBAVerdict != "" {
			return Review{}, fmt.Errorf("%w: review %s has verdict %s by %s",
				ErrAlreadyDecided, id, cur.DBAVerdict, cur.DBAActor)
		}
		now := utc(st.now())
		c := NextCredit(cur.Credit(), CreditFacts{DBAVerdict: v.Verdict,
			ReceiptVerdict: cur.ReceiptVerdict, LandingState: cur.LandingState}, toil, now)
		out, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_decide v1 */
			UPDATE sage.change_review SET dba_verdict = $2, dba_direction = $3,
			dba_reason = $4, dba_actor = $5, dba_decided_at = $6, credit_state = $7,
			credited_minutes = $8, toil_model_version = $9, credit_final_at = $10,
			updated_at = now()
			WHERE id = $1::uuid AND state = 'complete' AND dba_verdict IS NULL
			  AND receipt_verdict IS NOT DISTINCT FROM $11
			  AND landing_state IS NOT DISTINCT FROM $12 AND credit_state = $13
			RETURNING `+selectFull, id, v.Verdict, nullText(v.Direction),
			nullText(strings.TrimSpace(v.Reason)), v.Actor, now, c.State, c.Minutes,
			nullInt(c.ToilModelVersion), c.FinalAt, nullText(cur.ReceiptVerdict),
			nullText(cur.LandingState), cur.CreditState))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Review{}, fmt.Errorf("%w: record the verdict: %v", ErrUnavailable, err)
		}
		return out, nil
	}
	return Review{}, fmt.Errorf("%w: review %s kept changing", ErrUnavailable, id)
}

// MarkLanded records when a complete, awaiting review's change landed.
func (st *Store) MarkLanded(ctx context.Context, id string, l Landing) (Review, error) {
	switch l.Source {
	case LandingSourceCatalog, LandingSourceChangeEvent, LandingSourceReported:
	default:
		return Review{}, invalid("landing source %q", l.Source)
	}
	if l.At.IsZero() || l.Lower.IsZero() || l.Lower.After(l.At) {
		return Review{}, invalid("a landing needs lower <= at, both set")
	}
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_landed v1 */
		UPDATE sage.change_review SET landing_state = 'landed', landing_source = $2,
		landed_lower = $3, landed_at = $4, updated_at = now()
		WHERE id = $1::uuid AND state = 'complete' AND landing_state = 'awaiting'
		RETURNING `+selectFull, id, l.Source, utc(l.Lower), utc(l.At))
	return st.transitioned(ctx, row, id, "landing")
}

// MarkNotApplicable records that a complete review's post-conditions
// already held when it completed: there is nothing to land, so no receipt
// can settle a credit and a pending one becomes none (NextCredit).
func (st *Store) MarkNotApplicable(ctx context.Context, id string) (Review, error) {
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_not_applicable v1 */
		UPDATE sage.change_review SET landing_state = 'not_applicable',
		credit_state = 'none', credited_minutes = 0, toil_model_version = NULL,
		credit_final_at = NULL, updated_at = now()
		WHERE id = $1::uuid AND state = 'complete' AND landing_state = 'awaiting'
		  AND credit_state IN ('none', 'pending')
		RETURNING `+selectFull, id)
	return st.transitioned(ctx, row, id, "landing")
}

// SetShadowScore scores a complete review's would-auto-clear flag once:
// correct or incorrect.
func (st *Store) SetShadowScore(ctx context.Context, id, score string) (Review, error) {
	if score != ShadowCorrect && score != ShadowIncorrect {
		return Review{}, invalid("shadow score %q is not correct or incorrect", score)
	}
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_shadow v1 */
		UPDATE sage.change_review SET shadow_score = $2, updated_at = now()
		WHERE id = $1::uuid AND state = 'complete' AND shadow_score = 'unscored'
		RETURNING `+selectFull, id, score)
	return st.transitioned(ctx, row, id, "shadow score")
}

// transitioned maps a conditional UPDATE ... RETURNING that matched no
// row: an unknown review, one that is not complete, or one already past
// the transition.
func (st *Store) transitioned(ctx context.Context, row pgx.Row, id, what string) (Review,
	error) {
	out, err := scanReview(row)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Review{}, fmt.Errorf("%w: record the %s: %v", ErrUnavailable, what, err)
	}
	cur, err := st.Get(ctx, id)
	if err != nil {
		return Review{}, err
	}
	if cur.State != StateComplete {
		return Review{}, invalid("review %s is %s, not complete", id, cur.State)
	}
	return Review{}, fmt.Errorf("%w: the %s of review %s is already recorded",
		ErrAlreadyDecided, what, id)
}

// DecideReceipt seals a landed review's receipt once (receipt_hash) and
// moves its credit. The verdict must be clean, clean_low_traffic,
// regressed or unverifiable.
func (st *Store) DecideReceipt(ctx context.Context, id string, rc Receipt) (Review, error) {
	switch rc.Verdict {
	case ReceiptClean, ReceiptCleanLowTraffic, ReceiptRegressed, ReceiptUnverifiable:
	default:
		return Review{}, invalid("receipt verdict %q is not decided", rc.Verdict)
	}
	return st.seal(ctx, id, func(cur Review) (Review, error) {
		if cur.ReceiptVerdict != ReceiptPending {
			return cur, fmt.Errorf("%w: review %s has receipt %s", ErrAlreadyDecided, id,
				cur.ReceiptVerdict)
		}
		if cur.LandingState != LandingLanded {
			return cur, invalid("review %s has not landed (%s)", id, cur.LandingState)
		}
		cur.ReceiptVerdict, cur.Receipt = rc.Verdict, &rc
		return cur, nil
	})
}

// Expire ends an awaiting review that never landed: landing expired,
// receipt unverifiable (sealed), credit void.
func (st *Store) Expire(ctx context.Context, id string) (Review, error) {
	return st.seal(ctx, id, func(cur Review) (Review, error) {
		if cur.LandingState != LandingAwaiting {
			return cur, fmt.Errorf("%w: review %s landing is %s", ErrAlreadyDecided, id,
				cur.LandingState)
		}
		cur.LandingState, cur.ReceiptVerdict = LandingExpired, ReceiptUnverifiable
		cur.Receipt = &Receipt{Verdict: ReceiptUnverifiable, Reason: "landing_expired"}
		return cur, nil
	})
}
