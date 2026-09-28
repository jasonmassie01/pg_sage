package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

// authorizeOperatorAction asks the standing gate to authorize an operator
// run action (manual "take action" or an approved queue item) and records
// the decision before the action executes, so it is auditable and can be
// verified and credited like autonomous work. The approval replaces tier,
// ramp and execution mode; hard stops, provider support, trust, the
// standing policy's change classes and its windows still bind.
func (e *Executor) authorizeOperatorAction(
	ctx context.Context, sql string, findingID int, approvedBy *int,
) (policy.Decision, error) {
	gate := e.StandingPolicyGate()
	if gate == nil {
		return policy.Decision{}, fmt.Errorf("%s", reasonNoStandingPolicy)
	}
	request, _ := operatorRequest(sql, findingID, approvedBy)
	decision := gate.Authorize(ctx, request)
	if decision.Verdict != policy.VerdictExecute {
		return policy.Decision{}, fmt.Errorf("policy refused operator action: %s",
			operatorRefusal(decision))
	}
	return decision, nil
}

// explainOperatorAction reports whether an operator approval of sql would
// be authorized now, without recording a decision.
func (e *Executor) explainOperatorAction(ctx context.Context, sql string) ActionPolicyDecision {
	request, contract := operatorRequest(sql, 0, nil)
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return noStandingPolicyDecision(contract)
	}
	decision := standingPolicyDecision(explainer.Explain(ctx, request))
	decision.Guardrails = append([]string(nil), contract.Guardrails...)
	return decision
}

func operatorRequest(sql string, findingID int, approvedBy *int) (
	policy.ActionRequest, ActionContract,
) {
	actionType := actionTypeForProposalSQL(sql)
	contract, ok := ContractForActionType(actionType)
	if !ok {
		contract = ActionContract{ActionType: actionType, BaseRiskTier: "high"}
	}
	evidence := map[string]any{"finding_id": findingID, "source": "operator"}
	if approvedBy != nil {
		evidence["approved_by"] = *approvedBy
	}
	return policy.ActionRequest{
		SQL: sql, Feature: changeClassForActionType(actionType),
		Contract: policyContract(contract), Evidence: evidence,
		OperatorApproved: true,
	}, contract
}

func operatorRefusal(decision policy.Decision) string {
	reason := humanReason(string(decision.Reason))
	if strings.TrimSpace(decision.Detail) == "" {
		return reason
	}
	return reason + " (" + decision.Detail + ")"
}

// humanPolicyReason renders a policy verdict's reason and detail.
func humanPolicyReason(decision ActionPolicyDecision) string {
	reason := humanReason(decision.BlockedReason)
	if strings.TrimSpace(decision.Detail) == "" {
		return reason
	}
	return reason + " (" + decision.Detail + ")"
}

// humanReason renders a gate reason code for operators
// ("outside_maintenance_window" -> "outside maintenance window").
func humanReason(reason string) string {
	return strings.ReplaceAll(reason, "_", " ")
}

// databaseIDValue is the canonical database identity stamped on every
// action_log row (nil outside meta-db mode, where the pool is the identity).
func (e *Executor) databaseIDValue() *int64 {
	return int64Pointer(e.databaseID)
}
