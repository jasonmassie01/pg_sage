package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotPending means a queue item is not pending (decided, expired or
// missing), so it cannot be snoozed.
var ErrNotPending = errors.New("action is not pending")

// ApproveExpecting approves a pending queue item only if its proposed SQL
// is exactly proposedSQL: an approval card approves the SQL it showed,
// never SQL that changed after it was sent.
func (s *ActionStore) ApproveExpecting(
	ctx context.Context, queueID, userID int, proposedSQL string,
) (*QueuedAction, error) {
	if strings.TrimSpace(proposedSQL) == "" {
		return nil, fmt.Errorf("approving action %d: no expected SQL", queueID)
	}
	return s.approve(ctx, queueID, userID, &proposedSQL)
}

// Snooze defers a pending, unexpired queue item until until: it stays
// pending and approvable, and pg_sage asks again when the snooze ends.
func (s *ActionStore) Snooze(
	ctx context.Context, queueID, userID int, until time.Time, reason string,
) error {
	if userID <= 0 {
		return fmt.Errorf("snoozing action %d: a user is required", queueID)
	}
	if !until.After(time.Now()) {
		return fmt.Errorf("snoozing action %d: %v is not in the future", queueID, until)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.action_queue
		SET snoozed_until = $2, snoozed_by = $3, snooze_reason = $4
		WHERE id = $1 AND status = 'pending' AND expires_at > now()`,
		queueID, until, userID, strings.TrimSpace(reason))
	if err != nil {
		return fmt.Errorf("snoozing action %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snoozing action %d: %w", queueID, ErrNotPending)
	}
	return nil
}
