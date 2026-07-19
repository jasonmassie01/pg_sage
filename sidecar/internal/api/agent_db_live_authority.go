package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/config"
)

const (
	agentDBPricingRevision = "static-pricing-v1"
	providerPolicyTTL      = 30 * 24 * time.Hour
)

type agentDBLiveAuthority struct {
	config   config.AgentDBConfig
	loadedAt time.Time
}

type agentDBLiveAuthorizationResponse struct {
	PlanHash        string                         `json:"plan_hash"`
	EstimateID      string                         `json:"estimate_id"`
	AuthorizationID string                         `json:"authorization_id"`
	IdempotencyKey  string                         `json:"idempotency_key"`
	Plan            agentdb.NormalizedLivePlan     `json:"plan"`
	Estimate        agentdb.IssuedLiveCostEstimate `json:"estimate"`
}

func newAgentDBLiveAuthority(cfg config.AgentDBConfig) *agentDBLiveAuthority {
	return &agentDBLiveAuthority{config: cfg, loadedAt: time.Now().UTC()}
}

func agentDBAuthorizeLiveHandler(
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readMap(r)
		operation, err := liveOperation(str(body, "operation"))
		if err != nil {
			agentDBError(w, err)
			return
		}
		requester := liveRequesterID(r)
		key := firstString(r.Header.Get("Idempotency-Key"), str(body, "idempotency_key"))
		if requester == "" || key == "" || authority == nil {
			agentDBError(w, agentdb.ErrInvalid)
			return
		}
		response, err := issueOrReplayLiveAuthorization(
			r.Context(), st, registry, authority, agentDBID(r),
			operation, requester, key,
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, response)
	}
}

func issueOrReplayLiveAuthorization(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	deploymentID string,
	operation agentdb.ProvisionOperation,
	requesterID string,
	idempotencyKey string,
) (agentDBLiveAuthorizationResponse, error) {
	existing, err := st.FindLiveExecutionAttempt(
		ctx, deploymentID, operation, requesterID, idempotencyKey,
	)
	if err == nil {
		records, loadErr := currentLiveExecutionRecords(
			ctx, st, registry, authority, existing,
		)
		if loadErr != nil {
			return agentDBLiveAuthorizationResponse{}, loadErr
		}
		return liveAuthorizationResponse(records), nil
	}
	if !errors.Is(err, agentdb.ErrNotFound) {
		return agentDBLiveAuthorizationResponse{}, err
	}
	return issueNewLiveAuthorization(
		ctx, st, registry, authority, deploymentID,
		operation, requesterID, idempotencyKey,
	)
}

func issueNewLiveAuthorization(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	deploymentID string,
	operation agentdb.ProvisionOperation,
	requesterID string,
	idempotencyKey string,
) (agentDBLiveAuthorizationResponse, error) {
	dep, err := st.Get(ctx, deploymentID)
	if err != nil {
		return agentDBLiveAuthorizationResponse{}, err
	}
	if _, err := liveProviderRunner(registry, dep.Provider); err != nil {
		return agentDBLiveAuthorizationResponse{}, err
	}
	runtime, global, provider, err := authority.livePolicySources(
		ctx, st, registry, dep.Provider,
	)
	if err != nil {
		return agentDBLiveAuthorizationResponse{}, err
	}
	if dep.BudgetUSD <= 0 {
		dep.BudgetUSD = liveBudgetCeiling(global.Policy, provider.Policy)
	}
	records, err := agentdb.IssueLiveExecutionRecords(agentdb.LiveExecutionIssueInput{
		Deployment: dep, Operation: operation, RequesterID: requesterID,
		ReviewerID: requesterID, IdempotencyKey: idempotencyKey,
		Runtime: runtime, Global: global, Provider: provider,
		PricingRevision: agentDBPricingRevision, Now: time.Now().UTC(),
	})
	if err != nil {
		return agentDBLiveAuthorizationResponse{}, err
	}
	if operation == agentdb.ProvisionOpDestroy {
		if err := st.ValidateLiveDestroyPrerequisites(
			ctx, dep, records.CurrentPolicy,
		); err != nil {
			return agentDBLiveAuthorizationResponse{}, err
		}
	}
	if err := st.PersistLiveExecutionRecords(ctx, records); err != nil {
		return agentDBLiveAuthorizationResponse{}, err
	}
	return liveAuthorizationResponse(records), nil
}

