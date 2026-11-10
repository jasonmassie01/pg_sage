package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
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
// It fails closed as ExplainActions does.
func (e *Executor) ExplainFamilies(
	ctx context.Context, contracts []ActionContract, isReplica bool,
) []ActionPolicyDecision {
	requests := make([]ExplainActionRequest, len(contracts))
	for i, contract := range contracts {
		requests[i].Contract = contract
	}
	return e.ExplainActions(ctx, requests, isReplica)
}

// ExplainActionRequest is one action to explain under Contract. A request
// whose Finding has a RecommendedSQL is explained as the executor would
// authorize that finding's statement (its targets, bindings and approval
// rules); one without SQL is the contract's family readiness.
type ExplainActionRequest struct {
	Contract ActionContract
	Finding  analyzer.Finding
}

// ExplainActions reports the standing gate's verdict for each request
// against one policy snapshot (one ExplainBatch), recording nothing. A nil
// executor, no gate, or a gate that cannot batch-explain fails closed; so
// does a cancelled or expired ctx (policy_unavailable, nothing read).
func (e *Executor) ExplainActions(
	ctx context.Context, requests []ExplainActionRequest, isReplica bool,
) []ActionPolicyDecision {
	out := make([]ActionPolicyDecision, len(requests))
	if err := ctx.Err(); err != nil {
		for i, request := range requests {
			out[i] = cancelledExplainDecision(request.Contract, err)
		}
		return out
	}
	var batch policy.BatchExplainer
	ok := false
	if e != nil {
		batch, ok = e.StandingPolicyGate().(policy.BatchExplainer)
	}
	if !ok {
		for i, request := range requests {
			out[i] = noStandingPolicyDecision(request.Contract)
		}
		return out
	}
	gateRequests := make([]policy.ActionRequest, len(requests))
	for i, request := range requests {
		gateRequests[i] = explainGateRequest(request, isReplica)
	}
	provider := e.policyProvider()
	for i, decision := range batch.ExplainBatch(ctx, gateRequests) {
		out[i] = standingPolicyDecision(decision)
		out[i].Guardrails = append([]string(nil), requests[i].Contract.Guardrails...)
		out[i].Provider = provider
	}
	return out
}

// explainGateRequest builds the gate request for one explanation: the
// family request without SQL, or the finding's request under the given
// contract, its statement bound to the finding's object (executor-08).
func explainGateRequest(request ExplainActionRequest, isReplica bool) policy.ActionRequest {
	contract := policyContract(request.Contract)
	if request.Finding.RecommendedSQL == "" {
		return policy.ActionRequest{
			Contract: contract, ExplainFamily: true, IsReplica: isReplica,
			Feature: changeClassForActionType(request.Contract.ActionType),
		}
	}
	gateRequest := contractFindingRequest(request.Finding, contract, isReplica)
	bindFindingRequest(&gateRequest, request.Finding)
	return gateRequest
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
