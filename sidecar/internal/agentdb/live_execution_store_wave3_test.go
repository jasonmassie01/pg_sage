package agentdb

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWave3LiveExecutionRecordsPersistAndClaimOnce(t *testing.T) {
	store, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	const deploymentID = "wave3_persisted_live_contract"
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1",
		deploymentID,
	)
	if _, err := store.Provision(ctx, RegisterRequest{
		DeploymentID: deploymentID, TenantID: "wave3", AgentID: "persisted",
		Provider: ProviderAWSRDS, ProvisioningLevel: LevelInstance,
		LeaseSeconds: 3600, BudgetUSD: 25,
	}); err != nil {
		t.Fatalf("provision fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1",
			deploymentID,
		)
	})

	now := time.Now().UTC()
	records, attempt := wave3PersistedExecutionContract(t, deploymentID, now)
	if err := store.PersistLiveExecutionRecords(ctx, records); err != nil {
		t.Fatalf("persist records: %v", err)
	}
	loaded, err := store.LoadLiveExecutionRecords(ctx, attempt, records)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	if result := ValidateLiveExecutionAttempt(loaded, attempt, now); !result.Allowed {
		t.Fatalf("persisted records failed validation: %+v", result)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.ClaimLiveExecution(ctx, attempt, now)
		}()
	}
	wg.Wait()
	close(results)
	var claimed, conflicts int
	for err := range results {
		switch {
		case err == nil:
			claimed++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if claimed != 1 || conflicts != 1 {
		t.Fatalf("claims=%d conflicts=%d, want one each", claimed, conflicts)
	}

	receipt := LiveExecutionReceipt{
		AuthorizationID:    attempt.AuthorizationID,
		IdempotencyKey:     attempt.IdempotencyKey,
		PlanHash:           attempt.PlanHash,
		ProviderResourceID: "resource-1",
	}
	if err := store.PersistLiveExecutionReceipt(ctx, receipt); err != nil {
		t.Fatalf("persist receipt: %v", err)
	}
	loaded, err = store.LoadLiveExecutionRecords(ctx, attempt, records)
	if err != nil {
		t.Fatalf("reload consumed records: %v", err)
	}
	result := ValidateLiveExecutionAttempt(loaded, attempt, now)
	if !result.Replay || result.Receipt == nil ||
		result.Receipt.ProviderResourceID != "resource-1" {
		t.Fatalf("receipt replay was not exact: %+v", result)
	}
}

func TestWave3ProviderReadinessNilRegistryFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	opts := wave3ReadinessOptions(now)
	opts.Registry = nil
	got := wave3ReadinessFor(t, ProviderReadinessList(t.Context(), opts))
	assertWave3DisabledCode(t, got.DisabledReasons, "runner_unavailable")
}

func wave3PersistedExecutionContract(
	t *testing.T,
	deploymentID string,
	now time.Time,
) (LiveExecutionRecords, LiveExecutionAttempt) {
	t.Helper()
	records, attempt := wave3ExecutionContract(t, now)
	plan, err := BuildNormalizedLivePlan(LivePlanInput{
		DeploymentID: deploymentID, Provider: ProviderAWSRDS,
		Operation: ProvisionOpCreate, Region: "us-east-1", Account: "123456789012",
		SizeProfileID: "aws-rds-dev", ProviderParams: map[string]any{"class": "r7g"},
		StorageGB: 20, TTLSeconds: 3600, BudgetUSD: 25,
	})
	if err != nil {
		t.Fatalf("build persisted plan: %v", err)
	}
	records.Plan = &plan
	records.Estimate.PlanHash = plan.Hash
	records.Estimate.DeploymentID = plan.DeploymentID
	records.Estimate.SizeProfileID = plan.SizeProfileID
	records.Estimate.ProviderParamsHash = plan.ProviderParamsHash
	records.Estimate.StorageGB = plan.StorageGB
	records.Estimate.TTLSeconds = plan.TTLSeconds
	records.Estimate.BudgetUSD = plan.BudgetUSD
	records.Authorization.DeploymentID = plan.DeploymentID
	records.Authorization.PlanHash = plan.Hash
	attempt.DeploymentID = plan.DeploymentID
	attempt.PlanHash = plan.Hash
	return records, attempt
}
