package agentdb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWave3NormalizedLivePlanIsServerOwnedAndImmutable(t *testing.T) {
	params := map[string]any{
		"db_instance_class":     "db.r7g.large",
		"allocated_storage":     120,
		"backup_retention_days": 14,
	}
	plan, err := BuildNormalizedLivePlan(LivePlanInput{
		DeploymentID:   "wave3-plan",
		Provider:       ProviderAWSRDS,
		Operation:      ProvisionOpCreate,
		Region:         "us-east-1",
		Account:        "123456789012",
		SizeProfileID:  "rds-prod",
		ProviderParams: params,
		StorageGB:      120,
		TTLSeconds:     3600,
		BudgetUSD:      25,
	})
	if err != nil {
		t.Fatalf("build normalized plan: %v", err)
	}
	if plan.Hash == "" || plan.ProviderParamsHash == "" {
		t.Fatalf("normalized plan omitted immutable hashes: %+v", plan)
	}
	params["db_instance_class"] = "db.t4g.micro"
	if got := plan.ProviderParams["db_instance_class"]; got != "db.r7g.large" {
		t.Fatalf("caller mutation changed server plan to %v", got)
	}
	rebuilt, err := BuildNormalizedLivePlan(plan.Input())
	if err != nil {
		t.Fatalf("rebuild normalized plan: %v", err)
	}
	if rebuilt.Hash != plan.Hash || !reflect.DeepEqual(rebuilt, plan) {
		t.Fatalf("normalization is not deterministic:\nfirst=%+v\nsecond=%+v", plan, rebuilt)
	}
}

func TestWave3ValidLiveExecutionContractAuthorizesExactTuple(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	got := ValidateLiveExecutionAttempt(records, attempt, now)
	if !got.Allowed || got.Replay {
		t.Fatalf("valid exact execution tuple rejected: %+v", got)
	}
}

func TestWave3PlanIntegrityIsRecheckedAtExecution(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	records.Plan.ProviderParams["class"] = "tampered"
	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "integrity",
	)
}

func TestWave3EstimateMustExistBeFreshAndMatchPlan(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*LiveExecutionRecords, *LiveExecutionAttempt)
		reason string
	}{
		{"missing", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate = nil
		}, "estimate"},
		{"expired", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.ExpiresAt = now.Add(-time.Second)
		}, "expired"},
		{"superseded", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			superseded := now.Add(-time.Minute)
			r.Estimate.SupersededAt = &superseded
		}, "superseded"},
		{"changed plan", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.PlanHash = "other-plan"
		}, "plan"},
		{"changed profile", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.SizeProfileID = "rds-other"
		}, "profile"},
		{"changed params", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.ProviderParamsHash = "other-params"
		}, "parameter"},
		{"changed region", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.Region = "us-west-2"
		}, "region"},
		{"changed account", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.Account = "999999999999"
		}, "account"},
		{"changed provider", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.Provider = ProviderGCPCloudSQL
		}, "provider"},
		{"changed deployment", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.DeploymentID = "other-deployment"
		}, "deployment"},
		{"changed ttl", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.TTLSeconds++
		}, "ttl"},
		{"changed storage", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.StorageGB++
		}, "storage"},
		{"changed budget", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Estimate.BudgetUSD++
		}, "budget"},
		{"changed catalog", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.CurrentPricingRevision = "prices-2026-07-20"
		}, "pricing"},
	}
	wave3RunExecutionDenials(t, now, tests)
}

func TestWave3LowConfidenceEstimateCannotAutoAuthorize(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	records.CurrentPolicy.ExecutionMode = LiveModeAutoWithinPolicy
	records.Estimate.Confidence = EstimateConfidenceLow
	records.Authorization.AutoIssued = true
	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "confidence",
	)
}

func TestWave3UnknownCostComponentsCannotAutoAuthorize(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	records.CurrentPolicy.ExecutionMode = LiveModeAutoWithinPolicy
	records.Estimate.UnknownComponents = []string{"serverless_usage"}
	records.Authorization.AutoIssued = true

	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "unknown",
	)
}

