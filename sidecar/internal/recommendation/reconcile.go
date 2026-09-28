package recommendation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// interruptedReason explains a recovered claim: the process died between
// applying and applied, so the outcome of that attempt is unknown.
const interruptedReason = "interrupted: the apply outcome was not recorded before " +
	"the claim's lease expired; retrying after backoff"

// RecoverStaleApplying resumes claims whose owner died between applying
// and applied. Once a lease has expired no worker can still hold it (the
// lease outlives the execution deadline), so the attempt is recorded as
// failed (or abandoned when the budget is spent), never re-proposed.
func (s *Store) RecoverStaleApplying(ctx context.Context, database string) (int, error) {
	claims, err := s.claimsWhere(ctx, database, `state = 'applying'
		AND (lease_until IS NULL OR lease_until < now())`)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, c := range claims {
		err := s.withTx(ctx, func(tx pgx.Tx) error {
			_, err := RecordFailureTx(ctx, tx, c, interruptedReason, nil)
			return err
		})
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

// claimsWhere lists heads of database matching a constant predicate as
// claims (id, revision, attempt).
func (s *Store) claimsWhere(ctx context.Context, database, where string) ([]Claim, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, revision, content_hash,
		attempt_count FROM sage.recommendation
		WHERE database_name = $1 AND `+where+` ORDER BY id`, database)
	if err != nil {
		return nil, fmt.Errorf("list recommendations: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claim, error) {
		var c Claim
		err := row.Scan(&c.ID, &c.Revision, &c.ContentHash, &c.Attempt)
		return c, err
	})
}

// effect is the verification view of one verifying recommendation.
type effect struct {
	claim        Claim
	verdict      string
	outcome      *string
	reason       string
	verification *string
}

// ReconcileVerifying records verification from durable facts (C15):
// the verdict (sage.verification, or the action's rollback outcome) is
// recorded on the head while it stays verifying; only a completed effect
// (retained, rolled back, or unverifiable) ends it. It also moves applied
// heads whose process died before StartVerifying. Idempotent.
func (s *Store) ReconcileVerifying(ctx context.Context, database string) (int, error) {
	applied, err := s.claimsWhere(ctx, database, `state = 'applied'`)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, c := range applied {
		if err := s.StartVerifying(ctx, c); err == nil {
			changed++
		} else if !errors.Is(err, ErrConflict) {
			return changed, err
		}
	}
	effects, err := s.verifyingEffects(ctx, database)
	if err != nil {
		return changed, err
	}
	for _, e := range effects {
		moved, err := s.settle(ctx, e)
		if err != nil {
			return changed, err
		}
		if moved {
			changed++
		}
	}
	return changed, nil
}

func (s *Store) verifyingEffects(ctx context.Context, database string) ([]effect, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT r.id, r.revision,
		r.attempt_count, COALESCE(r.verdict, ''), al.outcome,
		COALESCE(al.rollback_reason, ''), v.verdict
		FROM sage.recommendation r
		LEFT JOIN sage.action_log al ON al.id = r.action_log_id
		LEFT JOIN LATERAL (SELECT verdict FROM sage.verification
		                    WHERE action_log_id = r.action_log_id
		                    ORDER BY id DESC LIMIT 1) v ON true
		WHERE r.database_name = $1 AND r.state = 'verifying' ORDER BY r.id`, database)
	if err != nil {
		return nil, fmt.Errorf("list verifying recommendations: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (effect, error) {
		var e effect
		err := row.Scan(&e.claim.ID, &e.claim.Revision, &e.claim.Attempt, &e.verdict,
			&e.outcome, &e.reason, &e.verification)
		return e, err
	})
}

// settlement maps one effect to its verdict and, when the effect is
// complete, its terminal state ("" keeps verifying).
func settlement(e effect) (verdict string, final State) {
	if e.outcome == nil {
		return "missing", StateInconclusive
	}
	switch outcome := *e.outcome; outcome {
	case "success":
		return "success", StateVerified
	case "rolled_back":
		return "regressed", StateReverted
	case "unverifiable", "failed":
		return outcome, StateInconclusive
	case "rollback_failed", "rollback_skipped":
		return "regressed", ""
	}
	if e.verification == nil {
		return e.verdict, ""
	}
	switch *e.verification {
	case "revert":
		return "regressed", ""
	case "unverifiable":
		return "unverifiable", ""
	default:
		return e.verdict, ""
	}
}

// settle records e's verdict and, when complete, its terminal state.
func (s *Store) settle(ctx context.Context, e effect) (bool, error) {
	verdict, final := settlement(e)
	if final == "" && verdict == e.verdict {
		return false, nil
	}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if final == "" {
			_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.recommendation
				SET verdict = $4, updated_at = now()
				WHERE id = $1 AND state = 'verifying' AND revision = $2
				  AND attempt_count = $3`,
				e.claim.ID, e.claim.Revision, e.claim.Attempt, verdict)
			return err
		}
		reason := verdict
		if e.reason != "" {
			reason = verdict + ": " + e.reason
		}
		m := move{id: e.claim.ID, from: StateVerifying, to: final,
			revision: e.claim.Revision, attempt: e.claim.Attempt, actor: "verifier",
			reason: reason}
		return applyMove(ctx, tx, m, `, verdict = $6, reason = $7`, verdict, reason)
	})
	if errors.Is(err, ErrConflict) {
		return false, nil
	}
	return err == nil, err
}
