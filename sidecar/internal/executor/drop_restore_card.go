package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// An emergency stop halts everything, the soft-drop restore included, but
// never silently: the regressed drop is recorded as rollback_withheld and
// one approval card is queued. The card becomes approvable once the stop
// is lifted; approving it runs the action's recorded restore.

const (
	outcomeRollbackWithheld = "rollback_withheld"
	restoreIdentityPrefix   = ActionTypeRestoreDroppedIndex + ":action:"
	dropRestoreStopReason   = "emergency_stop: the restore of this regressed drop was " +
		"withheld; approve its queued restore after the stop is lifted"
)

// restoreIdentityKey is the queue identity of action actionID's restore.
func restoreIdentityKey(actionID int64) string {
	return restoreIdentityPrefix + strconv.FormatInt(actionID, 10)
}

// restoreQueueActionID names the drop a restore card restores.
func restoreQueueActionID(action store.QueuedAction) (int64, bool) {
	if action.ActionType != ActionTypeRestoreDroppedIndex ||
		!strings.HasPrefix(action.IdentityKey, restoreIdentityPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(action.IdentityKey,
		restoreIdentityPrefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// IsDropRestoreCard reports a soft-drop restore card. Nothing re-proposes
// one, so a refused approval (during the stop) must leave it pending: it
// becomes approvable once the stop is lifted.
func IsDropRestoreCard(action store.QueuedAction) bool {
	_, ok := restoreQueueActionID(action)
	return ok
}

// holdDropRestore records a restore the emergency stop withholds. The
// compare-and-set claims the action, so concurrent monitors queue one card.
func (e *Executor) holdDropRestore(ctx context.Context, actionID int64, restoreSQL string) {
	rec, err := e.loadDropRecord(ctx, actionID)
	if err == nil {
		err = matchDropRestore(restoreSQL, rec.rollbackSQL, rec.sql)
	}
	if err != nil {
		e.logFn("rollback", "action %d: restore refused: %v", actionID, err)
		setMonitoredOutcome(ctx, e.pool, actionID, "rollback_skipped", err.Error())
		return
	}
	if !setMonitoredOutcome(ctx, e.pool, actionID, outcomeRollbackWithheld,
		dropRestoreStopReason) {
		e.logFn("rollback", "action %d changed state; restore not withheld again", actionID)
		return
	}
	e.logFn("rollback", "emergency stop active — restore of action %d withheld as an "+
		"approval card", actionID)
	queueID, err := e.queueDropRestore(ctx, actionID, rec.findingID, restoreSQL)
	if err != nil {
		e.logFn("ERROR", "action %d: the emergency stop withheld its restore and the "+
			"approval card could not be queued (%v); roll the action back by hand once "+
			"the stop is lifted", actionID, err)
		return
	}
	e.requestApproval(ctx, fmt.Sprintf("Restore the index dropped by action %d", actionID),
		restoreSQL, "safe", 0, queueID)
}

func (e *Executor) queueDropRestore(
	ctx context.Context, actionID, findingID int64, restoreSQL string,
) (int, error) {
	proposer, ok := e.actionStore.(ActionMetadataProposer)
	if !ok {
		return 0, fmt.Errorf("the action store cannot record a restore card")
	}
	meta := store.ActionProposalMetadata{
		ActionType: ActionTypeRestoreDroppedIndex, IdentityKey: restoreIdentityKey(actionID),
		PolicyDecision: PolicyDecisionQueueApproval, VerificationStatus: "not_started",
		ShadowToilMinutes: estimatedToilForActionType(ActionTypeRestoreDroppedIndex),
	}
	return proposer.ProposeWithMetadata(ctx, nil, int(findingID), restoreSQL, "", "safe",
		meta)
}

// restoreWithheld reports whether action actionID's restore is still
// withheld (the card's evidence: the drop's finding is gone by design).
func (e *Executor) restoreWithheld(ctx context.Context, actionID int64) bool {
	rec, err := e.loadDropRecord(ctx, actionID)
	return err == nil && rec.outcome == outcomeRollbackWithheld
}

// dropRestoreReadiness is a restore card's readiness: approvable while the
// restore is withheld and the gate would run it for an operator now.
func (e *Executor) dropRestoreReadiness(
	action store.QueuedAction, now time.Time, actionID int64,
) ApprovalReadiness {
	ctx := context.Background()
	readiness := lifecycleReadiness(action, now, e.restoreWithheld(ctx, actionID))
	if !readiness.Eligible {
		return readiness
	}
	return withPolicyReadiness(readiness, e.explainDropRestoreApproval(ctx, action.ProposedSQL))
}

// explainDropRestoreApproval explains an operator approval of a restore.
func (e *Executor) explainDropRestoreApproval(
	ctx context.Context, sql string,
) ActionPolicyDecision {
	contract := restoreDroppedIndexContract()
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return noStandingPolicyDecision(contract)
	}
	request := dropRestoreRequest(sql)
	request.OperatorApproved = true
	return standingPolicyDecision(explainer.Explain(ctx, request))
}

// runApprovedDropRestore runs an approved restore card: the action's
// recorded restore, through the operator rollback (which claims the
// action, so it runs once).
func (e *Executor) runApprovedDropRestore(
	ctx context.Context, action store.QueuedAction, actionID int64,
) (ApprovedRun, error) {
	rec, err := e.loadDropRecord(ctx, actionID)
	if err != nil {
		return ApprovedRun{}, err
	}
	if rec.outcome != outcomeRollbackWithheld {
		return ApprovedRun{}, fmt.Errorf("action %d is %s; its restore is no longer withheld",
			actionID, rec.outcome)
	}
	if err := matchDropRestore(action.ProposedSQL, rec.rollbackSQL, rec.sql); err != nil {
		return ApprovedRun{}, err
	}
	reason := fmt.Sprintf("restore approved after the emergency stop (queue item %d)",
		action.ID)
	if err := e.RollbackAction(ctx, actionID, reason); err != nil {
		return ApprovedRun{}, err
	}
	return ApprovedRun{ActionLogID: actionID, VerificationStatus: "verified"}, nil
}

// supersedeSettledRestoresSQL closes restore cards whose action is no
// longer withheld (rolled back by hand, or restored outside pg_sage).
const supersedeSettledRestoresSQL = `/* pg_sage executor */ UPDATE sage.action_queue q
	SET status = 'superseded',
	    reason = 'superseded: action ' || al.id || ' is ' || al.outcome ||
	             '; its restore is no longer withheld'
	FROM sage.action_log al
	WHERE q.status = 'pending' AND q.action_type = '` + ActionTypeRestoreDroppedIndex + `'
	  AND q.identity_key = '` + restoreIdentityPrefix + `' || al.id
	  AND al.outcome <> '` + outcomeRollbackWithheld + `'
	RETURNING q.id`
