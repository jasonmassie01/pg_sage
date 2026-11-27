package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// refusedApproval explains a queue row Approve could not approve: a row
// superseded by a new recommendation revision needs re-approval of that
// revision (recommendation.ErrRevised).
func refusedApproval(ctx context.Context, tx pgx.Tx, queueID int, cause error) error {
	var status string
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT status FROM sage.action_queue
		WHERE id = $1`, queueID).Scan(&status); err == nil && status == "superseded" {
		return fmt.Errorf("%w: queued proposal %d was superseded", recommendation.ErrRevised,
			queueID)
	}
	return cause
}

// Reject marks an action as rejected with a reason.
func (s *ActionStore) Reject(
	ctx context.Context, queueID, userID int, reason string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		 SET status = 'rejected',
		     decided_by = $1,
		     decided_at = now(),
		     reason = $2
		 WHERE id = $3 AND status = 'pending'`,
		userID, reason, queueID,
	)
	if err != nil {
		return fmt.Errorf("rejecting action %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found or not pending", queueID)
	}
	return nil
}

func (s *ActionStore) MarkAttemptFailed(
	ctx context.Context,
	queueID int,
	failure string,
	cooldown time.Duration,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cooldownUntil := time.Now().UTC().Add(cooldown)
	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		 SET status = 'failed',
		     attempt_count = attempt_count + 1,
		     last_attempt_at = now(),
		     cooldown_until = CASE
		         WHEN $2 THEN $3::timestamptz ELSE NULL::timestamptz END,
		     last_failure_fingerprint = failure_fingerprint,
		     failure_fingerprint = $4,
		     verification_status = 'failed',
		     reason = $5
		 WHERE id = $1`,
		queueID, cooldown > 0, cooldownUntil,
		failureFingerprint(failure), failure,
	)
	if err != nil {
		return fmt.Errorf("marking action %d failed: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found", queueID)
	}
	return nil
}

func (s *ActionStore) MarkExecuted(
	ctx context.Context,
	queueID int,
	actionLogID int64,
	verificationStatus string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if verificationStatus == "" {
		verificationStatus = "verified"
	}
	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		 SET status = 'executed',
		     action_log_id = $2,
		     verification_status = $3,
		     last_attempt_at = now(),
		     cooldown_until = NULL
		 WHERE id = $1`,
		queueID, actionLogID, verificationStatus,
	)
	if err != nil {
		return fmt.Errorf("marking action %d executed: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found", queueID)
	}
	return nil
}

// ExpireStale marks expired pending actions.
func (s *ActionStore) ExpireStale(
	ctx context.Context,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		 SET status = 'expired'
		 WHERE status = 'pending'
		   AND expires_at <= now()`,
	)
	if err != nil {
		return 0, fmt.Errorf("expiring stale actions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkExpiredByReadiness records proposals that are no longer actionable
// because their deterministic lifecycle check has closed the gate.
func (s *ActionStore) MarkExpiredByReadiness(
	ctx context.Context,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		 SET status = 'expired',
		     reason = CASE
		         WHEN expires_at <= now()
		         THEN 'action proposal expired'
		         ELSE reason
		     END
		 WHERE status IN ('pending', 'failed')
		   AND expires_at <= now()`,
	)
	if err != nil {
		return 0, fmt.Errorf("marking readiness-expired actions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *ActionStore) MarkReadinessOutcome(
	ctx context.Context,
	queueID int,
	status string,
	reason string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		    SET status = $2,
		        reason = $3
		  WHERE id = $1
		    AND status <> 'executed'`,
		queueID, status, reason,
	)
	if err != nil {
		return fmt.Errorf(
			"marking readiness outcome for action %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found or already executed", queueID)
	}
	return nil
}
