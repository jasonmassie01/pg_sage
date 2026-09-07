package agentdb

import (
	"context"
	"testing"
	"time"
)

func persistedLiveTestRequest(
	t *testing.T,
	store *Store,
	ctx context.Context,
	deploymentID string,
	suffix string,
) LiveExecutionRequest {
	t.Helper()
	dep, err := store.Get(ctx, deploymentID)
	if err != nil {
		t.Fatalf("load live fixture deployment: %v", err)
	}
	params, _ := dep.Metadata["provider_params"].(map[string]any)
	region := stringParam(params, "region")
	if region == "" {
		region = liveTestRegion(dep.Provider)
	}
	storage := float64Param(params, "allocated_storage")
	if storage <= 0 {
		storage = float64Param(params, "storage_size")
	}
	if storage <= 0 {
		storage = 20
	}
	ttl := 3600
	if dep.LeaseExpiresAt != nil {
		ttl = max(1, int(time.Until(*dep.LeaseExpiresAt).Seconds()))
	}
	budget := dep.BudgetUSD
	if budget <= 0 {
		budget = 25
	}
	plan, err := BuildNormalizedLivePlan(LivePlanInput{
		DeploymentID: dep.DeploymentID, Provider: dep.Provider,
		Operation: ProvisionOpCreate, Region: region,
		Account:       stringParam(params, "account"),
		Project:       stringParam(params, "project"),
		Workspace:     stringParam(params, "workspace"),
		SizeProfileID: dep.SizeProfileID, ProviderParams: params,
		StorageGB: storage, TTLSeconds: ttl, BudgetUSD: budget,
	})
	if err != nil {
		t.Fatalf("build persisted live test plan: %v", err)
	}
	now := time.Now().UTC()
	estimateID := "estimate-" + idFrom(deploymentID, suffix)
	authorizationID := "authorization-" + idFrom(deploymentID, suffix)
	policyHash := "policy-" + idFrom(dep.Provider)
	policy := LiveProvisionPolicy{
		LiveProvisioningEnabled: true, ProviderEnabled: true,
		Provider: dep.Provider, AllowedRegions: []string{"*"},
		AllowedAccounts: []string{"*"}, AllowedProjects: []string{"*"},
		AllowedWorkspaces: []string{"*"}, MaxTTLSeconds: 86400,
		AllowPublicIP: plan.PublicIP, MaxEstimatedCostUSD: 1000,
		ExecutionMode: LiveModeApproval,
	}
	estimate := &IssuedLiveCostEstimate{
		EstimateID: estimateID, PlanHash: plan.Hash,
		DeploymentID: plan.DeploymentID, Provider: plan.Provider,
		Region: plan.Region, Account: plan.Account, Project: plan.Project,
		Workspace: plan.Workspace, SizeProfileID: plan.SizeProfileID,
		ProviderParamsHash: plan.ProviderParamsHash,
		StorageGB:          plan.StorageGB, TTLSeconds: plan.TTLSeconds,
		BudgetUSD: plan.BudgetUSD, EstimatedCostUSD: 1,
		PricingRevision: "test-pricing-v1", Confidence: EstimateConfidenceHigh,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	authz := &LiveOperationAuthorization{
		AuthorizationID: authorizationID, Allowed: true,
		DeploymentID: plan.DeploymentID, Provider: plan.Provider,
		Operation: plan.Operation, PlanHash: plan.Hash, EstimateID: estimateID,
		PolicyHash: policyHash, PolicyVersion: 1,
		RequesterID: "test-requester", ReviewerID: "test-reviewer",
		ExpiresAt: now.Add(10 * time.Minute), Nonce: "nonce-" + suffix,
		IdempotencyKey: "idem-" + idFrom(deploymentID, suffix),
	}
	records := LiveExecutionRecords{
		Plan: &plan, Estimate: estimate, Authorization: authz,
		CurrentPolicy: policy, CurrentPolicyHash: policyHash,
		CurrentPolicyVersion: 1, CurrentPricingRevision: "test-pricing-v1",
	}
	attempt := LiveExecutionAttempt{
		DeploymentID: plan.DeploymentID, Provider: plan.Provider,
		Operation: plan.Operation, PlanHash: plan.Hash,
		EstimateID: estimateID, AuthorizationID: authorizationID,
		AuthenticatedRequesterID: authz.RequesterID,
		IdempotencyKey:           authz.IdempotencyKey,
	}
	if err := store.PersistLiveExecutionRecords(ctx, records); err != nil {
		t.Fatalf("persist live test records: %v", err)
	}
	return LiveExecutionRequest{
		Mode: "live", Records: &records, Attempt: &attempt, Now: now,
	}
}

func liveTestRegion(provider string) string {
	switch provider {
	case ProviderGCPCloudSQL:
		return "us-central1"
	default:
		return "us-east-1"
	}
}
