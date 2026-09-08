package agentdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const (
	EstimateConfidenceHigh = "high"
	EstimateConfidenceLow  = "low"
)

type LivePlanInput struct {
	DeploymentID   string
	Provider       string
	Operation      ProvisionOperation
	Region         string
	Account        string
	Project        string
	Workspace      string
	SizeProfileID  string
	ProviderParams map[string]any
	PublicIP       bool
	StorageGB      float64
	TTLSeconds     int
	BudgetUSD      float64
}

type NormalizedLivePlan struct {
	DeploymentID       string             `json:"deployment_id"`
	Provider           string             `json:"provider"`
	Operation          ProvisionOperation `json:"operation"`
	Region             string             `json:"region"`
	Account            string             `json:"account,omitempty"`
	Project            string             `json:"project,omitempty"`
	Workspace          string             `json:"workspace,omitempty"`
	SizeProfileID      string             `json:"size_profile_id"`
	ProviderParams     map[string]any     `json:"provider_params"`
	ProviderParamsHash string             `json:"provider_params_hash"`
	PublicIP           bool               `json:"public_ip"`
	StorageGB          float64            `json:"storage_gb"`
	TTLSeconds         int                `json:"ttl_seconds"`
	BudgetUSD          float64            `json:"budget_usd"`
	Hash               string             `json:"hash"`
}

func (p NormalizedLivePlan) Input() LivePlanInput {
	return LivePlanInput{
		DeploymentID: p.DeploymentID, Provider: p.Provider, Operation: p.Operation,
		Region: p.Region, Account: p.Account, Project: p.Project, Workspace: p.Workspace,
		SizeProfileID: p.SizeProfileID, ProviderParams: cloneAnyMap(p.ProviderParams),
		PublicIP: p.PublicIP, StorageGB: p.StorageGB,
		TTLSeconds: p.TTLSeconds, BudgetUSD: p.BudgetUSD,
	}
}

func BuildNormalizedLivePlan(input LivePlanInput) (NormalizedLivePlan, error) {
	input.DeploymentID = strings.TrimSpace(input.DeploymentID)
	input.Provider = normalizeProvider(input.Provider)
	input.Region = strings.ToLower(strings.TrimSpace(input.Region))
	input.Account = strings.TrimSpace(input.Account)
	input.Project = strings.TrimSpace(input.Project)
	input.Workspace = strings.TrimSpace(input.Workspace)
	input.SizeProfileID = strings.TrimSpace(input.SizeProfileID)
	if input.Provider == ProviderNeon || input.Provider == ProviderSupabase {
		input.PublicIP = true
		input.Account = firstNonEmpty(input.Account, stringParam(input.ProviderParams, "organization"))
		input.Project = firstNonEmpty(input.Project, stringParam(input.ProviderParams, "project"))
	}
	if input.DeploymentID == "" || !cloudProvider(input.Provider) ||
		!validProvider(input.Provider) || input.Operation == "" ||
		input.Region == "" || input.SizeProfileID == "" ||
		input.TTLSeconds <= 0 || input.StorageGB <= 0 || input.BudgetUSD <= 0 {
		return NormalizedLivePlan{}, ErrInvalid
	}
	params, err := cloneAnyMapStrict(input.ProviderParams)
	if err != nil {
		return NormalizedLivePlan{}, ErrInvalid
	}
	paramsHash, err := hashJSON(params)
	if err != nil {
		return NormalizedLivePlan{}, ErrInvalid
	}
	plan := NormalizedLivePlan{
		DeploymentID: input.DeploymentID, Provider: input.Provider,
		Operation: input.Operation, Region: input.Region,
		Account: input.Account, Project: input.Project, Workspace: input.Workspace,
		SizeProfileID: input.SizeProfileID, ProviderParams: params,
		ProviderParamsHash: paramsHash,
		PublicIP:           input.PublicIP || providerParamsRequestPublicIP(params),
		StorageGB:          input.StorageGB,
		TTLSeconds:         input.TTLSeconds, BudgetUSD: input.BudgetUSD,
	}
	hash, err := hashNormalizedPlan(plan)
	if err != nil {
		return NormalizedLivePlan{}, ErrInvalid
	}
	plan.Hash = hash
	return plan, nil
}

