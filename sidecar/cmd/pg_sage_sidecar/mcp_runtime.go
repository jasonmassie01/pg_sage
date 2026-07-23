package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
)

var mcpRuntime *mcp.Runtime

func startMCPRuntime() {
	if cfg == nil || !cfg.MCP.Enabled {
		return
	}
	access := &fleetMCPAccess{manager: fleetMgr, fallback: pool}
	standingGate := &fleetStandingPolicyGate{manager: fleetMgr}
	intentExecutor := &fleetIntentExecutor{
		access: access, gate: standingGate, cloneConfig: cfg.Clone,
		migrationFactory: configuredMCPMigrationRuntime,
	}
	backend, err := mcp.NewProductionBackend(mcp.ProductionDependencies{
		Gate:       standingGate,
		Planner:    mcp.DeterministicIntentPlanner{},
		Executor:   intentExecutor,
		Policy:     access,
		Ledger:     access,
		Value:      access,
		Guarantees: access,
	})
	if err != nil {
		logError("mcp", "production backend: %v", err)
		return
	}
	runtime, err := mcp.NewRuntime(
		cfg.MCP, mcp.NewServer(backend), os.Stdin, os.Stdout,
	)
	if err != nil {
		logError("mcp", "runtime: %v", err)
		return
	}
	mcpRuntime = runtime
	if runtime == nil || runtime.Transport() != "stdio" {
		return
	}
	go func() {
		if err := runtime.Serve(shutdownCtx); err != nil && err != context.Canceled {
			logError("mcp", "stdio server: %v", err)
		}
	}()
}

func mcpHTTPHandler() http.Handler {
	if mcpRuntime == nil || mcpRuntime.Transport() != "http" {
		return nil
	}
	return mcpRuntime.HTTPHandler()
}

type fleetStandingPolicyGate struct {
	manager *fleet.DatabaseManager
}

func (gate *fleetStandingPolicyGate) Authorize(
	ctx context.Context, request policy.ActionRequest,
) policy.Decision {
	instance := mcpInstance(gate.manager, request.DatabaseID)
	if instance == nil || instance.Executor == nil {
		return unavailablePolicyDecision("target database executor is unavailable")
	}
	standing := instance.Executor.StandingPolicyGate()
	if standing == nil {
		return unavailablePolicyDecision("target standing policy is unavailable")
	}
	return standing.Authorize(ctx, request)
}

func unavailablePolicyDecision(detail string) policy.Decision {
	return policy.Decision{
		Verdict: policy.VerdictBlocked,
		Reason:  policy.ReasonPolicyUnavailable,
		Detail:  detail,
	}
}

type fleetMCPAccess struct {
	manager  *fleet.DatabaseManager
	fallback *pgxpool.Pool
}

func (access *fleetMCPAccess) GetPolicy(
	ctx context.Context, request mcp.PolicyRequest,
) (mcp.PolicyResult, error) {
	adapter, err := access.adapter(request.DatabaseID)
	if err != nil {
		return mcp.PolicyResult{}, err
	}
	return adapter.GetPolicy(ctx, request)
}

func (access *fleetMCPAccess) ProposePolicyChangeDryRun(
	ctx context.Context, request mcp.PolicyProposalRequest,
) (mcp.PolicyProposalResult, error) {
	adapter, err := access.adapter(request.DatabaseID)
	if err != nil {
		return mcp.PolicyProposalResult{}, err
	}
	return adapter.ProposePolicyChangeDryRun(ctx, request)
}

func (access *fleetMCPAccess) GetLedger(
	ctx context.Context, request mcp.LedgerRequest,
) (mcp.LedgerResult, error) {
	adapter, err := access.adapter(databaseIDFromLedgerFilter(request.Filter))
	if err != nil {
		return mcp.LedgerResult{}, err
	}
	return adapter.GetLedger(ctx, request)
}