func TestWave3LiveExecutionCurrentPolicyRejectsPublicIP(t *testing.T) {
	now := time.Now().UTC()
	records, attempt := wave3ExecutionContract(t, now)
	records.CurrentPolicy.AllowPublicIP = false
	records.Plan.PublicIP = true
	planHash, err := hashNormalizedPlan(*records.Plan)
	if err != nil {
		t.Fatalf("rehash public plan: %v", err)
	}
	records.Plan.Hash = planHash
	records.Estimate.PlanHash = planHash
	records.Authorization.PlanHash = planHash
	attempt.PlanHash = planHash
	got := ValidateLiveExecutionAttempt(records, attempt, now)
	assertWave3ExecutionDenied(t, got, "public ip")
}

func TestWave3AuthorizationIsExactFreshAndServerOwned(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*LiveExecutionRecords, *LiveExecutionAttempt)
		reason string
	}{
		{"missing", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization = nil
		}, "authorization"},
		{"expired", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.ExpiresAt = now.Add(-time.Second)
		}, "expired"},
		{"revoked", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			revoked := now.Add(-time.Minute)
			r.Authorization.RevokedAt = &revoked
		}, "revoked"},
		{"wrong plan", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.PlanHash = "other-plan"
		}, "plan"},
		{"wrong estimate", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.EstimateID = "other-estimate"
		}, "estimate"},
		{"wrong policy", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.CurrentPolicyHash = "policy-tightened"
		}, "policy"},
		{"wrong policy generation", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.CurrentPolicyVersion++
		}, "policy"},
		{"wrong deployment", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.DeploymentID = "other-deployment"
		}, "deployment"},
		{"wrong operation", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.Operation = ProvisionOpDestroy
		}, "operation"},
		{"wrong provider", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.Provider = ProviderGCPCloudSQL
		}, "provider"},
		{"wrong requester", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.RequesterID = "user-99"
		}, "requester"},
		{"wrong idempotency", func(_ *LiveExecutionRecords, a *LiveExecutionAttempt) {
			a.IdempotencyKey = "different-key"
		}, "idempotency"},
		{"missing nonce", func(r *LiveExecutionRecords, _ *LiveExecutionAttempt) {
			r.Authorization.Nonce = ""
		}, "nonce"},
	}
	wave3RunExecutionDenials(t, now, tests)
}

func TestWave3BodyClaimsCannotCreateExecutionAuthority(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	records.Estimate = nil
	records.Authorization = nil
	attempt.ClientClaims = LiveExecutionClientClaims{
		Approved:            true,
		EstimatedCostUSD:    0.01,
		CostEstimateID:      "made-up-estimate",
		ActorID:             "admin",
		AdminOverrideReason: "trust the request body",
		Policy:              wave3LayerPolicy([]string{"*"}, 86400, 1000, LiveModeAutoWithinPolicy),
	}

	got := ValidateLiveExecutionAttempt(records, attempt, now)
	assertWave3ExecutionDenied(t, got, "estimate")
	if got.Replay {
		t.Fatalf("body claims produced replay authority: %+v", got)
	}
}

func TestWave3ConsumedAuthorizationCannotMutateProviderTwice(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	consumed := now.Add(-time.Minute)
	records.Estimate.ConsumedAt = &consumed
	records.Authorization.ConsumedAt = &consumed

	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "consumed",
	)
	records.Receipt = &LiveExecutionReceipt{
		AuthorizationID:    records.Authorization.AuthorizationID,
		IdempotencyKey:     attempt.IdempotencyKey,
		PlanHash:           records.Plan.Hash,
		ProviderResourceID: "db-wave3-existing",
	}
	got := ValidateLiveExecutionAttempt(records, attempt, now)
	if got.Allowed || !got.Replay || got.Receipt == nil {
		t.Fatalf("exact retry was not a non-mutating receipt replay: %+v", got)
	}

	attempt.IdempotencyKey = "new-key"
	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "consumed",
	)
}

