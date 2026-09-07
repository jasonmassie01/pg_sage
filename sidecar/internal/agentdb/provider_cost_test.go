package agentdb

import "testing"

func TestEstimateAgentDBCost(t *testing.T) {
	cases := []CostEstimateRequest{
		{Provider: ProviderAWSRDS, ProviderParams: map[string]any{
			"db_instance_class": "db.t4g.micro", "allocated_storage": 20,
		}, TTLSeconds: 3600},
		{Provider: ProviderGCPCloudSQL, ProviderParams: map[string]any{
			"tier": "db-f1-micro", "storage_size": 10,
		}, TTLSeconds: 3600},
		{Provider: ProviderGCPCloudSQL, ProviderParams: map[string]any{
			"tier": "db-custom-1-3840", "storage_size": 20,
		}, TTLSeconds: 7200},
		{Provider: ProviderDatabricksLakebase, ProviderParams: map[string]any{
			"mode": "autoscaling_branch",
		}, TTLSeconds: 3600},
		{Provider: ProviderAWSRDS, ProviderParams: map[string]any{
			"db_instance_class": "db.unknown",
		}, TTLSeconds: 3600},
	}
	for _, req := range cases {
		got := EstimateAgentDBCost(req)
		if got.MonthlyUSD <= 0 || got.TTLCostUSD <= 0 {
			t.Fatalf("non-positive estimate for %#v: %#v", req, got)
		}
	}
}

func TestBudgetGate(t *testing.T) {
	low := CostEstimate{TTLCostUSD: 0.25, Confidence: "medium"}
	if got := BudgetGate(low, 1); !got.Allowed {
		t.Fatalf("low estimate denied: %#v", got)
	}
	high := CostEstimate{TTLCostUSD: 2, Confidence: "medium"}
	if got := BudgetGate(high, 1); got.Allowed {
		t.Fatalf("high estimate allowed: %#v", got)
	}
	uncertain := CostEstimate{
		TTLCostUSD:        0.6,
		Confidence:        "low",
		UnknownComponents: []string{"serverless_usage"},
	}
	if got := BudgetGate(uncertain, 2); !got.RequiresReview {
		t.Fatalf("uncertain high estimate should require review: %#v", got)
	}
}
