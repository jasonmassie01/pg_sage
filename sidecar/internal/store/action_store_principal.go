package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// countPendingForPrincipalSQL is D9's pending count for one agent; it
// reads action_queue_principal_status.
const countPendingForPrincipalSQL = `/* pg_sage agent_pending v1 */
SELECT count(*) FROM sage.action_queue
WHERE principal_id = $1 AND status = 'pending'
  AND (expires_at IS NULL OR expires_at > now())`

// CountPendingForPrincipal counts the unexpired pending items an agent's
// requests queued (agents.approvals.max_pending_per_principal, D9).
func (s *ActionStore) CountPendingForPrincipal(ctx context.Context,
	principalID string) (int, error) {
	if principalID == "" {
		return 0, errors.New("counting pending actions: a principal is required")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	if err := s.pool.QueryRow(qctx, countPendingForPrincipalSQL, principalID).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("counting pending actions of %s: %w", principalID, err)
	}
	return n, nil
}