func (access *fleetMCPAccess) GetValue(ctx context.Context) (map[string]any, error) {
	adapter, err := access.adapter(nil)
	if err != nil {
		return nil, err
	}
	return adapter.GetValue(ctx)
}

func (access *fleetMCPAccess) GetGuaranteeStatus(
	ctx context.Context,
) (mcp.GuaranteeStatus, error) {
	adapter, err := access.adapter(nil)
	if err != nil {
		return mcp.GuaranteeStatus{}, err
	}
	return adapter.GetGuaranteeStatus(ctx)
}

type fleetIntentExecutor struct {
	access           *fleetMCPAccess
	gate             policy.Gate
	cloneConfig      config.CloneProviderConfig
	migrationFactory mcpMigrationFactory
}

func (executor *fleetIntentExecutor) Execute(
	ctx context.Context, request policy.ActionRequest, decision policy.Decision,
) (any, error) {
	adapter, err := executor.access.adapter(request.DatabaseID)
	if err != nil {
		return nil, err
	}
	production := mcp.NewProductionIntentExecutor(adapter, plan.NewPlanner(), executor.gate)
	return production.Execute(ctx, request, decision)
}

func (executor *fleetIntentExecutor) ExecuteConcrete(
	ctx context.Context, request policy.ActionRequest,
) (any, error) {
	adapter, err := executor.access.adapter(request.DatabaseID)
	if err != nil {
		return nil, err
	}
	production := mcp.NewProductionIntentExecutor(adapter, plan.NewPlanner(), executor.gate)
	if request.Contract != nil && request.Contract.ActionType == "apply_migration" {
		factory := executor.migrationFactory
		if factory == nil {
			factory = configuredMCPMigrationRuntime
		}
		targetPool, poolErr := executor.access.targetPool(request.DatabaseID)
		if poolErr == nil {
			boundGate := databaseBoundMCPGate{
				delegate: executor.gate, databaseID: request.DatabaseID,
			}
			runtime, runtimeErr := factory(targetPool, boundGate, executor.cloneConfig)
			if runtimeErr == nil && runtime != nil {
				production.WithMigrationRuntime(runtime)
			}
		}
	}
	return production.ExecuteConcrete(ctx, request)
}

type databaseBoundMCPGate struct {
	delegate   policy.Gate
	databaseID *int64
}

func (gate databaseBoundMCPGate) Authorize(
	ctx context.Context, request policy.ActionRequest,
) policy.Decision {
	if request.DatabaseID == nil {
		request.DatabaseID = gate.databaseID
	}
	if gate.delegate == nil {
		return unavailablePolicyDecision("target standing policy is unavailable")
	}
	return gate.delegate.Authorize(ctx, request)
}

func (access *fleetMCPAccess) adapter(databaseID *int64) (*mcp.PostgresAccess, error) {
	pool, err := access.targetPool(databaseID)
	if err != nil {
		return nil, err
	}
	return mcp.NewPostgresAccess(pool), nil
}

func (access *fleetMCPAccess) targetPool(databaseID *int64) (*pgxpool.Pool, error) {
	instance := mcpInstance(access.manager, databaseID)
	if instance != nil && instance.Pool != nil {
		return instance.Pool, nil
	}
	if databaseID == nil && access.fallback != nil {
		return access.fallback, nil
	}
	return nil, fmt.Errorf("MCP target database is unavailable")
}

func mcpInstance(
	manager *fleet.DatabaseManager, databaseID *int64,
) *fleet.DatabaseInstance {
	if manager == nil {
		return nil
	}
	if databaseID != nil {
		return manager.GetInstanceByDatabaseID(int(*databaseID))
	}
	instances := manager.Instances()
	if len(instances) != 1 {
		return nil
	}
	for _, instance := range instances {
		return instance
	}
	return nil
}

func databaseIDFromLedgerFilter(raw json.RawMessage) *int64 {
	var filter struct {
		DatabaseID *int64 `json:"database_id"`
	}
	if json.Unmarshal(raw, &filter) != nil {
		return nil
	}
	return filter.DatabaseID
}
