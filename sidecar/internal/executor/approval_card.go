package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/ledger"
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

// linkDecisionToQueue records which queue item a decision queued and
// resolves the decision, so the next queue item of the same verdict gets
// its own row and its card its own reason (policy-02). A decision already
// linked keeps its link; a failure is logged (the card then explains the
// item without the gate's reason).
func (e *Executor) linkDecisionToQueue(ctx context.Context, decisionID int64, queueID int) {
	if e.pool == nil {
		return
	}
	if _, err := ledger.NewPostgresRepository(e.pool).LinkQueue(ctx, decisionID,
		queueID); err != nil {
		e.logFn("executor", "%v", err)
	}
}
