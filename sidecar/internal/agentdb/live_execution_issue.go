package agentdb

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type LiveExecutionIssueInput struct {
	Deployment      Deployment
	Operation       ProvisionOperation
	RequesterID     string
	ReviewerID      string
	IdempotencyKey  string
	Runtime         *RuntimeLiveCapability
	Global          *LivePolicyLayer
	Provider        *LivePolicyLayer
	PricingRevision string
	Now             time.Time
	AutoIssued      bool
}

func IssueLiveExecutionRecords(
	input LiveExecutionIssueInput,
) (LiveExecutionRecords, error) {
	input.Now = normalizedIssueTime(input.Now)
	if err := validateLiveIssueInput(input); err != nil {
		return LiveExecutionRecords{}, err
	}
	plan, err := issueNormalizedLivePlan(input)
	if err != nil {
		return LiveExecutionRecords{}, err
	}
	estimate, err := issueLiveEstimate(input, plan)
	if err != nil {
		return LiveExecutionRecords{}, err
	}
	authz, err := issueLiveAuthorization(input, plan, estimate)
	if err != nil {
		return LiveExecutionRecords{}, err
	}
	resolved := resolveIssuedLivePolicy(input, plan, estimate, authz)
	if !resolved.Decision.Allowed || resolved.Decision.RequiresReview {
		return LiveExecutionRecords{}, fmt.Errorf("%w: live policy denied issuance", ErrInvalid)
	}
	authz.PolicyHash = resolved.PolicyHash
	authz.PolicyVersion = LivePolicyGeneration(resolved.PolicyHash)
	records := LiveExecutionRecords{
		Plan: &plan, Estimate: estimate, Authorization: authz,
		CurrentPolicy: resolved.Policy, CurrentPolicyHash: resolved.PolicyHash,
		CurrentPolicyVersion:   authz.PolicyVersion,
		CurrentPricingRevision: input.PricingRevision,
	}
	if err := validatePersistedLiveRecords(records); err != nil {
		return LiveExecutionRecords{}, err
	}
	return records, nil
}

func validateLiveIssueInput(input LiveExecutionIssueInput) error {
	if input.Deployment.DeploymentID == "" || input.Deployment.Provider == "" ||
		(input.Operation != ProvisionOpCreate && input.Operation != ProvisionOpDestroy) ||
		strings.TrimSpace(input.RequesterID) == "" ||
		strings.TrimSpace(input.IdempotencyKey) == "" ||
		strings.TrimSpace(input.PricingRevision) == "" {
		return ErrInvalid
	}
	if !input.AutoIssued && strings.TrimSpace(input.ReviewerID) == "" {
		return ErrInvalid
	}
	return nil
}

func issueNormalizedLivePlan(
	input LiveExecutionIssueInput,
) (NormalizedLivePlan, error) {
	params, _ := input.Deployment.Metadata["provider_params"].(map[string]any)
	storage := float64Param(params, "allocated_storage")
	if storage <= 0 {
		storage = float64Param(params, "storage_size")
	}
	if storage <= 0 {
		storage = float64Param(params, "storage_gb")
	}
	return BuildNormalizedLivePlan(LivePlanInput{
		DeploymentID: input.Deployment.DeploymentID,
		Provider:     input.Deployment.Provider, Operation: input.Operation,
		Region: stringParam(params, "region"), Account: stringParam(params, "account"),
		Project:       stringParam(params, "project"),
		Workspace:     stringParam(params, "workspace"),
		SizeProfileID: input.Deployment.SizeProfileID, ProviderParams: params,
		StorageGB: storage, TTLSeconds: issueTTL(input),
		BudgetUSD: input.Deployment.BudgetUSD,
	})
}

func issueTTL(input LiveExecutionIssueInput) int {
	if input.Deployment.LeaseExpiresAt == nil {
		return 0
	}
	return int(input.Deployment.LeaseExpiresAt.Sub(input.Now).Seconds())
}

