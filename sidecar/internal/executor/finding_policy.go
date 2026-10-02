package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// evaluateFindingPolicy is the single background-action authorization path.
// It reloads runtime mode, trust, enabled state, and emergency stop for every
// candidate so a safety downgrade takes effect before the next action.
func (e *Executor) evaluateFindingPolicy(
	ctx context.Context,
	f analyzer.Finding,
	isReplica bool,
) ActionPolicyDecision {
	e.policyMu.RLock()
	gate := e.policyGate
	e.policyMu.RUnlock()
	if gate == nil {
		// The standing gate is the only authority (G4-I01); production
		// installs it before anything runs, so its absence fails closed.
		contract, _ := contractForFinding(f)
		return noStandingPolicyDecision(contract)
	}
	return e.evaluateStandingPolicy(ctx, gate, f, isReplica)
}

func (e *Executor) evaluateStandingPolicy(
	ctx context.Context, gate policy.Gate, finding analyzer.Finding, isReplica bool,
) ActionPolicyDecision {
	return standingPolicyDecision(gate.Authorize(ctx, findingRequest(finding, isReplica)))
}

// findingRequest is the standing-gate request for a background finding.
func findingRequest(finding analyzer.Finding, isReplica bool) policy.ActionRequest {
	request := policy.ActionRequest{
		SQL: finding.RecommendedSQL, Feature: featureForFinding(finding),
		TargetObjs: targetObjectsForFinding(finding),
		IsReplica:  isReplica,
	}
	if contract, ok := contractForFinding(finding); ok {
		request.Contract = policyContract(contract)
		requireApprovalWithoutWhatIf(finding, request.Contract)
	}
	return request
}
func policyContract(contract ActionContract) *policy.ActionContract {
	result := &policy.ActionContract{
		ActionType: contract.ActionType, RiskTier: policy.RiskTier(contract.BaseRiskTier),
		ProviderSupport: append([]string(nil), contract.ProviderSupport...),
		RollbackClass:   policy.RollbackClass(contract.RollbackClass),
		DropKind:        dropKindForActionType(contract.ActionType),
	}
	for _, guardrail := range contract.Guardrails {
		if isApprovalRequiredGuardrail(guardrail) {
			result.Guardrails = []policy.Guardrail{policy.GuardrailApprovalRequired}
		}
	}
	return result
}

func targetObjectsForFinding(finding analyzer.Finding) []string {
	target := strings.TrimSpace(finding.ObjectIdentifier)
	if target == "" {
		return nil
	}
	return []string{target}
}

func featureForFinding(finding analyzer.Finding) string {
	return changeClassForActionType(actionTypeForProposalSQL(finding.RecommendedSQL))
}

func standingPolicyDecision(decision policy.Decision) ActionPolicyDecision {
	result := ActionPolicyDecision{
		RiskTier: string(decision.RiskTier), BlockedReason: string(decision.Reason),
		Detail:           decision.Detail,
		RequiresApproval: decision.Verdict == policy.VerdictQueueApproval,
		RequiresMaintenanceWindow: decision.RiskTier == policy.RiskModerate ||
			decision.RiskTier == policy.RiskHigh,
		EvidenceID: decision.EvidenceID, DecisionID: decision.DecisionID,
		LockCeilingMS: decision.LockCeilingMS, SerializeMode: decision.SerializeMode,
	}
	switch decision.Verdict {
	case policy.VerdictExecute:
		result.Decision = PolicyDecisionExecute
	case policy.VerdictQueueApproval:
		result.Decision = PolicyDecisionQueueApproval
	case policy.VerdictPark:
		result.Decision = PolicyDecisionParked
	case policy.VerdictObserveOnly:
		result.Decision = PolicyDecisionObserveOnly
	default:
		result.Decision = PolicyDecisionBlocked
	}
	for _, guardrail := range decision.Guardrails {
		result.Guardrails = append(result.Guardrails, string(guardrail))
	}
	// A plain authorization is not a blocked reason; informative execute
	// reasons such as deadline_override are kept.
	if decision.Reason == policy.ReasonAuthorized ||
		decision.Reason == policy.ReasonOperatorApproved {
		result.BlockedReason = ""
	}
	return result
}

func (e *Executor) checkEmergencyStop(ctx context.Context) bool {
	e.policyMu.RLock()
	check := e.emergencyStopFn
	e.policyMu.RUnlock()
	if check != nil {
		return check(ctx)
	}
	return CheckEmergencyStop(ctx, e.pool)
}

func contractForFinding(f analyzer.Finding) (ActionContract, bool) {
	if err := ValidateExecutorSQL(f.RecommendedSQL); err != nil {
		return ActionContract{}, false
	}
	actionType := actionTypeForProposalSQL(f.RecommendedSQL)
	return ContractForActionType(actionType)
}
