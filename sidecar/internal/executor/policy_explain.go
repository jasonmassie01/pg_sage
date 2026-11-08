package executor

import (
	"context"
	"strings"

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

// cancelledExplainDecision fails a family closed when the caller is gone,
// so a closed dashboard request reads nothing more from the database.
func cancelledExplainDecision(contract ActionContract, err error) ActionPolicyDecision {
	decision := noStandingPolicyDecision(contract)
	decision.BlockedReason = string(policy.ReasonPolicyUnavailable)
	decision.Detail = "policy explanation stopped: " + err.Error()
	return decision
}

// ExplainFamilies reports the standing gate's verdict for each action
// family (no concrete SQL) against one policy snapshot, recording nothing.
// A nil executor, no gate, or a gate that cannot batch-explain fails closed;
// so does a cancelled or expired ctx (policy_unavailable, nothing read).
func (e *Executor) ExplainFamilies(
	ctx context.Context, contracts []ActionContract, isReplica bool,
) []ActionPolicyDecision {
	out := make([]ActionPolicyDecision, len(contracts))
	if err := ctx.Err(); err != nil {
		for i, contract := range contracts {
			out[i] = cancelledExplainDecision(contract, err)
		}
		return out
	}
	var batch policy.BatchExplainer
	ok := false
	if e != nil {
		batch, ok = e.StandingPolicyGate().(policy.BatchExplainer)
	}
	if !ok {
		for i, contract := range contracts {
			out[i] = noStandingPolicyDecision(contract)
		}
		return out
	}
	requests := make([]policy.ActionRequest, len(contracts))
	for i, contract := range contracts {
		requests[i] = policy.ActionRequest{
			Contract: policyContract(contract), ExplainFamily: true,
			Feature: changeClassForActionType(contract.ActionType), IsReplica: isReplica,
		}
	}
	provider := e.policyProvider()
	for i, decision := range batch.ExplainBatch(ctx, requests) {
		out[i] = standingPolicyDecision(decision)
		out[i].Guardrails = append([]string(nil), contracts[i].Guardrails...)
		out[i].Provider = provider
	}
	return out
}

// policyProvider is the provider the gate evaluates against: the configured
// cloud environment, with self-managed shown as postgres.
func (e *Executor) policyProvider() string {
	cfg, _, _ := e.policySnapshot()
	if cfg == nil {
		return "postgres"
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.CloudEnvironment))
	if provider == "" || provider == "self-managed" {
		return "postgres"
	}
	return provider
}
