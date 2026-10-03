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
// it is approved, in the same statement (single use, content match).
func approveAndRunExpecting(ctx context.Context, as *store.ActionStore,
	exec *executor.Executor, queueID, userID int,
	expectSQL string) (map[string]any, *approvalRefusal) {
	action, err := as.GetByID(ctx, queueID)
	if err != nil {
		slog.Error("approve action failed", "action_id", queueID, "error", err)
		return nil, &approvalRefusal{status: http.StatusNotFound,
			msg: "failed to approve action"}
	}
	if reason, ok := approvalBlocked(ctx, as, exec, *action); !ok {
		return nil, &approvalRefusal{status: http.StatusConflict,
			msg: "action is not eligible: " + reason}
	}
	if expectSQL != "" {
		action, err = as.ApproveExpecting(ctx, queueID, userID, expectSQL)
	} else {
		action, err = as.Approve(ctx, queueID, userID)
	}
	if err != nil {
		return nil, &approvalRefusal{approveErr: err}
	}
	run, execErr := exec.RunApprovedAction(ctx, *action, userID)
	if execErr != nil {
		recordApproveExecutionFailure(ctx, as, queueID, execErr.Error())
		return map[string]any{"ok": false, "queue_id": queueID, "error": execErr.Error(),
			"status": "failed", "executed": false}, nil
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
		"verification_status": verification}, nil
}

// approvalBlocked reports whether an item may be approved now (its
// lifecycle and the policy gate), recording why not on the item.
func approvalBlocked(ctx context.Context, as *store.ActionStore, exec *executor.Executor,
	action store.QueuedAction) (string, bool) {
	if exec == nil {
		return "", true
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
	if readiness.Eligible {
		return "", true
	}
	reason := strings.TrimSpace(readiness.DeferReason)
	if reason == "" {
		reason = "action is not eligible for approval"
	}
	if as != nil && action.Status == "pending" {
		_ = as.MarkReadinessOutcome(ctx, action.ID, readinessOutcomeStatus(readiness),
			reason)
	}
	return reason, false
}