func issueLiveEstimate(
	input LiveExecutionIssueInput,
	plan NormalizedLivePlan,
) (*IssuedLiveCostEstimate, error) {
	id, err := lifecycleOperationID("estimate")
	if err != nil {
		return nil, err
	}
	cost := EstimateAgentDBCost(CostEstimateRequest{
		Provider: plan.Provider, ProvisioningLevel: input.Deployment.ProvisioningLevel,
		SizeProfileID: plan.SizeProfileID, ProviderParams: plan.ProviderParams,
		StorageGB: plan.StorageGB, TTLSeconds: plan.TTLSeconds,
		BudgetUSD: plan.BudgetUSD,
	})
	return &IssuedLiveCostEstimate{
		EstimateID: id, PlanHash: plan.Hash, DeploymentID: plan.DeploymentID,
		Provider: plan.Provider, Region: plan.Region, Account: plan.Account,
		Project: plan.Project, Workspace: plan.Workspace,
		SizeProfileID: plan.SizeProfileID, ProviderParamsHash: plan.ProviderParamsHash,
		StorageGB: plan.StorageGB, TTLSeconds: plan.TTLSeconds,
		BudgetUSD: plan.BudgetUSD, EstimatedCostUSD: cost.TTLCostUSD,
		PricingRevision: input.PricingRevision, Confidence: cost.Confidence,
		UnknownComponents: append([]string(nil), cost.UnknownComponents...),
		ExpiresAt:         input.Now.Add(15 * time.Minute),
	}, nil
}

func issueLiveAuthorization(
	input LiveExecutionIssueInput,
	plan NormalizedLivePlan,
	estimate *IssuedLiveCostEstimate,
) (*LiveOperationAuthorization, error) {
	id, err := lifecycleOperationID("authorization")
	if err != nil {
		return nil, err
	}
	nonce, err := lifecycleOperationID("nonce")
	if err != nil {
		return nil, err
	}
	return &LiveOperationAuthorization{
		AuthorizationID: id, Allowed: true, DeploymentID: plan.DeploymentID,
		Provider: plan.Provider, Operation: plan.Operation, PlanHash: plan.Hash,
		EstimateID: estimate.EstimateID, RequesterID: input.RequesterID,
		ReviewerID: input.ReviewerID, ExpiresAt: input.Now.Add(10 * time.Minute),
		Nonce: nonce, IdempotencyKey: input.IdempotencyKey,
		AutoIssued: input.AutoIssued,
	}, nil
}

func resolveIssuedLivePolicy(
	input LiveExecutionIssueInput,
	plan NormalizedLivePlan,
	estimate *IssuedLiveCostEstimate,
	authz *LiveOperationAuthorization,
) EffectiveLivePolicyResult {
	return ResolveEffectiveLiveProvisionPolicy(EffectiveLivePolicyInput{
		Now: input.Now, Runtime: input.Runtime, Global: input.Global,
		Provider: input.Provider, Authorization: authz, Operation: plan.Operation,
		DeploymentID: plan.DeploymentID, PlanHash: plan.Hash,
		Request: LiveProvisionRequest{
			Provider: plan.Provider, Region: plan.Region, Account: plan.Account,
			Project: plan.Project, Workspace: plan.Workspace,
			TTLSeconds: plan.TTLSeconds, PublicIP: plan.PublicIP,
			EstimatedCostUSD: estimate.EstimatedCostUSD,
			EstimatedCostDoubled: estimate.Confidence != EstimateConfidenceHigh ||
				len(estimate.UnknownComponents) > 0,
			Approved: input.ReviewerID != "",
		},
	})
}

func normalizedIssueTime(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func LivePolicyGeneration(policyHash string) int64 {
	raw, err := hex.DecodeString(policyHash)
	if err != nil || len(raw) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(raw[:8]) & 0x7fffffffffffffff)
}
