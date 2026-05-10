package agentdb

import "math"

type CostEstimateRequest struct {
	Provider          string
	ProvisioningLevel string
	SizeProfileID     string
	ProviderParams    map[string]any
	StorageGB         float64
	TTLSeconds        int
	BudgetUSD         float64
}

type CostEstimate struct {
	Provider          string   `json:"provider"`
	MonthlyUSD        float64  `json:"monthly_usd"`
	TTLCostUSD        float64  `json:"ttl_cost_usd"`
	Confidence        string   `json:"confidence"`
	UnknownComponents []string `json:"unknown_components"`
}

func EstimateAgentDBCost(req CostEstimateRequest) CostEstimate {
	switch normalizeProvider(req.Provider) {
	case ProviderAWSRDS:
		return rdsCost(req)
	case ProviderGCPCloudSQL:
		return cloudSQLCost(req)
	case ProviderDatabricksLakebase:
		return lakebaseCost(req)
	default:
		return unknownCost(req)
	}
}

func BudgetGate(estimate CostEstimate, budgetUSD float64) LivePolicyDecision {
	if budgetUSD <= 0 {
		return LivePolicyDecision{Allowed: true}
	}
	compare := estimate.TTLCostUSD
	if estimate.Confidence == "low" || len(estimate.UnknownComponents) > 0 {
		compare *= 2
	}
	if compare > budgetUSD {
		return LivePolicyDecision{
			Allowed: false,
			DisabledReasons: []string{
				"estimated ttl cost exceeds deployment budget",
			},
		}
	}
	decision := LivePolicyDecision{Allowed: true}
	if compare > budgetUSD*0.5 {
		decision.RequiresReview = true
		decision.Reasons = append(decision.Reasons, "estimate is above half the budget")
	}
	return decision
}

func rdsCost(req CostEstimateRequest) CostEstimate {
	class := stringParam(req.ProviderParams, "db_instance_class")
	monthly := map[string]float64{
		"db.t4g.micro":  12,
		"db.t4g.small":  24,
		"db.t4g.medium": 48,
	}[class]
	if monthly == 0 {
		estimate := unknownCost(req)
		estimate.Provider = ProviderAWSRDS
		return estimate
	}
	storage := math.Max(req.StorageGB, float64Param(req.ProviderParams, "allocated_storage"))
	monthly += storage * 0.115
	return finishEstimate(req, monthly, "medium", nil)
}

func cloudSQLCost(req CostEstimateRequest) CostEstimate {
	tier := stringParam(req.ProviderParams, "tier")
	monthly := map[string]float64{
		"db-f1-micro":      8,
		"db-g1-small":      25,
		"db-custom-1-3840": 52,
	}[tier]
	if monthly == 0 {
		estimate := unknownCost(req)
		estimate.Provider = ProviderGCPCloudSQL
		return estimate
	}
	storage := math.Max(req.StorageGB, float64Param(req.ProviderParams, "storage_size"))
	monthly += storage * 0.17
	return finishEstimate(req, monthly, "medium", nil)
}

func lakebaseCost(req CostEstimateRequest) CostEstimate {
	mode := stringParam(req.ProviderParams, "mode")
	if mode == "" || mode == "autoscaling_branch" {
		return finishEstimate(req, 6, "low", []string{"serverless_usage"})
	}
	return finishEstimate(req, 120, "low", []string{"workspace_compute"})
}

func unknownCost(req CostEstimateRequest) CostEstimate {
	return finishEstimate(req, 100, "low", []string{"instance_class"})
}

func finishEstimate(
	req CostEstimateRequest,
	monthly float64,
	confidence string,
	unknown []string,
) CostEstimate {
	ttl := float64(req.TTLSeconds)
	if ttl <= 0 {
		ttl = 3600
	}
	return CostEstimate{
		Provider:          normalizeProvider(req.Provider),
		MonthlyUSD:        roundCents(monthly),
		TTLCostUSD:        roundCents(monthly * ttl / (30 * 24 * 3600)),
		Confidence:        confidence,
		UnknownComponents: unknown,
	}
}

func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

func stringParam(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	if value, ok := params[key].(string); ok {
		return value
	}
	return ""
}

func float64Param(params map[string]any, key string) float64 {
	if params == nil {
		return 0
	}
	switch value := params[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	default:
		return 0
	}
}
