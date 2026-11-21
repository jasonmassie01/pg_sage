package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approve implements gitops.ApprovalQueue with approveAndRunExpecting: the
// same readiness gate, single-use SQL-matched approval, executor run and
// outcome record as an approval in the app.
func (q *ApprovalPRQueue) Approve(ctx context.Context, database string, queueID,
	userID int, expectSQL string) (string, error) {
	inst := q.instance(database)
	if inst == nil {
		return "", fmt.Errorf("database %s is not monitored", database)
	}
	if userID <= 0 || expectSQL == "" {
		return "", fmt.Errorf("approving %s/%d by PR: a user and the card's SQL are "+
			"required", database, queueID)
	}
	as := store.NewActionStore(inst.Pool)
	if err := stillPending(ctx, as, queueID); err != nil {
		return "", err
	}
	body, refused := approveAndRunExpecting(ctx, as, inst.Executor, queueID, userID,
		expectSQL)
	if refused != nil {
		if refused.approveErr != nil {
			if err := stillPending(ctx, as, queueID); err != nil {
				return "", err
			}
			return "", fmt.Errorf("the approval was refused: %w", refused.approveErr)
		}
		return "", errors.New(refused.msg)
	}
	return approvalOutcomeText(database, body), nil
}

// stillPending is ErrAlreadyDecided once the item left pending.
func stillPending(ctx context.Context, as *store.ActionStore, queueID int) error {
	a, err := as.GetByID(ctx, queueID)
	if err != nil {
		return fmt.Errorf("reading queue item %d: %w", queueID, err)
	}
	if a.Status != "pending" {
		return fmt.Errorf("%w (%s)", gitops.ErrAlreadyDecided, a.Status)
	}
	return nil
}

// approvalOutcomeText is what an approval did, for the PR comment.
func approvalOutcomeText(database string, body map[string]any) string {
	switch {
	case body["status"] == "scheduled":
		return fmt.Sprintf("scheduled for the maintenance window opening %v",
			body["run_after_label"])
	case body["executed"] != true:
		return fmt.Sprintf("approved, but the change did not run: %v", body["error"])
	}
	return fmt.Sprintf("ran on %s (verification: %v)", database, body["verification_status"])
}

// Reject implements gitops.ApprovalQueue.
func (q *ApprovalPRQueue) Reject(ctx context.Context, database string, queueID,
	userID int, reason string) error {
	inst := q.instance(database)
	if inst == nil {
		return fmt.Errorf("database %s is not monitored", database)
	}
	as := store.NewActionStore(inst.Pool)
	if err := as.Reject(ctx, queueID, userID, reason); err != nil {
		if perr := stillPending(ctx, as, queueID); perr != nil {
			return perr
		}
		return err
	}
	return nil
}
