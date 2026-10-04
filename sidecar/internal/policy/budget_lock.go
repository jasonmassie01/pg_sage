package policy

import "context"

// authorizeSerialized evaluates and records a budget-spending request
// inside the cross-process budget lock (GateConfig.Serialize): the usage
// read and the recorded verdict share one transaction, so two sidecars on
// one database cannot both take the last slot of a budget. The lock is
// released when the transaction commits (or rolls back).
func (gate *authorizationGate) authorizeSerialized(
	ctx context.Context, req ActionRequest,
) Decision {
	lockedCtx, done, err := gate.config.Serialize(ctx, req)
	if err != nil {
		return gate.finish(ctx, req, blocked(ReasonPolicyUnavailable,
			"budget lock: "+err.Error()))
	}
	decision, recordErr := gate.record(lockedCtx, req, gate.evaluate(lockedCtx, req))
	if recordErr != nil {
		if rollbackErr := done(false); rollbackErr != nil {
			decision.Detail += "; budget lock rollback: " + rollbackErr.Error()
		}
		return decision
	}
	if err := done(true); err != nil {
		return decisionForRequest(req, blocked(ReasonPolicyUnavailable,
			"budget lock: commit the recorded decision: "+err.Error()))
	}
	return decision
}
