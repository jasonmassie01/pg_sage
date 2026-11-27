package policy

import (
	"context"
	"fmt"
)

func (gate *authorizationGate) finish(
	ctx context.Context,
	req ActionRequest,
	decision Decision,
) Decision {
	recorded, _ := gate.record(ctx, req, decision) // a failed record is a blocked verdict
	return recorded
}

// record stamps and records the decision. A failed record returns
// recordFailure's blocked policy_unavailable decision and the error.
func (gate *authorizationGate) record(
	ctx context.Context, req ActionRequest, decision Decision,
) (Decision, error) {
	decision = decisionForRequest(req, decision)
	if gate.config.RecordDecisionDetailed != nil {
		evidenceID, decisionID, err := gate.config.RecordDecisionDetailed(ctx, req, decision)
		if err != nil {
			return gate.recordFailure(ctx, req, decision, err), err
		}
		decision.EvidenceID, decision.DecisionID = evidenceID, decisionID
		return decision, nil
	}
	if gate.config.RecordDecision == nil {
		return decision, nil
	}
	evidenceID, err := gate.config.RecordDecision(ctx, req, decision)
	if err != nil {
		return gate.recordFailure(ctx, req, decision, err), err
	}
	decision.EvidenceID = evidenceID
	return decision, nil
}

// recordFailure answers a decision the gate could not record. It fails
// closed (blocked policy_unavailable) and its detail names the verdict and
// reason the gate meant to record, so the operator sees the real reason;
// GateConfig.RecordFailed hears the original decision and the error.
func (gate *authorizationGate) recordFailure(
	ctx context.Context, req ActionRequest, decision Decision, err error,
) Decision {
	if gate.config.RecordFailed != nil {
		gate.config.RecordFailed(ctx, req, decision, err)
	}
	return decisionForRequest(req, blocked(ReasonPolicyUnavailable,
		fmt.Sprintf("could not record the %s decision: %v", decision.LedgerLabel(), err)))
}

// LedgerLabel names a decision as the ledger records it, verdict/reason
// (for example parked/no_typed_contract).
func (d Decision) LedgerLabel() string {
	verdict := string(d.Verdict)
	if d.Verdict == VerdictPark {
		verdict = "parked" // the ledger's spelling of the park verdict
	}
	return verdict + "/" + string(d.Reason)
}
