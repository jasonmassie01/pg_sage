package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// reasonNoStandingPolicy is the fail-closed reason when no gate is attached.
const reasonNoStandingPolicy = "standing policy unavailable"

func noStandingPolicyDecision(contract ActionContract) ActionPolicyDecision {
	return ActionPolicyDecision{
		Decision: PolicyDecisionBlocked, RiskTier: contract.BaseRiskTier,
		BlockedReason: reasonNoStandingPolicy,
		Guardrails:    append([]string(nil), contract.Guardrails...),
	}
}

// ExplainAction reports the standing gate's verdict for sql without
// recording a decision. The contract's descriptive guardrails are kept for
// display. No gate, or a gate that cannot explain, fails closed.
func (e *Executor) ExplainAction(
	ctx context.Context, contract ActionContract, sql, target string,
) ActionPolicyDecision {
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return noStandingPolicyDecision(contract)
	}
	request := policy.ActionRequest{
		SQL: sql, Feature: changeClassForActionType(contract.ActionType),
		Contract: policyContract(contract),
	}
	if target != "" {
		request.TargetObjs = []string{target}
	}
	decision := standingPolicyDecision(explainer.Explain(ctx, request))
	decision.Guardrails = append([]string(nil), contract.Guardrails...)
	return decision
}
