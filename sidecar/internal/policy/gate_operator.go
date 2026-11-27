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
	// A human decided: the refusal set does not apply. The lock ceiling is a
	// safety bound and does.
	if decision, stop := gate.operatorWindowDecision(doc, runtime, req); stop {
		return withDocumentBounds(doc, decision)
	}
	return withDocumentBounds(doc, gate.decision(req, VerdictExecute, ReasonOperatorApproved))
}

// operatorWindowDecision bounds moderate and high operator actions by the
// policy windows and, when one is configured, trust.maintenance_window.
func (gate *authorizationGate) operatorWindowDecision(
	doc Document, runtime RuntimeState, req ActionRequest,
) (Decision, bool) {
	if req.Contract.RiskTier != RiskModerate && req.Contract.RiskTier != RiskHigh {
		return Decision{}, false
	}
	if approvedMitigation(req) || req.assumeWindowOpen {
		return Decision{}, false
	}
	now := gate.now()
	trustWindowOpen := !runtime.WindowConfigured || runtime.InConfiguredWindow
	inWindow := trustWindowOpen && inAnyWindow(doc.MaintenanceWindows, now)
	return gate.windowVerdict(doc, req, inWindow,
		inWindow && gate.windowClosing(doc, runtime, runtime.WindowConfigured, now))
}

// approvedMitigation reports a human-approved, mitigation-only backend
// cancel: an incident mitigation, not maintenance, so maintenance windows
// do not bind it (Sage SRE M5). Every other gate still does.
func approvedMitigation(req ActionRequest) bool {
	return req.OperatorApproved && req.Contract != nil &&
		req.Contract.ActionType == "cancel_backend" &&
		req.Contract.RollbackClass == RollbackMitigationOnly
}