func liveBudgetCeiling(
	global agentdb.LiveProvisionPolicy,
	provider agentdb.LiveProvisionPolicy,
) float64 {
	if global.MaxEstimatedCostUSD <= 0 || provider.MaxEstimatedCostUSD <= 0 {
		return 0
	}
	if global.MaxEstimatedCostUSD < provider.MaxEstimatedCostUSD {
		return global.MaxEstimatedCostUSD
	}
	return provider.MaxEstimatedCostUSD
}

func (a *agentDBLiveAuthority) livePolicySources(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	providerName string,
) (*agentdb.RuntimeLiveCapability, *agentdb.LivePolicyLayer, *agentdb.LivePolicyLayer, error) {
	if a == nil {
		return nil, nil, nil, agentdb.ErrInvalid
	}
	persisted, err := st.ProviderConfig(ctx, providerName)
	if err != nil {
		return nil, nil, nil, err
	}
	persistedPolicy := livePolicyFromProviderConfig(providerName, persisted)
	globalPolicy := a.globalLivePolicy(providerName, persistedPolicy.ExecutionMode)
	runner, runnerErr := registry.ForProvider(providerName)
	runnerAvailable := runnerErr == nil && runner != nil && runner.Name() != "dry_run"
	runtime := &agentdb.RuntimeLiveCapability{
		Enabled:         a.config.LiveProvisioningEnabled,
		RunnerAvailable: runnerAvailable, Policy: globalPolicy,
	}
	global := &agentdb.LivePolicyLayer{
		Policy: globalPolicy, Version: a.loadedAt.UnixNano(),
		ValidatedAt: a.loadedAt,
	}
	provider := &agentdb.LivePolicyLayer{
		Policy: persistedPolicy, Version: persisted.UpdatedAt.UnixNano(),
		ValidatedAt: persisted.UpdatedAt, ValidationTTL: providerPolicyTTL,
	}
	return runtime, global, provider, nil
}

func (a *agentDBLiveAuthority) globalLivePolicy(
	provider string,
	executionMode string,
) agentdb.LiveProvisionPolicy {
	p, ok := a.config.Providers[provider]
	return agentdb.LiveProvisionPolicy{
		LiveProvisioningEnabled: a.config.LiveProvisioningEnabled,
		ProviderEnabled:         ok && p.Enabled, Provider: provider,
		AllowedRegions:          append([]string(nil), p.AllowedRegions...),
		AllowedAccounts:         append([]string(nil), p.AllowedAccounts...),
		AllowedProjects:         append([]string(nil), p.AllowedProjects...),
		AllowedWorkspaces:       append([]string(nil), p.AllowedWorkspaces...),
		AllowPublicIP:           a.config.AllowPublicIP,
		RequireBackupBeforeDrop: a.config.RequireBackupBeforeDrop,
		MaxTTLSeconds:           p.MaxTTLSeconds, MaxEstimatedCostUSD: p.MaxCostUSD,
		ExecutionMode: executionMode,
	}
}

