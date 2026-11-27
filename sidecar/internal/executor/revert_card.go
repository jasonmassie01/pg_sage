package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/store"
)

// A regression the policy (or a hard stop) would not revert must not stay
// in place unseen (G4-I12, policy-05). The withheld revert is recorded as
// rollback_skipped and one "Revert action N" item is queued; approving it
// runs the action's recorded rollback through the operator rollback.

// ActionTypeRevertAction is the queue action type of a revert item.
const ActionTypeRevertAction = "revert_action"

const revertIdentityPrefix = ActionTypeRevertAction + ":action:"

// RevertsAction names the action a revert item reverts.
func RevertsAction(action store.QueuedAction) (int64, bool) {
	if action.ActionType != ActionTypeRevertAction || action.RevertsActionID == nil ||
		*action.RevertsActionID <= 0 {
		return 0, false
	}
	return *action.RevertsActionID, true
}

// IsRevertCard reports a revert item. Nothing re-proposes one, so a refused
// approval (during a stop) leaves it pending.
func IsRevertCard(action store.QueuedAction) bool {
	return action.ActionType == ActionTypeRevertAction
}

// queueRevert offers a withheld automatic rollback to a person. The caller
// claimed the action (rollback_skipped), so concurrent monitors queue one
// item.
func (e *Executor) queueRevert(
	ctx context.Context, actionID int64, rollbackSQL string, verdict RollbackVerdict,
) {
	proposer, ok := e.actionStore.(ActionMetadataProposer)
	if !ok {
		e.logFn("ERROR", "action %d: its automatic rollback was withheld (%s) and no "+
			"action store can queue the revert; roll it back by hand", actionID,
			verdict.Reason)
		return
	}
	findingID, risk := e.revertedActionContext(ctx, actionID, rollbackSQL)
	meta := store.ActionProposalMetadata{
		ActionType: ActionTypeRevertAction, IdentityKey: revertIdentityKey(actionID),
		PolicyDecision: PolicyDecisionQueueApproval, VerificationStatus: "not_started",
		RevertsActionID: actionID,
	}
	queueID, err := proposer.ProposeWithMetadata(ctx, e.databaseID, findingID, rollbackSQL,
		"", risk, meta)
	if err != nil {
		e.logFn("ERROR", "action %d: its automatic rollback was withheld (%s) and the "+
			"revert item could not be queued (%v); roll it back by hand", actionID,
			verdict.Reason, err)
		return
	}
	e.requestApproval(ctx, fmt.Sprintf("Revert action %d", actionID), rollbackSQL, risk,
		verdict.DecisionID, queueID)
}

func revertIdentityKey(actionID int64) string {
	return fmt.Sprintf("%s%d", revertIdentityPrefix, actionID)
}

// revertedActionContext is the finding of the reverted action (0 when it
// has none) and the revert's risk tier, from its rollback contract.
func (e *Executor) revertedActionContext(
	ctx context.Context, actionID int64, rollbackSQL string,
) (int, string) {
	risk := "high"
	if contract, ok := rollbackContractFor(e.forwardSQL(ctx, actionID), rollbackSQL); ok {
		risk = contract.BaseRiskTier
	}
	var findingID int
	if e.pool != nil {
		_ = e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT COALESCE(finding_id, 0)::int
			FROM sage.action_log WHERE id = $1`, actionID).Scan(&findingID)
	}
	return findingID, risk
}

// revertState is the reverted action's outcome and recorded reason.
func (e *Executor) revertState(ctx context.Context, actionID int64) (string, string, error) {
	if e.pool == nil {
		return "", "", errors.New("no database to read the reverted action from")
	}
	var outcome, reason string
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */
SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&outcome, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return "", "", fmt.Errorf("read action %d: %w", actionID, err)
	}
	return outcome, reason, nil
}

// revertReadiness is a revert item's readiness: approvable while the
// action's rollback is still withheld and the gate would run a person's
// revert now.
func (e *Executor) revertReadiness(
	action store.QueuedAction, now time.Time,
) ApprovalReadiness {
	ctx := context.Background()
	actionID, ok := RevertsAction(action)
	if !ok {
		return ApprovalReadiness{LifecycleNow: now,
			DeferReason: "the action this revert item reverts is gone"}
	}
	outcome, _, err := e.revertState(ctx, actionID)
	readiness := lifecycleReadiness(action, now, err == nil && revertWithheld(outcome))
	if !readiness.Eligible {
		return readiness
	}
	return withPolicyReadiness(readiness, e.explainRollback(ctx, actionID,
		action.ProposedSQL))
}

// runApprovedRevert runs an approved revert item: the action's recorded
// rollback through the operator rollback, which claims the action (so it
// runs once) and records the operator-approved rollback decision.
func (e *Executor) runApprovedRevert(
	ctx context.Context, action store.QueuedAction,
) (ApprovedRun, error) {
	actionID, ok := RevertsAction(action)
	if !ok {
		return ApprovedRun{}, errors.New("the action this revert item reverts is gone")
	}
	outcome, _, err := e.revertState(ctx, actionID)
	if err != nil {
		return ApprovedRun{}, err
	}
	if !revertWithheld(outcome) {
		return ApprovedRun{}, fmt.Errorf("action %d is %s; its revert is no longer withheld",
			actionID, outcome)
	}
	stored, err := e.loadRollbackSQL(ctx, actionID)
	if err == nil {
		stored, err = scopeHintRollback(ctx, e.pool, actionID, stored)
	}
	if err != nil {
		return ApprovedRun{}, err
	}
	if strings.TrimSpace(stored) != strings.TrimSpace(action.ProposedSQL) {
		return ApprovedRun{}, fmt.Errorf("%s: the revert item differs from the rollback "+
			"recorded with action %d", rollbackSQLMismatch, actionID)
	}
	reason := fmt.Sprintf("revert approved (queue item %d)", action.ID)
	if err := e.RollbackAction(ctx, actionID, reason); err != nil {
		return ApprovedRun{}, err
	}
	return ApprovedRun{ActionLogID: actionID, VerificationStatus: "verified"}, nil
}

// supersedeSettledRevertsSQL closes revert items whose action is no longer
// withheld (rolled back by hand, or settled another way).
const supersedeSettledRevertsSQL = `/* pg_sage executor */ UPDATE sage.action_queue q
	SET status = 'superseded',
	    reason = 'superseded: action ' || al.id || ' is ' || al.outcome ||
	             '; its revert is no longer withheld'
	FROM sage.action_log al
	WHERE q.status = 'pending' AND q.action_type = '` + ActionTypeRevertAction + `'
	  AND q.reverts_action_id = al.id
	  AND al.outcome NOT IN ('rollback_skipped', 'verification_expired')
	RETURNING q.id`

// revertWithheld reports an action whose revert item is still open: its
// automatic rollback was withheld, or its verification expired unjudged
// and verify.on_expired asked a person (lab-01c).
func revertWithheld(outcome string) bool {
	return outcome == "rollback_skipped" || outcome == "verification_expired"
}