func TestWave3AuthenticatedReviewerCannotComeFromJSON(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	records, attempt := wave3ExecutionContract(t, now)
	records.Authorization.ReviewerID = ""
	attempt.ClientClaims.ActorID = "reviewer-from-body"
	assertWave3ExecutionDenied(
		t, ValidateLiveExecutionAttempt(records, attempt, now), "reviewer",
	)
}

func wave3ExecutionContract(
	t *testing.T,
	now time.Time,
) (LiveExecutionRecords, LiveExecutionAttempt) {
	t.Helper()
	plan, err := BuildNormalizedLivePlan(LivePlanInput{
		DeploymentID: "wave3-execute", Provider: ProviderAWSRDS,
		Operation: ProvisionOpCreate, Region: "us-east-1", Account: "123456789012",
		SizeProfileID: "rds-prod", ProviderParams: map[string]any{"class": "r7g"},
		StorageGB: 100, TTLSeconds: 3600, BudgetUSD: 25,
	})
	if err != nil {
		t.Fatalf("build baseline normalized plan: %v", err)
	}
	estimate := &IssuedLiveCostEstimate{
		EstimateID: "estimate-1", PlanHash: plan.Hash,
		DeploymentID: plan.DeploymentID, Provider: plan.Provider,
		Region: plan.Region, Account: plan.Account,
		SizeProfileID: plan.SizeProfileID, ProviderParamsHash: plan.ProviderParamsHash,
		StorageGB: plan.StorageGB, TTLSeconds: plan.TTLSeconds, BudgetUSD: plan.BudgetUSD,
		EstimatedCostUSD: 4.5, PricingRevision: "prices-2026-07-19",
		Confidence: EstimateConfidenceHigh, ExpiresAt: now.Add(15 * time.Minute),
	}
	authz := &LiveOperationAuthorization{
		AuthorizationID: "auth-1", Allowed: true,
		DeploymentID: plan.DeploymentID, Provider: plan.Provider, Operation: plan.Operation,
		PlanHash: plan.Hash, EstimateID: estimate.EstimateID, PolicyHash: "policy-12",
		PolicyVersion: 12,
		RequesterID:   "user-7", ReviewerID: "user-8", ExpiresAt: now.Add(10 * time.Minute),
		Nonce: "nonce-1", IdempotencyKey: "idem-1",
	}
	policy := wave3LayerPolicy([]string{"us-east-1"}, 7200, 25, LiveModeApproval)
	return LiveExecutionRecords{
			Plan: &plan, Estimate: estimate, Authorization: authz,
			CurrentPolicy: policy, CurrentPolicyHash: "policy-12", CurrentPolicyVersion: 12,
			CurrentPricingRevision: "prices-2026-07-19",
		}, LiveExecutionAttempt{
			DeploymentID: plan.DeploymentID, Provider: plan.Provider,
			Operation: plan.Operation, PlanHash: plan.Hash,
			EstimateID: estimate.EstimateID, AuthorizationID: authz.AuthorizationID,
			AuthenticatedRequesterID: "user-7", IdempotencyKey: "idem-1",
		}
}

func wave3RunExecutionDenials(
	t *testing.T,
	now time.Time,
	tests []struct {
		name   string
		mutate func(*LiveExecutionRecords, *LiveExecutionAttempt)
		reason string
	},
) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			records, attempt := wave3ExecutionContract(t, now)
			tc.mutate(&records, &attempt)
			got := ValidateLiveExecutionAttempt(records, attempt, now)
			assertWave3ExecutionDenied(t, got, tc.reason)
		})
	}
}

func assertWave3ExecutionDenied(
	t *testing.T,
	got LiveExecutionValidationResult,
	reason string,
) {
	t.Helper()
	if got.Allowed || got.Replay {
		t.Fatalf("execution authorized unexpectedly: %+v", got)
	}
	joined := strings.ToLower(strings.Join(got.DisabledReasons, " "))
	if !strings.Contains(joined, strings.ToLower(reason)) {
		t.Fatalf("disabled reasons %q do not identify %q", joined, reason)
	}
}