func currentLiveExecutionRecords(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	attempt agentdb.LiveExecutionAttempt,
) (agentdb.LiveExecutionRecords, error) {
	loaded, err := st.LoadLiveExecutionRecords(
		ctx, attempt, agentdb.LiveExecutionRecords{},
	)
	if err != nil {
		return loaded, err
	}
	runtime, global, provider, err := authority.livePolicySources(
		ctx, st, registry, attempt.Provider,
	)
	if err != nil {
		return loaded, err
	}
	resolved := agentdb.ResolveEffectiveLiveProvisionPolicy(
		effectivePolicyInputForRecords(runtime, global, provider, loaded),
	)
	loaded.CurrentPolicy = resolved.Policy
	loaded.CurrentPolicyHash = resolved.PolicyHash
	loaded.CurrentPolicyVersion = agentdb.LivePolicyGeneration(resolved.PolicyHash)
	loaded.CurrentPricingRevision = agentDBPricingRevision
	validation := agentdb.ValidateLiveExecutionAttempt(loaded, attempt, time.Now().UTC())
	if !validation.Allowed && !validation.Replay {
		return loaded, agentdb.ErrInvalid
	}
	return loaded, nil
}

func executeAuthorizedLiveCreate(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	dep agentdb.Deployment,
	body map[string]any,
	r *http.Request,
) (any, error) {
	canonical, records, err := authorizedLiveRequest(
		ctx, st, registry, authority, dep, agentdb.ProvisionOpCreate, body, r,
	)
	if err != nil {
		return nil, err
	}
	if records.Receipt != nil {
		return records.Receipt, nil
	}
	runner, err := liveProviderRunner(registry, dep.Provider)
	if err != nil {
		return nil, err
	}
	return st.ExecuteProvisionLive(ctx, dep.DeploymentID, runner,
		agentdb.LiveExecutionRequest{
			Mode: "live", Records: &records, Attempt: &canonical,
			Now: time.Now().UTC(),
		},
	)
}

func executeAuthorizedLiveDestroy(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	dep agentdb.Deployment,
	body map[string]any,
	r *http.Request,
) (any, error) {
	canonical, records, err := authorizedLiveRequest(
		ctx, st, registry, authority, dep, agentdb.ProvisionOpDestroy, body, r,
	)
	if err != nil {
		return nil, err
	}
	if records.Receipt != nil {
		return records.Receipt, nil
	}
	runner, err := liveProviderRunner(registry, dep.Provider)
	if err != nil {
		return nil, err
	}
	return st.ExecuteDestroyProvisionLive(ctx, dep.DeploymentID, runner,
		agentdb.LiveExecutionRequest{
			Mode: "live", Records: &records, Attempt: &canonical,
			Now: time.Now().UTC(),
		},
	)
}

func authorizedLiveRequest(
	ctx context.Context,
	st *agentdb.Store,
	registry *agentdb.RunnerRegistry,
	authority *agentDBLiveAuthority,
	dep agentdb.Deployment,
	operation agentdb.ProvisionOperation,
	body map[string]any,
	r *http.Request,
) (agentdb.LiveExecutionAttempt, agentdb.LiveExecutionRecords, error) {
	requested := liveAttemptFromRequest(dep, operation, body, r)
	if authority == nil || !completeLiveAttempt(requested) {
		return requested, agentdb.LiveExecutionRecords{}, agentdb.ErrInvalid
	}
	canonical, err := st.LiveExecutionAttemptByAuthorization(
		ctx, requested.AuthorizationID,
	)
	if err != nil {
		if errors.Is(err, agentdb.ErrNotFound) {
			err = agentdb.ErrConflict
		}
		return requested, agentdb.LiveExecutionRecords{}, err
	}
	if canonical.AuthenticatedRequesterID != requested.AuthenticatedRequesterID {
		return requested, agentdb.LiveExecutionRecords{}, agentdb.ErrInvalid
	}
	if !sameLiveAttempt(canonical, requested) {
		return requested, agentdb.LiveExecutionRecords{}, agentdb.ErrConflict
	}
	records, err := currentLiveExecutionRecords(
		ctx, st, registry, authority, canonical,
	)
	return canonical, records, err
}

func liveAttemptFromRequest(
	dep agentdb.Deployment,
	operation agentdb.ProvisionOperation,
	body map[string]any,
	r *http.Request,
) agentdb.LiveExecutionAttempt {
	return agentdb.LiveExecutionAttempt{
		DeploymentID: dep.DeploymentID, Provider: dep.Provider,
		Operation: operation, PlanHash: str(body, "plan_hash"),
		EstimateID:               str(body, "estimate_id"),
		AuthorizationID:          str(body, "authorization_id"),
		AuthenticatedRequesterID: liveRequesterID(r),
		IdempotencyKey: firstString(
			r.Header.Get("Idempotency-Key"), str(body, "idempotency_key"),
		),
	}
}