type IssuedLiveCostEstimate struct {
	EstimateID         string     `json:"estimate_id"`
	PlanHash           string     `json:"plan_hash"`
	DeploymentID       string     `json:"deployment_id"`
	Provider           string     `json:"provider"`
	Region             string     `json:"region"`
	Account            string     `json:"account,omitempty"`
	Project            string     `json:"project,omitempty"`
	Workspace          string     `json:"workspace,omitempty"`
	SizeProfileID      string     `json:"size_profile_id"`
	ProviderParamsHash string     `json:"provider_params_hash"`
	StorageGB          float64    `json:"storage_gb"`
	TTLSeconds         int        `json:"ttl_seconds"`
	BudgetUSD          float64    `json:"budget_usd"`
	EstimatedCostUSD   float64    `json:"estimated_cost_usd"`
	PricingRevision    string     `json:"pricing_revision"`
	Confidence         string     `json:"confidence"`
	UnknownComponents  []string   `json:"unknown_components,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at"`
	SupersededAt       *time.Time `json:"superseded_at,omitempty"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
}

type LiveOperationAuthorization struct {
	AuthorizationID string             `json:"authorization_id"`
	Allowed         bool               `json:"allowed"`
	DeploymentID    string             `json:"deployment_id"`
	Provider        string             `json:"provider"`
	Operation       ProvisionOperation `json:"operation"`
	PlanHash        string             `json:"plan_hash"`
	EstimateID      string             `json:"estimate_id"`
	PolicyHash      string             `json:"policy_hash"`
	PolicyVersion   int64              `json:"policy_version"`
	RequesterID     string             `json:"requester_id"`
	ReviewerID      string             `json:"reviewer_id,omitempty"`
	ExpiresAt       time.Time          `json:"expires_at"`
	RevokedAt       *time.Time         `json:"revoked_at,omitempty"`
	ConsumedAt      *time.Time         `json:"consumed_at,omitempty"`
	Nonce           string             `json:"nonce"`
	IdempotencyKey  string             `json:"idempotency_key"`
	AutoIssued      bool               `json:"auto_issued"`
}

type LiveExecutionClientClaims struct {
	Approved            bool
	EstimatedCostUSD    float64
	CostEstimateID      string
	ActorID             string
	AdminOverrideReason string
	Policy              LiveProvisionPolicy
}

type LiveExecutionAttempt struct {
	DeploymentID             string
	Provider                 string
	Operation                ProvisionOperation
	PlanHash                 string
	EstimateID               string
	AuthorizationID          string
	AuthenticatedRequesterID string
	IdempotencyKey           string
	ClientClaims             LiveExecutionClientClaims
}

type LiveExecutionReceipt struct {
	AuthorizationID    string
	IdempotencyKey     string
	PlanHash           string
	ProviderResourceID string
}

type LiveExecutionRecords struct {
	Plan                   *NormalizedLivePlan
	Estimate               *IssuedLiveCostEstimate
	Authorization          *LiveOperationAuthorization
	CurrentPolicy          LiveProvisionPolicy
	CurrentPolicyHash      string
	CurrentPolicyVersion   int64
	CurrentPricingRevision string
	Receipt                *LiveExecutionReceipt
}

type LiveExecutionValidationResult struct {
	Allowed         bool
	Replay          bool
	Receipt         *LiveExecutionReceipt
	DisabledReasons []string
}

func ValidateLiveExecutionAttempt(
	records LiveExecutionRecords,
	attempt LiveExecutionAttempt,
	now time.Time,
) LiveExecutionValidationResult {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reasons := make([]string, 0, 12)
	validateLivePlanRecord(records.Plan, attempt, &reasons)
	validateLiveEstimate(records, attempt, now, &reasons)
	validateLiveAuthorization(records, attempt, now, &reasons)
	if consumedLiveRecords(records) {
		if receiptMatchesAttempt(records, attempt) {
			receipt := *records.Receipt
			return LiveExecutionValidationResult{Replay: true, Receipt: &receipt}
		}
		reasons = append(reasons, "estimate or authorization was already consumed")
	}
	if records.Plan != nil && records.Estimate != nil {
		validateCurrentLivePolicy(records, &reasons)
	}
	return LiveExecutionValidationResult{
		Allowed:         len(reasons) == 0,
		DisabledReasons: uniqueStrings(reasons),
	}
}

func validateLivePlanRecord(
	plan *NormalizedLivePlan,
	attempt LiveExecutionAttempt,
	reasons *[]string,
) {
	if plan == nil {
		*reasons = append(*reasons, "server-owned plan is missing")
		return
	}
	paramsHash, paramsErr := hashJSON(plan.ProviderParams)
	planHash, planErr := hashNormalizedPlan(*plan)
	if paramsErr != nil || paramsHash != plan.ProviderParamsHash {
		*reasons = append(*reasons, "server-owned provider parameters failed integrity check")
	}
	if planErr != nil || planHash != plan.Hash {
		*reasons = append(*reasons, "server-owned plan failed integrity check")
	}
	if attempt.PlanHash != plan.Hash {
		*reasons = append(*reasons, "execution plan hash does not match")
	}
	if attempt.DeploymentID != plan.DeploymentID {
		*reasons = append(*reasons, "execution deployment does not match plan")
	}
	if attempt.Provider != plan.Provider {
		*reasons = append(*reasons, "execution provider does not match plan")
	}
	if attempt.Operation != plan.Operation {
		*reasons = append(*reasons, "execution operation does not match plan")
	}
}

func validateLiveEstimate(
	records LiveExecutionRecords,
	attempt LiveExecutionAttempt,
	now time.Time,
	reasons *[]string,
) {
	estimate := records.Estimate
	if estimate == nil {
		*reasons = append(*reasons, "server-issued estimate is missing")
		return
	}
	if strings.TrimSpace(estimate.EstimateID) == "" || estimate.EstimatedCostUSD <= 0 {
		*reasons = append(*reasons, "cost estimate identity or amount is invalid")
	}
	if estimate.ExpiresAt.IsZero() || !estimate.ExpiresAt.After(now) {
		*reasons = append(*reasons, "cost estimate is expired")
	}
	if estimate.SupersededAt != nil {
		*reasons = append(*reasons, "cost estimate is superseded")
	}
	if estimate.EstimateID != attempt.EstimateID {
		*reasons = append(*reasons, "estimate id does not match execution attempt")
	}
	if estimate.PricingRevision == "" ||
		estimate.PricingRevision != records.CurrentPricingRevision {
		*reasons = append(*reasons, "pricing revision does not match")
	}
	if records.Plan != nil {
		validateEstimatePlanTuple(estimate, records.Plan, reasons)
	}
	if records.CurrentPolicy.ExecutionMode == LiveModeAutoWithinPolicy &&
		estimate.Confidence != EstimateConfidenceHigh {
		*reasons = append(*reasons, "estimate confidence is insufficient for auto mode")
	}
	if records.CurrentPolicy.ExecutionMode == LiveModeAutoWithinPolicy &&
		len(estimate.UnknownComponents) > 0 {
		*reasons = append(*reasons, "estimate has unknown cost components")
	}
}

func validateEstimatePlanTuple(
	estimate *IssuedLiveCostEstimate,
	plan *NormalizedLivePlan,
	reasons *[]string,
) {
	checks := []struct {
		ok     bool
		reason string
	}{
		{estimate.PlanHash == plan.Hash, "estimate plan hash does not match"},
		{estimate.DeploymentID == plan.DeploymentID, "estimate deployment does not match"},
		{estimate.Provider == plan.Provider, "estimate provider does not match"},
		{estimate.Region == plan.Region, "estimate region does not match"},
		{estimate.Account == plan.Account, "estimate account does not match"},
		{estimate.Project == plan.Project, "estimate project does not match"},
		{estimate.Workspace == plan.Workspace, "estimate workspace does not match"},
		{estimate.SizeProfileID == plan.SizeProfileID, "estimate profile does not match"},
		{estimate.ProviderParamsHash == plan.ProviderParamsHash,
			"estimate provider parameters do not match"},
		{estimate.StorageGB == plan.StorageGB, "estimate storage does not match"},
		{estimate.TTLSeconds == plan.TTLSeconds, "estimate ttl does not match"},
		{estimate.BudgetUSD == plan.BudgetUSD, "estimate budget does not match"},
	}
	for _, check := range checks {
		if !check.ok {
			*reasons = append(*reasons, check.reason)
		}
	}
}

func validateLiveAuthorization(
	records LiveExecutionRecords,
	attempt LiveExecutionAttempt,
	now time.Time,
	reasons *[]string,
) {
	authz := records.Authorization
	if authz == nil {
		*reasons = append(*reasons, "server-issued authorization is missing")
		return
	}
	if strings.TrimSpace(authz.AuthorizationID) == "" ||
		strings.TrimSpace(authz.Nonce) == "" ||
		strings.TrimSpace(authz.IdempotencyKey) == "" {
		*reasons = append(*reasons, "authorization identity nonce or idempotency is invalid")
	}
	if !authz.Allowed {
		*reasons = append(*reasons, "authorization denied execution")
	}
	if authz.ExpiresAt.IsZero() || !authz.ExpiresAt.After(now) {
		*reasons = append(*reasons, "authorization is expired")
	}
	if authz.RevokedAt != nil {
		*reasons = append(*reasons, "authorization is revoked")
	}
	if authz.AuthorizationID != attempt.AuthorizationID {
		*reasons = append(*reasons, "authorization id does not match")
	}
	validateAuthorizationTuple(records, attempt, authz, reasons)
	if records.CurrentPolicy.ExecutionMode == LiveModeApproval &&
		!authz.AutoIssued && strings.TrimSpace(authz.ReviewerID) == "" {
		*reasons = append(*reasons, "authenticated reviewer is missing")
	}
}

func validateAuthorizationTuple(
	records LiveExecutionRecords,
	attempt LiveExecutionAttempt,
	authz *LiveOperationAuthorization,
	reasons *[]string,
) {
	plan := records.Plan
	if plan == nil {
		return
	}
	checks := []struct {
		ok     bool
		reason string
	}{
		{authz.PlanHash == plan.Hash, "authorization plan does not match"},
		{records.Estimate != nil && authz.EstimateID == records.Estimate.EstimateID,
			"authorization estimate does not match"},
		{authz.PolicyHash == records.CurrentPolicyHash, "authorization policy does not match"},
		{authz.PolicyVersion == records.CurrentPolicyVersion,
			"authorization policy generation does not match"},
		{authz.DeploymentID == plan.DeploymentID, "authorization deployment does not match"},
		{authz.Provider == plan.Provider, "authorization provider does not match"},
		{authz.Operation == plan.Operation, "authorization operation does not match"},
		{authz.RequesterID == attempt.AuthenticatedRequesterID,
			"authorization requester does not match"},
		{authz.IdempotencyKey == attempt.IdempotencyKey,
			"authorization idempotency key does not match"},
	}
	for _, check := range checks {
		if !check.ok {
			*reasons = append(*reasons, check.reason)
		}
	}
}

func validateCurrentLivePolicy(
	records LiveExecutionRecords,
	reasons *[]string,
) {
	plan := records.Plan
	estimate := records.Estimate
	if records.CurrentPolicy.ExecutionMode == LiveModeManual {
		*reasons = append(*reasons, "manual mode disables live provider mutation")
	}
	decision := EvaluateLiveProvisionPolicy(records.CurrentPolicy, LiveProvisionRequest{
		Provider: plan.Provider, Region: plan.Region, Account: plan.Account,
		Project: plan.Project, Workspace: plan.Workspace,
		TTLSeconds: plan.TTLSeconds, PublicIP: plan.PublicIP,
		EstimatedCostUSD: estimate.EstimatedCostUSD,
	})
	*reasons = append(*reasons, decision.DisabledReasons...)
}

func providerParamsRequestPublicIP(params map[string]any) bool {
	for _, key := range []string{"publicly_accessible", "ipv4_enabled", "public_ip"} {
		if enabled, ok := params[key].(bool); ok && enabled {
			return true
		}
	}
	return false
}

func consumedLiveRecords(records LiveExecutionRecords) bool {
	return records.Estimate != nil && records.Estimate.ConsumedAt != nil ||
		records.Authorization != nil && records.Authorization.ConsumedAt != nil
}

func receiptMatchesAttempt(
	records LiveExecutionRecords,
	attempt LiveExecutionAttempt,
) bool {
	return records.Receipt != nil && records.Authorization != nil && records.Plan != nil &&
		records.Receipt.AuthorizationID == records.Authorization.AuthorizationID &&
		records.Receipt.IdempotencyKey == attempt.IdempotencyKey &&
		records.Receipt.PlanHash == records.Plan.Hash
}

func hashNormalizedPlan(plan NormalizedLivePlan) (string, error) {
	plan.Hash = ""
	return hashJSON(plan)
}

func hashJSON(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func cloneAnyMap(value map[string]any) map[string]any {
	out, err := cloneAnyMapStrict(value)
	if err != nil {
		return map[string]any{}
	}
	return out
}

func cloneAnyMapStrict(value map[string]any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
