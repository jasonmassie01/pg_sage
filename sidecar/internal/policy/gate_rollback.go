package policy

import "context"

// ReasonRollbackMitigation executes the revert of a change that already
// ran (policy-05): a mitigation, outside the windows, budgets, per-class
// autonomy and change-class allowlist.
const ReasonRollbackMitigation Reason = "rollback_mitigation"

// rollbackMitigation reports a revert the gate decides as a mitigation:
// every rollback request except pg_sage's own soft-drop restore, which
// keeps its trust-downgrade bypass (dropRestoreDecision, executor-01). It
// extends approvedMitigation: neither is maintenance, so maintenance
// windows do not bind them.
func rollbackMitigation(req ActionRequest) bool {
	return req.Rollback && (req.OperatorApproved || !IsDropRestore(req))
}

// rollbackDecision authorizes the revert of an executed or operator-approved
// change (policy-05). The hard stops, request validation, the provider list
// and confirmed facts have already bound (evaluate). Here the observation
// trust refusal, the rollback contract's approval guardrail and, for an
// automatic revert, the policy's approval classes and refusal set still
// bind, and the lock ceiling bounds the run. A policy read error does not
// strand the revert: it runs under safety.lock_timeout_ms (no ceiling) and
// the decision names the error.
func (gate *authorizationGate) rollbackDecision(
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
	if !req.OperatorApproved && hasGuardrail(*req.Contract, GuardrailApprovalRequired) {
		return gate.decision(req, VerdictQueueApproval, ReasonApprovalRequired)
	}
	note := budgetNote{bypass: BudgetBypassFor(req, gate.now())}
	if req.OperatorApproved {
		note = budgetNote{} // an operator's revert spends no self-initiated budget
	}
	doc, err := gate.policy(ctx, req)
	if err == nil {
		err = ValidateDocument(doc)
	}
	if err != nil {
		revert := gate.decision(req, VerdictExecute, ReasonRollbackMitigation)
		revert.PolicyReadError = err.Error()
		revert.Detail = "standing policy unreadable (" + err.Error() +
			"): the revert runs under safety.lock_timeout_ms"
		return note.stamp(revert)
	}
	if decision, stop := automaticRevertDocumentDecision(doc, req); stop {
		return decision
	}
	return note.stamp(withDocumentBounds(doc,
		gate.decision(req, VerdictExecute, ReasonRollbackMitigation)))
}

// automaticRevertDocumentDecision sends an automatic revert to a person
// when the standing policy asks one for its class (approval classes) or
// its statement (the refusal set). An operator's revert already has one.
func automaticRevertDocumentDecision(doc Document, req ActionRequest) (Decision, bool) {
	if req.OperatorApproved {
		return Decision{}, false
	}
	if containsChangeClass(doc.ApprovalRequiredClasses, ChangeClass(req.Feature)) {
		return decisionForRequest(req, blockedAs(VerdictQueueApproval,
			ReasonApprovalRequired)), true
	}
	return refusalDecision(doc, req)
}