func completeLiveAttempt(attempt agentdb.LiveExecutionAttempt) bool {
	return attempt.DeploymentID != "" && attempt.Provider != "" &&
		attempt.Operation != "" && attempt.PlanHash != "" &&
		attempt.EstimateID != "" && attempt.AuthorizationID != "" &&
		attempt.AuthenticatedRequesterID != "" && attempt.IdempotencyKey != ""
}

func sameLiveAttempt(
	left agentdb.LiveExecutionAttempt,
	right agentdb.LiveExecutionAttempt,
) bool {
	return left.DeploymentID == right.DeploymentID &&
		left.Provider == right.Provider && left.Operation == right.Operation &&
		left.PlanHash == right.PlanHash && left.EstimateID == right.EstimateID &&
		left.AuthorizationID == right.AuthorizationID &&
		left.AuthenticatedRequesterID == right.AuthenticatedRequesterID &&
		left.IdempotencyKey == right.IdempotencyKey
}

func liveProviderRunner(
	registry *agentdb.RunnerRegistry,
	provider string,
) (agentdb.ProviderRunner, error) {
	runner, err := registry.ForProvider(provider)
	if err != nil {
		return nil, err
	}
	if runner == nil || runner.Name() == "dry_run" {
		return nil, agentdb.ErrRunnerUnavailable
	}
	return runner, nil
}

func effectivePolicyInputForRecords(
	runtime *agentdb.RuntimeLiveCapability,
	global *agentdb.LivePolicyLayer,
	provider *agentdb.LivePolicyLayer,
	records agentdb.LiveExecutionRecords,
) agentdb.EffectiveLivePolicyInput {
	plan := records.Plan
	estimate := records.Estimate
	return agentdb.EffectiveLivePolicyInput{
		Now: time.Now().UTC(), Runtime: runtime, Global: global, Provider: provider,
		Authorization: records.Authorization, Operation: plan.Operation,
		DeploymentID: plan.DeploymentID, PlanHash: plan.Hash,
		Request: agentdb.LiveProvisionRequest{
			Provider: plan.Provider, Region: plan.Region, Account: plan.Account,
			Project: plan.Project, Workspace: plan.Workspace,
			TTLSeconds: plan.TTLSeconds, PublicIP: plan.PublicIP,
			EstimatedCostUSD: estimate.EstimatedCostUSD,
			EstimatedCostDoubled: estimate.Confidence != agentdb.EstimateConfidenceHigh ||
				len(estimate.UnknownComponents) > 0,
			Approved: records.Authorization.ReviewerID != "",
		},
	}
}

func liveAuthorizationResponse(
	records agentdb.LiveExecutionRecords,
) agentDBLiveAuthorizationResponse {
	return agentDBLiveAuthorizationResponse{
		PlanHash: records.Plan.Hash, EstimateID: records.Estimate.EstimateID,
		AuthorizationID: records.Authorization.AuthorizationID,
		IdempotencyKey:  records.Authorization.IdempotencyKey,
		Plan:            *records.Plan, Estimate: *records.Estimate,
	}
}

func liveOperation(value string) (agentdb.ProvisionOperation, error) {
	switch value {
	case string(agentdb.ProvisionOpCreate):
		return agentdb.ProvisionOpCreate, nil
	case string(agentdb.ProvisionOpDestroy):
		return agentdb.ProvisionOpDestroy, nil
	default:
		return "", agentdb.ErrInvalid
	}
}

func liveRequesterID(r *http.Request) string {
	user := UserFromContext(r.Context())
	if user == nil || user.ID <= 0 {
		return ""
	}
	return "user:" + strconv.Itoa(user.ID)
}
