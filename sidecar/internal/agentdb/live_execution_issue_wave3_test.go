package agentdb

import (
	"errors"
	"testing"
	"time"
)

func TestWave3IssueLiveExecutionRecordsOwnsTheFullAuthorityTuple(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	input := wave3LiveIssueInput(now, ProviderAWSRDS, LiveModeApproval)

	records, err := IssueLiveExecutionRecords(input)
	if err != nil {
		t.Fatalf("IssueLiveExecutionRecords: %v", err)
	}
	if records.Plan == nil || records.Estimate == nil || records.Authorization == nil {
		t.Fatalf("issued tuple is incomplete: %+v", records)
	}
	if records.Plan.Operation != ProvisionOpCreate ||
		records.Authorization.RequesterID != "user:7" ||
		records.Authorization.ReviewerID != "user:8" ||
		records.Authorization.IdempotencyKey != "create-7" {
		t.Fatalf("issued authorization tuple is wrong: %+v", records.Authorization)
	}
	if records.Estimate.PricingRevision != "static-pricing-v1" ||
		records.Estimate.PlanHash != records.Plan.Hash ||
		records.Authorization.PolicyHash != records.CurrentPolicyHash {
		t.Fatalf("issued provenance is inconsistent: %+v", records)
	}
	attempt := LiveExecutionAttempt{
		DeploymentID: input.Deployment.DeploymentID,
		Provider:     input.Deployment.Provider, Operation: input.Operation,
		PlanHash: records.Plan.Hash, EstimateID: records.Estimate.EstimateID,
		AuthorizationID:          records.Authorization.AuthorizationID,
		AuthenticatedRequesterID: input.RequesterID,
		IdempotencyKey:           input.IdempotencyKey,
	}
	if got := ValidateLiveExecutionAttempt(records, attempt, now); !got.Allowed {
		t.Fatalf("server-issued tuple is not executable: %+v", got)
	}
}

func TestWave3IssueLiveExecutionRecordsFailsClosedForManualOrUnknownAutoCost(
	t *testing.T,
) {
	now := time.Now().UTC().Truncate(time.Second)
	manual := wave3LiveIssueInput(now, ProviderAWSRDS, LiveModeManual)
	if _, err := IssueLiveExecutionRecords(manual); !errors.Is(err, ErrInvalid) {
		t.Fatalf("manual issue error = %v, want ErrInvalid", err)
	}
	auto := wave3LiveIssueInput(now, ProviderDatabricksLakebase, LiveModeAutoWithinPolicy)
	auto.AutoIssued = true
	auto.ReviewerID = ""
	if _, err := IssueLiveExecutionRecords(auto); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown auto cost issue error = %v, want ErrInvalid", err)
	}
}

func wave3LiveIssueInput(
	now time.Time,
	provider string,
	mode string,
) LiveExecutionIssueInput {
	policy := wave3LayerPolicy([]string{"*"}, 86400, 1000, mode)
	policy.Provider = provider
	params := map[string]any{
		"region": "us-east-1", "db_instance_class": "db.t4g.micro",
		"allocated_storage": 20,
	}
	if provider == ProviderDatabricksLakebase {
		params = map[string]any{"region": "us-east-1", "mode": "autoscaling_branch"}
	}
	lease := now.Add(time.Hour)
	return LiveExecutionIssueInput{
		Deployment: Deployment{
			DeploymentID: "issue-live", Provider: provider,
			ProvisioningLevel: LevelInstance, SizeProfileID: "small",
			LeaseExpiresAt: &lease, BudgetUSD: 100,
			Metadata: map[string]any{"provider_params": params},
		},
		Operation: ProvisionOpCreate, RequesterID: "user:7",
		ReviewerID: "user:8", IdempotencyKey: "create-7",
		Runtime: &RuntimeLiveCapability{
			Enabled: true, RunnerAvailable: true, Policy: policy,
		},
		Global:          &LivePolicyLayer{Policy: policy, Version: 3},
		Provider:        &LivePolicyLayer{Policy: policy, Version: 4},
		PricingRevision: "static-pricing-v1", Now: now,
	}
}
