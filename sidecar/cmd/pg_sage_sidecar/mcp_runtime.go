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
	"github.com/pg-sage/sidecar/internal/value"
)

var mcpRuntime *mcp.Runtime

func startMCPRuntime() {
	if cfg == nil || !cfg.MCP.Enabled {
		return
	}
	backend, err := mcp.NewProductionBackend(mcpDependencies())
	if err != nil {
		logError("mcp", "production backend: %v", err)
		return
	}
	server := mcp.NewServer(backend).WithDirectory(fleetMCPDirectory{manager: fleetMgr}).
		WithVersion(version)
	runtime, err := mcp.NewRuntime(cfg.MCP, server, os.Stdin, os.Stdout)
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

// mcpDependencies wires every MCP backend to the fleet. Every request goes
// through the standing policy gate of its database, never as an approval.
func mcpDependencies() mcp.ProductionDependencies {
	access := &fleetMCPAccess{manager: fleetMgr, fallback: pool}
	standingGate := mcp.NeverApproved(&fleetStandingPolicyGate{manager: fleetMgr})
	return mcp.ProductionDependencies{
		Gate:    standingGate,
		Planner: mcp.DeterministicIntentPlanner{},
		Executor: &fleetIntentExecutor{
			access: access, gate: standingGate, cloneConfig: cfg.Clone,
			migrationFactory: configuredMCPMigrationRuntime,
		},
		Policy: access, Ledger: access, Value: access, Guarantees: access,
		// Sage SRE read tools (sre_list_incidents, sre_get_investigation,
		// sre_get_evidence) resolve through the same fleet.
		Investigations: access,
		// SLO and change-feed read tools (sre_list_slos, sre_get_slo,
		// sre_list_changes).
		Signals: access,
		// Sage SRE action tools (sre_propose_action, sre_request_execution)
		// propose and queue; they never execute.
		Actions: access,
		// Earned autonomy: read; operator downgrade/review/evaluate (never approval).
		Autonomy: autonomyMCPBackend{registry: processAutonomy().registry, manager: fleetMgr},
		// Binding facts: read, propose (stays proposed), decide (a person).
		Facts: factsMCPBackend{manager: fleetMgr},
		// Coding-agent tools (roadmap phase 3) on the resolved database.
		AgentTools: fleetAgentTools{manager: fleetMgr, options: agentToolOptions(cfg)},
	}
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
	instance := mcpInstanceFor(ctx, gate.manager, request.DatabaseID)
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
	adapter, err := access.adapter(ctx, request.DatabaseID)
	if err != nil {
		return mcp.PolicyResult{}, err
	}
	return adapter.GetPolicy(ctx, request)
}

func (access *fleetMCPAccess) ProposePolicyChangeDryRun(
	ctx context.Context, request mcp.PolicyProposalRequest,
) (mcp.PolicyProposalResult, error) {
	adapter, err := access.adapter(ctx, request.DatabaseID)
	if err != nil {
		return mcp.PolicyProposalResult{}, err
	}
	return adapter.ProposePolicyChangeDryRun(ctx, request)
}

func (access *fleetMCPAccess) GetLedger(
	ctx context.Context, request mcp.LedgerRequest,
) (mcp.LedgerResult, error) {
	adapter, err := access.adapter(ctx, databaseIDFromLedgerFilter(request.Filter))
	if err != nil {
		return mcp.LedgerResult{}, err
	}
	return adapter.GetLedger(ctx, request)
}

// GetValue aggregates the value ledger of the monitored databases (D3):
// the database the MCP server resolved for the request, or every one.
// It never reads the meta database: in meta-db mode the ledger lives in
// the targets.
func (access *fleetMCPAccess) GetValue(ctx context.Context) (map[string]any, error) {
	if access == nil || access.manager == nil {
		return nil, fmt.Errorf("MCP value fleet is unavailable")
	}
	manager := access.manager
	name, named := mcp.DatabaseFromContext(ctx)
	reader := value.NewFleetService(func() []value.Source {
		sources := fleet.ValueSources(manager)
		if !named {
			return sources
		}
		var selected []value.Source
		for _, source := range sources {
			if source.Name == name {
				selected = append(selected, source)
			}
		}
		return selected
	})
	return mcp.NewValueAccess(reader).GetValue(ctx)
}

func (access *fleetMCPAccess) GetGuaranteeStatus(
	ctx context.Context,
) (mcp.GuaranteeStatus, error) {
	adapter, err := access.adapter(ctx, nil)
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
	adapter, err := executor.access.adapter(ctx, request.DatabaseID)
	if err != nil {
		return nil, err
	}
	production := mcp.NewProductionIntentExecutor(adapter, plan.NewPlanner(), executor.gate)
	return production.Execute(ctx, request, decision)
}

func (executor *fleetIntentExecutor) ExecuteConcrete(
	ctx context.Context, request policy.ActionRequest,
) (any, error) {
	adapter, err := executor.access.adapter(ctx, request.DatabaseID)
	if err != nil {
		return nil, err
	}
	production := mcp.NewProductionIntentExecutor(adapter, plan.NewPlanner(), executor.gate)
	if request.Contract != nil && request.Contract.ActionType == "apply_migration" {
		factory := executor.migrationFactory
		if factory == nil {
			factory = configuredMCPMigrationRuntime
		}
		targetPool, poolErr := executor.access.targetPool(ctx, request.DatabaseID)
		if poolErr == nil {
			boundGate := mcp.NeverApproved(databaseBoundMCPGate{
				delegate: executor.gate, databaseID: request.DatabaseID,
			})
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

func (access *fleetMCPAccess) adapter(
	ctx context.Context, databaseID *int64,
) (*mcp.PostgresAccess, error) {
	pool, err := access.targetPool(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	return mcp.NewPostgresAccess(pool), nil
}

func (access *fleetMCPAccess) targetPool(
	ctx context.Context, databaseID *int64,
) (*pgxpool.Pool, error) {
	instance := mcpInstanceFor(ctx, access.manager, databaseID)
	if instance != nil && instance.Pool != nil {
		return instance.Pool, nil
	}
	_, named := mcp.DatabaseFromContext(ctx)
	if databaseID == nil && !named && access.fallback != nil {
		return access.fallback, nil
	}
	return nil, fmt.Errorf("MCP target database is unavailable")
}

// mcpInstanceFor is the instance an MCP request targets: the database the
// MCP server resolved (in ctx) when there is one, else the legacy
// database id, else the only instance.
func mcpInstanceFor(
	ctx context.Context, manager *fleet.DatabaseManager, databaseID *int64,
) *fleet.DatabaseInstance {
	if name, ok := mcp.DatabaseFromContext(ctx); ok {
		if manager == nil {
			return nil
		}
		return manager.GetInstance(name)
	}
	return mcpInstance(manager, databaseID)
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
