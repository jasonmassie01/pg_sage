package policy

import "context"

// operatorDecision authorizes a human-approved request. The approval
// replaces tier flags, the trust ramp, execution mode and the
// self-initiated usage limits. Trust, the standing policy's change-class
// allowlist and its maintenance windows still bind (decision 2026-09-26).
func (gate *authorizationGate) operatorDecision(
	ctx context.Context, runtime RuntimeState, req ActionRequest,
) Decision {
	switch runtime.TrustLevel {
	case TrustObservation:
		return blockedAs(VerdictObserveOnly, ReasonObserveOnly)
	case TrustAdvisory, TrustAutonomous:
	default:
		return blocked(ReasonUnknownTrustLevel, runtime.TrustLevel)
	}
	if !knownRisk(req.Contract.RiskTier) {
		return blocked(ReasonUnknownRiskTier, "")
	}
	doc, err := gate.policy(ctx, req)
	if err != nil || ValidateDocument(doc) != nil {
		return blocked(ReasonPolicyUnavailable, errorDetail(err))
	}
	if !containsChangeClass(doc.AllowedChangeClasses, ChangeClass(req.Feature)) {
		return gate.decision(req, VerdictBlocked, ReasonChangeClassNotAllowed)
	}
	if decision, stop := gate.operatorWindowDecision(doc, runtime, req); stop {
		return decision
	}
	return gate.decision(req, VerdictExecute, ReasonOperatorApproved)
}

// operatorWindowDecision bounds moderate and high operator actions by the
// policy windows and, when one is configured, trust.maintenance_window.
func (gate *authorizationGate) operatorWindowDecision(
	doc Document, runtime RuntimeState, req ActionRequest,
) (Decision, bool) {
	if req.Contract.RiskTier != RiskModerate && req.Contract.RiskTier != RiskHigh {
		return Decision{}, false
	}
	now := gate.now()
	trustWindowOpen := !runtime.WindowConfigured || runtime.InConfiguredWindow
	if trustWindowOpen && inAnyWindow(doc.MaintenanceWindows, now) {
		return Decision{}, false
	}
	if validDeadlineOverride(doc, req.Deadline, now) {
		decision := gate.decision(req, VerdictExecute, ReasonDeadlineOverride)
		decision.OffWindowOK = true
		return decision, true
	}
	return gate.decision(req, VerdictBlocked, ReasonOutsideMaintenanceWindow), true
}
