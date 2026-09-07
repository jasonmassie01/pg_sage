package api

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
)

type wave34AuthorizationResponse struct {
	PlanHash        string                     `json:"plan_hash"`
	EstimateID      string                     `json:"estimate_id"`
	AuthorizationID string                     `json:"authorization_id"`
	IdempotencyKey  string                     `json:"idempotency_key"`
	Plan            agentdb.NormalizedLivePlan `json:"plan"`
	Estimate        struct {
		EstimateID       string  `json:"estimate_id"`
		EstimatedCostUSD float64 `json:"estimated_cost_usd"`
		PricingRevision  string  `json:"pricing_revision"`
		Confidence       string  `json:"confidence"`
	} `json:"estimate"`
}

type wave34AuthorityFixture struct {
	id      string
	profile string
	store   *agentdb.Store
	ctx     context.Context
	pool    *pgxpool.Pool
	handler http.Handler
	runner  *wave34AuthorityRunner
}

type wave34AuthorityRunner struct {
	createCalls  atomic.Int64
	destroyCalls atomic.Int64
}

func (*wave34AuthorityRunner) Name() string { return "wave34_authority_runner" }

func (*wave34AuthorityRunner) Provider() string { return agentdb.ProviderAWSRDS }

func (*wave34AuthorityRunner) Preflight(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "preflight_passed"}
}

func (r *wave34AuthorityRunner) Create(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	r.createCalls.Add(1)
	return agentdb.ProvisionResult{
		Status: "available", ProviderResourceID: "wave34-live-resource",
		Detail: map[string]any{"provider_resource_id": "wave34-live-resource"},
	}
}

func (*wave34AuthorityRunner) Status(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "available"}
}

func (r *wave34AuthorityRunner) Destroy(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	r.destroyCalls.Add(1)
	return agentdb.ProvisionResult{Status: "destroying"}
}

func (*wave34AuthorityRunner) BackupCheck(
	context.Context,
	agentdb.ProvisionRequest,
) agentdb.ProvisionResult {
	return agentdb.ProvisionResult{Status: "verified"}
}
