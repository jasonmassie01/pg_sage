package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// approvalRefusal is why a queue item was not approved: approveErr is
// the store's single-use refusal, otherwise status and msg describe it.
type approvalRefusal struct {
	status     int
	msg        string
	approveErr error
}

func (r *approvalRefusal) write(w http.ResponseWriter, queueID int) {
	if r.approveErr != nil {
		approveFailure(w, queueID, r.approveErr)
		return
	}
	jsonError(w, r.msg, r.status)
}

// approveAndRun approves a ready queue item for a user (single use, the
// user is decided_by) and runs it through the executor: a Sage SRE
// proposal through its action service, every other item through the
// manual path. The browser approval and ChatOps share it.
func approveAndRun(ctx context.Context, as *store.ActionStore, exec *executor.Executor,
	queueID, userID int) (map[string]any, *approvalRefusal) {
	return approveAndRunExpecting(ctx, as, exec, queueID, userID, "")
}

// approveAndRunExpecting is approveAndRun bound to the SQL an approval card
// showed: a non-empty expectSQL must still be the item's proposed SQL when
// it is approved, in the same statement (single use, content match). An
// approval the maintenance window alone withholds is scheduled for the
// next window instead (policy-04).
func approveAndRunExpecting(ctx context.Context, as *store.ActionStore,
	exec *executor.Executor, queueID, userID int,
	expectSQL string) (map[string]any, *approvalRefusal) {
	action, err := as.GetByID(ctx, queueID)
	if err != nil {
		slog.Error("approve action failed", "action_id", queueID, "error", err)
		return nil, &approvalRefusal{status: http.StatusNotFound,
			msg: "failed to approve action"}
	}
	if readiness, ok := approvalReadiness(ctx, as, exec, *action); !ok {
		body, refused, handled := scheduleWindowOnly(ctx, as, exec, *action, readiness,
			userID, expectSQL)
		if handled {
			return body, refused
		}
		return nil, &approvalRefusal{status: http.StatusConflict,
			msg: "action is not eligible: " + markApprovalBlocked(ctx, as, *action, readiness)}
	}
	if expectSQL != "" {
		action, err = as.ApproveExpecting(ctx, queueID, userID, expectSQL)
	} else {
		action, err = as.Approve(ctx, queueID, userID)
	}
	if err != nil {
		return nil, &approvalRefusal{approveErr: err}
	}
	return runApproved(ctx, as, exec, *action, userID), nil
}

// runApproved runs an approved item and records the outcome on it.
func runApproved(ctx context.Context, as *store.ActionStore, exec *executor.Executor,
	action store.QueuedAction, userID int) map[string]any {
	queueID := action.ID
	run, execErr := exec.RunApprovedAction(ctx, action, userID)
	if execErr != nil {
		recordApproveExecutionFailure(ctx, as, queueID, execErr.Error())
		return map[string]any{"ok": false, "queue_id": queueID, "error": execErr.Error(),
			"status": "failed", "executed": false}
	}
	verification := run.VerificationStatus
	if verification == "" {
		verification = "verified"
		if action.RollbackSQL != "" {
			verification = "monitoring"
		}
	}
	recordApproveExecutionSuccess(ctx, as, queueID, run.ActionLogID, verification)
	return map[string]any{"ok": true, "queue_id": queueID,
		"action_log_id": run.ActionLogID, "status": "approved", "executed": true,
		"verification_status": verification}
}

// approvalReadiness evaluates whether an item may be approved now (its
// lifecycle and the policy gate). Without an executor it may.
func approvalReadiness(ctx context.Context, as *store.ActionStore, exec *executor.Executor,
	action store.QueuedAction) (executor.ApprovalReadiness, bool) {
	if exec == nil {
		return executor.ApprovalReadiness{Eligible: true}, true
	}
	evidencePresent := true
	if as != nil {
		present, err := as.FindingEvidencePresent(ctx, action.FindingID)
		if err == nil {
			evidencePresent = present
		}
	}
	readiness := exec.ApprovalReadinessWithEvidence(action, time.Now().UTC(),
		evidencePresent)
	return readiness, readiness.Eligible
}

// markApprovalBlocked records why an item may not be approved now on the
// item and returns the reason.
func markApprovalBlocked(ctx context.Context, as *store.ActionStore,
	action store.QueuedAction, readiness executor.ApprovalReadiness) string {
	reason := strings.TrimSpace(readiness.DeferReason)
	if reason == "" {
		reason = "action is not eligible for approval"
	}
	status := readinessOutcomeStatus(readiness)
	// A blocked restore or revert card stays pending: nothing would queue it
	// again, and it becomes approvable once the stop is lifted (executor-01,
	// policy-05).
	keep := status == "blocked" &&
		(executor.IsDropRestoreCard(action) || executor.IsRevertCard(action))
	if as != nil && action.Status == "pending" && !keep {
		_ = as.MarkReadinessOutcome(ctx, action.ID, status, reason)
	}
	return reason
}
