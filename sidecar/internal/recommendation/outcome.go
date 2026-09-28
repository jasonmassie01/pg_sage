package recommendation

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RecordAppliedTx moves a claim from applying to applied and links the
// action_log row, inside the transaction that inserted that row: the
// ledger and the state machine commit together or not at all.
func RecordAppliedTx(ctx context.Context, tx pgx.Tx, c Claim, actionLogID int64) error {
	m := move{id: c.ID, from: StateApplying, to: StateApplied, revision: c.Revision,
		attempt: c.Attempt, actor: ActorExecutor, reason: "applied",
		actionLogID: &actionLogID}
	return applyMove(ctx, tx, m, `, action_log_id = $6, lease_until = NULL`, actionLogID)
}

// RecordApplied is RecordAppliedTx in its own transaction.
func (s *Store) RecordApplied(ctx context.Context, c Claim, actionLogID int64) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		return RecordAppliedTx(ctx, tx, c, actionLogID)
	})
}

// RecordFailureTx moves a claim from applying to failed with a backoff
// (decision a: never back to proposed). When the attempts exceed the
// retry budget it continues to abandoned, recording why. It returns the
// state the recommendation ends in.
func RecordFailureTx(
	ctx context.Context, tx pgx.Tx, c Claim, reason string, actionLogID *int64,
) (State, error) {
	var attempts, budget int
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT attempt_count, retry_budget
		FROM sage.recommendation WHERE id = $1 FOR UPDATE`, c.ID).
		Scan(&attempts, &budget); err != nil {
		return "", fmt.Errorf("%w: recommendation %d: %v", ErrConflict, c.ID, err)
	}
	m := move{id: c.ID, from: StateApplying, to: StateFailed, revision: c.Revision,
		attempt: c.Attempt, actor: ActorExecutor, reason: reason, actionLogID: actionLogID}
	err := applyMove(ctx, tx, m, `, reason = $6, lease_until = NULL,
		next_attempt_at = now() + make_interval(secs => $7),
		action_log_id = COALESCE($8, action_log_id)`,
		reason, Backoff(c.Attempt).Seconds(), actionLogID)
	if err != nil || attempts <= budget {
		return StateFailed, err
	}
	why := fmt.Sprintf("retry budget exhausted after %d attempts: %s", attempts, reason)
	m = move{id: c.ID, from: StateFailed, to: StateAbandoned, revision: c.Revision,
		attempt: c.Attempt, actor: ActorExecutor, reason: why}
	err = applyMove(ctx, tx, m, `, reason = $6, next_attempt_at = NULL`, why)
	return StateAbandoned, err
}

// RecordFailure is RecordFailureTx in its own transaction.
func (s *Store) RecordFailure(ctx context.Context, c Claim, reason string) (State, error) {
	var state State
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		state, err = RecordFailureTx(ctx, tx, c, reason, nil)
		return err
	})
	return state, err
}

// StartVerifying moves an applied claim on to verification.
func (s *Store) StartVerifying(ctx context.Context, c Claim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		m := move{id: c.ID, from: StateApplied, to: StateVerifying, revision: c.Revision,
			attempt: c.Attempt, actor: ActorExecutor, reason: "verification started"}
		return applyMove(ctx, tx, m, "")
	})
}
