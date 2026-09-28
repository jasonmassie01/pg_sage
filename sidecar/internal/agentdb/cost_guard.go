package agentdb

import (
	"fmt"
	"slices"
)

// unknownInstanceClass marks an estimate whose instance class has no known
// price; issuing live work against it would authorize unbounded spend.
const unknownInstanceClass = "instance_class"

// liveCostAllowed fails closed on unpriced instance classes (unless the
// provider policy explicitly allows unknown pricing) and applies the
// deployment budget gate before any live authorization is issued (G8-B08).
func liveCostAllowed(input LiveExecutionIssueInput, estimate *IssuedLiveCostEstimate) error {
	if input.Operation != ProvisionOpCreate {
		return nil
	}
	allowUnknown := input.Provider != nil && input.Provider.Policy.AllowUnknownPricing
	if slices.Contains(estimate.UnknownComponents, unknownInstanceClass) && !allowUnknown {
		return fmt.Errorf("%w: instance class has no known price", ErrInvalid)
	}
	gate := BudgetGate(CostEstimate{
		TTLCostUSD: estimate.EstimatedCostUSD, Confidence: estimate.Confidence,
		UnknownComponents: estimate.UnknownComponents,
	}, input.Deployment.BudgetUSD)
	if !gate.Allowed {
		return fmt.Errorf("%w: %v", ErrInvalid, gate.DisabledReasons)
	}
	return nil
}

// leaseExtensionAllowed caps a lease extension by the effective policy TTL
// and, for cloud instances, re-evaluates the cost of the new lease against
// the deployment budget; an unpriced live instance cannot be extended.
func leaseExtensionAllowed(dep Deployment, req LeaseRequest) error {
	if req.MaxTTLSeconds > 0 && req.LeaseSeconds > req.MaxTTLSeconds {
		return fmt.Errorf("%w: lease exceeds provider policy ttl", ErrInvalid)
	}
	if !cloudProvider(dep.Provider) || dep.ProvisioningLevel != LevelInstance {
		return nil
	}
	params := providerParams(dep)
	estimate := EstimateAgentDBCost(CostEstimateRequest{
		Provider: dep.Provider, ProvisioningLevel: dep.ProvisioningLevel,
		ProviderParams: params, StorageGB: float64Param(params, "storage_gb"),
		TTLSeconds: req.LeaseSeconds, BudgetUSD: dep.BudgetUSD,
	})
	if dep.LiveMode && slices.Contains(estimate.UnknownComponents, unknownInstanceClass) {
		return fmt.Errorf("%w: cannot extend an unpriced live instance", ErrInvalid)
	}
	if gate := BudgetGate(estimate, dep.BudgetUSD); !gate.Allowed {
		return fmt.Errorf("%w: extended lease exceeds deployment budget", ErrInvalid)
	}
	return nil
}

// hostedCost prices Neon/Supabase as usage-based rather than as an unknown
// instance class; the estimate stays low-confidence (and is doubled).
func hostedCost(req CostEstimateRequest) CostEstimate {
	monthly := 5.0
	if stringParam(req.ProviderParams, "mode") == "project" {
		monthly = 25
	}
	return finishEstimate(req, monthly, "low", []string{"usage_based"})
}
