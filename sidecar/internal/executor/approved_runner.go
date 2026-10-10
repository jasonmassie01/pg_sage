package executor

import (
	"context"
	"sync"

	"github.com/pg-sage/sidecar/internal/store"
)

// ApprovedRun is the outcome of running one approved queue item.
type ApprovedRun struct {
	ActionLogID int64
	// VerificationStatus is the queue item's verification state after the
	// run ("" lets the caller decide).
	VerificationStatus string
}

// ApprovedActionRunner runs approved queue items it owns (a Sage SRE
// proposal's approval item) instead of the manual SQL path.
type ApprovedActionRunner interface {
	Owns(action store.QueuedAction) bool
	RunApproved(ctx context.Context, action store.QueuedAction,
		approvedBy int) (ApprovedRun, error)
}

// approvedRunnerSlot guards the registered runner.
type approvedRunnerSlot struct {
	mu     sync.RWMutex
	runner ApprovedActionRunner
}

// SetApprovedActionRunner registers the runner for approved items it owns;
// nil removes it.
func (e *Executor) SetApprovedActionRunner(r ApprovedActionRunner) {
	slot := &e.approvedRunner
	slot.mu.Lock()
	defer slot.mu.Unlock()
	slot.runner = r
}

// RunApprovedAction runs an approved queue item for its approver: items a
// registered runner owns go to it, every other item keeps the manual path
// (ExecuteManual against its finding).
func (e *Executor) RunApprovedAction(ctx context.Context, action store.QueuedAction,
	approvedBy int) (ApprovedRun, error) {
	if approvedBy <= 0 {
		return ApprovedRun{}, ErrBackendApprovalRequired
	}
	ctx = withQueuedPrincipal(ctx, action)
	slot := &e.approvedRunner
	slot.mu.RLock()
	runner := slot.runner
	slot.mu.RUnlock()
	if runner != nil && runner.Owns(action) {
		return runner.RunApproved(ctx, action, approvedBy)
	}
	ctx = withProposalOrigin(ctx, action)
	id, err := e.ExecuteManual(ctx, action.FindingID, action.ProposedSQL,
		action.RollbackSQL, &approvedBy)
	return ApprovedRun{ActionLogID: id}, err
}
