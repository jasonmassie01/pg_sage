package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Approval cards (roadmap 1.5): an approval request names its queue item,
// so the notifier can build its card and chat can decide it, and the gate
// decision that queued the item is linked to it, so the card can say why.

// requestApproval links the queuing decision to the queue item and sends
// the approval request.
func (e *Executor) requestApproval(ctx context.Context, title, sql, risk string,
	decisionID int64, queueID int) {
	e.linkDecisionToQueue(ctx, decisionID, queueID)
	e.dispatchEvent(ctx, notify.QueuedApprovalEvent(title, sql, e.databaseName, risk,
		queueID))
}

// linkDecisionToQueue records which queue item a decision queued. A
// decision already linked keeps its link; a failure is logged (the card
// then explains the item without the gate's reason).
func (e *Executor) linkDecisionToQueue(ctx context.Context, decisionID int64, queueID int) {
	if e.pool == nil || decisionID <= 0 || queueID <= 0 {
		return
	}
	if _, err := e.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.decision SET queue_id = $2
		WHERE id = $1 AND queue_id IS NULL`, decisionID, queueID); err != nil {
		e.logFn("executor", "link decision %d to queue item %d: %v", decisionID, queueID, err)
	}
}
