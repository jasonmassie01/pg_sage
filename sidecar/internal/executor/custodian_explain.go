package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ExplainCustodianProposal reports the standing gate's verdict for a
// custodian proposal without recording a decision: a pre-incident
// investigation shows it beside the diagnosis the proposal addresses.
// The custodian worker still submits the proposal on its own tick under
// the configured autonomy. No gate, or one that cannot explain, fails
// closed.
func (e *Executor) ExplainCustodianProposal(
	ctx context.Context, proposal CustodianProposal,
) ActionPolicyDecision {
	explainer, ok := e.StandingPolicyGate().(policy.Explainer)
	if !ok {
		return ActionPolicyDecision{Decision: PolicyDecisionBlocked, RiskTier: "unknown",
			BlockedReason: reasonNoStandingPolicy}
	}
	return standingPolicyDecision(explainer.Explain(ctx, custodianRequest(proposal)))
}
