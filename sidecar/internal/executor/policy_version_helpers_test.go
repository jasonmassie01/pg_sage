package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The gate stamps each decision with the policy version it evaluated
// (policy-03). Tests that build a decision by hand state the version the
// gate would have stamped.

// ledgerInputAt is ledgerInput for a decision made under policy version.
func ledgerInputAt(
	databaseID *int, version int64, req policy.ActionRequest, d policy.Decision,
) ledger.DecisionInput {
	d.PolicyVersion = version
	return ledgerInput(databaseID, req, d)
}

// recordStandingDecisionAt records a decision made under policy version.
func (e *Executor) recordStandingDecisionAt(
	ctx context.Context, version int64, req policy.ActionRequest, d policy.Decision,
) (string, int64, error) {
	d.PolicyVersion = version
	return e.recordStandingDecision(ctx, req, d)
}
