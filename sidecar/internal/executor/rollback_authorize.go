package executor

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-05: every revert of an action pg_sage ran (approved, manual or a
// resumed monitor's) is authorized by the standing gate as a typed
// rollback request and leaves a decision row; nothing re-checks a second,
// unrecorded rule set.

// authorizeRollback asks the standing gate to authorize rollbackSQL as the
// revert of action actionID. operatorApproved marks a person's revert (the
// rollback button, an approved revert item). The decision is recorded;
// like every rollback, it holds no budget slot.
func (e *Executor) authorizeRollback(
	ctx context.Context, actionID int64, rollbackSQL string, operatorApproved bool,
) ActionPolicyDecision {
	request := e.rollbackRequest(ctx, actionID, rollbackSQL, operatorApproved)
	gate := e.StandingPolicyGate()
	if gate == nil {
		return ActionPolicyDecision{Decision: PolicyDecisionBlocked,
			BlockedReason: reasonNoStandingPolicy}
	}
	decision := standingPolicyDecision(gate.Authorize(ctx, request))
	e.releaseBudget(ctx, decision.DecisionID)
	return decision
}

// explainRollback is authorizeRollback without a recorded decision, for a
// refusal before anything is claimed and for a revert item's readiness.
func (e *Executor) explainRollback(
	ctx context.Context, actionID int64, rollbackSQL string,
) ActionPolicyDecision {
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return ActionPolicyDecision{Decision: PolicyDecisionBlocked,
			BlockedReason: reasonNoStandingPolicy}
	}
	request := e.rollbackRequest(ctx, actionID, rollbackSQL, true)
	return standingPolicyDecision(explainer.Explain(ctx, request))
}

// rollbackRequest is the gate request of a revert: the statement that
// runs (an index rollback in its CONCURRENTLY form, as executeRollbackSQL
// rewrites it), the contract it classifies to (a drop's restore: the
// restore contract) and its statement targets. A rollback no contract
// classifies is sent without one, so the gate records no_typed_contract.
func (e *Executor) rollbackRequest(
	ctx context.Context, actionID int64, rollbackSQL string, operatorApproved bool,
) policy.ActionRequest {
	forward := e.forwardSQL(ctx, actionID)
	rollbackSQL = concurrentIndexRollbackSQL(rollbackSQL)
	request := policy.ActionRequest{SQL: rollbackSQL,
		TargetObjs: operatorLeaseTargets(rollbackSQL)}
	if isDropActionSQL(forward) {
		request = dropRestoreRequest(restoreSQLForDrop(rollbackSQL))
	}
	request.Rollback, request.OperatorApproved = true, operatorApproved
	request.Evidence = map[string]any{"rolled_back_action_id": actionID,
		"source": rollbackSource(operatorApproved)}
	contract, ok := rollbackContractFor(forward, rollbackSQL)
	if !ok {
		request.Contract = nil
		return request
	}
	request.Contract = policyContract(contract)
	request.Feature = changeClassForActionType(contract.ActionType)
	return request
}

func rollbackSource(operatorApproved bool) string {
	if operatorApproved {
		return "operator_rollback"
	}
	return "automatic_rollback"
}

// forwardSQL is the statement action actionID ran ("" when it cannot be
// read: the rollback is then classified by its own SQL).
func (e *Executor) forwardSQL(ctx context.Context, actionID int64) string {
	if e.pool == nil || actionID <= 0 {
		return ""
	}
	var sql string
	if err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT sql_executed
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&sql); err != nil {
		e.logFn("rollback", "read the statement of action %d: %v", actionID, err)
		return ""
	}
	return sql
}

// rollbackDecider is the monitor's authorizer for the automatic revert of
// action actionID.
func (e *Executor) rollbackDecider(
	actionID int64,
) func(context.Context, string) RollbackVerdict {
	return func(ctx context.Context, rollbackSQL string) RollbackVerdict {
		return rollbackVerdictFor(e.authorizeRollback(ctx, actionID, rollbackSQL, false))
	}
}

// rollbackVerdictFor turns a gate decision into the monitor's verdict.
func rollbackVerdictFor(decision ActionPolicyDecision) RollbackVerdict {
	if decision.Decision == PolicyDecisionExecute {
		return RollbackVerdict{Allowed: true, LockCeilingMS: decision.LockCeilingMS,
			DecisionID: decision.DecisionID}
	}
	if decision.BlockedReason == string(policy.ReasonNoTypedContract) {
		return RollbackVerdict{DecisionID: decision.DecisionID, Reason: rollbackNoContract +
			": no action contract classifies the rollback SQL; automatic rollback " +
			"withheld (the operator rollback remains)"}
	}
	return RollbackVerdict{DecisionID: decision.DecisionID, Reason: fmt.Sprintf(
		"%s (%s: %s)", standingWithheld, decision.Decision, decision.BlockedReason)}
}

// actionRollbackConfig is the monitor configuration of an action an
// operator approved or ran (and of a resumed monitor): a drop is restored
// under the restore contract, any other revert through authorizeRollback.
func (e *Executor) actionRollbackConfig(actionID int64, sql string) RollbackMonitorConfig {
	cfg := e.rollbackMonitorConfig(nil)
	if isDropActionSQL(sql) {
		e.withDropRestore(&cfg, actionID)
		return cfg
	}
	cfg.Decide = e.rollbackDecider(actionID)
	return cfg
}

// operatorRollbackRefusal is the error of a person's revert the gate
// refused.
func operatorRollbackRefusal(decision ActionPolicyDecision) error {
	return fmt.Errorf("policy refused rollback: %s", humanPolicyReason(decision))
}

// rollbackLockTimeoutMS bounds a revert's lock wait: the policy's lock
// ceiling caps an in-transaction rollback, a CONCURRENTLY one keeps
// safety.lock_timeout_ms (as forward DDL does).
func rollbackLockTimeoutMS(rollbackSQL string, safetyMS int, ceilingMS int64) int {
	return ddlLockTimeoutMS(ActionTypeForSQL(concurrentIndexRollbackSQL(rollbackSQL)),
		safetyMS, ceilingMS)
}

// stampRollbackEvidence marks a revert's decision (evidence.rollback, for
// the 24-hour audit) and names a standing policy it could not read.
// Request evidence cannot set these keys.
func stampRollbackEvidence(
	evidence map[string]any, request policy.ActionRequest, decision policy.Decision,
) {
	delete(evidence, "rollback")
	delete(evidence, "policy_read_error")
	if !request.Rollback {
		return
	}
	evidence["rollback"] = true
	if decision.PolicyReadError != "" {
		evidence["policy_read_error"] = decision.PolicyReadError
	}
}
