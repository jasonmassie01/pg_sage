package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/policy"
)

var ErrProductionDependencyUnavailable = errors.New("MCP production dependency unavailable")

type IntentPlanner interface {
	Plan(context.Context, string, json.RawMessage) (policy.ActionRequest, error)
}

type IntentExecutor interface {
	Execute(context.Context, policy.ActionRequest, policy.Decision) (any, error)
}

type ConcreteIntentExecutor interface {
	ExecuteConcrete(context.Context, policy.ActionRequest) (any, error)
}

type PolicyAccess interface {
	GetPolicy(context.Context, PolicyRequest) (PolicyResult, error)
	ProposePolicyChangeDryRun(
		context.Context, PolicyProposalRequest,
	) (PolicyProposalResult, error)
}

type LedgerAccess interface {
	GetLedger(context.Context, LedgerRequest) (LedgerResult, error)
}

type ValueAccess interface {
	GetValue(context.Context) (map[string]any, error)
}

type GuaranteeAccess interface {
	GetGuaranteeStatus(context.Context) (GuaranteeStatus, error)
}

type ProductionDependencies struct {
	Gate       policy.Gate
	Planner    IntentPlanner
	Executor   IntentExecutor
	Policy     PolicyAccess
	Ledger     LedgerAccess
	Value      ValueAccess
	Guarantees GuaranteeAccess
}

type ProductionBackend struct {
	dependencies ProductionDependencies
}

func NewProductionBackend(
	dependencies ProductionDependencies,
) (*ProductionBackend, error) {
	if dependencies.Gate == nil || dependencies.Planner == nil ||
		dependencies.Executor == nil || dependencies.Policy == nil ||
		dependencies.Ledger == nil || dependencies.Value == nil ||
		dependencies.Guarantees == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return &ProductionBackend{dependencies: dependencies}, nil
}

func (backend *ProductionBackend) GetPolicy(
	ctx context.Context, request PolicyRequest,
) (PolicyResult, error) {
	return backend.dependencies.Policy.GetPolicy(ctx, request)
}

func (backend *ProductionBackend) ProposePolicyChange(
	ctx context.Context, request PolicyProposalRequest,
) (PolicyProposalResult, error) {
	request.CallerClaims = nil
	return backend.dependencies.Policy.ProposePolicyChangeDryRun(ctx, request)
}

func (backend *ProductionBackend) RequestChange(
	ctx context.Context, request ChangeRequest,
) (ChangeResult, error) {
	return backend.requestMutation(
		ctx, "request_change", request.DatabaseID, request.Intent,
	)
}

func (backend *ProductionBackend) GetLedger(
	ctx context.Context, request LedgerRequest,
) (LedgerResult, error) {
	return backend.dependencies.Ledger.GetLedger(ctx, request)
}

func (backend *ProductionBackend) RequestIntent(
	ctx context.Context, tool string, arguments json.RawMessage,
) (any, error) {
	switch tool {
	case "get_value":
		return backend.dependencies.Value.GetValue(ctx)
	case "set_maintenance_policy":
		return backend.proposeMaintenancePolicy(ctx, arguments)
	case "get_guarantee_status":
		return backend.dependencies.Guarantees.GetGuaranteeStatus(ctx)
	case "optimize_query", "apply_migration", "ensure_fk_indexes",
		"declare_table_contract", "register_consumer":
		cleaned := removeCallerClaims(arguments)
		return backend.requestMutation(ctx, tool, databaseIDFromJSON(cleaned), cleaned)
	default:
		return nil, fmt.Errorf("unsupported production intent %q", tool)
	}
}

func (backend *ProductionBackend) requestMutation(
	ctx context.Context, tool string, databaseID *int64, arguments json.RawMessage,
) (ChangeResult, error) {
	request, err := backend.dependencies.Planner.Plan(ctx, tool, arguments)
	if err != nil {
		return ChangeResult{}, fmt.Errorf("plan %s intent: %w", tool, err)
	}
	request.DatabaseID = databaseID
	if concreteIntent(tool) {
		executor, ok := backend.dependencies.Executor.(ConcreteIntentExecutor)
		if ok {
			outcome, err := executor.ExecuteConcrete(ctx, request)
			if err != nil {
				return ChangeResult{}, fmt.Errorf("evaluate %s intent: %w", tool, err)
			}
			return ChangeResult{Decision: "recommend_only", Outcome: outcome}, nil
		}
	}
	decision := backend.dependencies.Gate.Authorize(ctx, request)
	result := ChangeResult{
		Decision:   decisionResult(decision.Verdict),
		EvidenceID: decision.EvidenceID,
		Reason:     string(decision.Reason),
	}
	if decision.Verdict != policy.VerdictExecute {
		return result, nil
	}
	outcome, err := backend.dependencies.Executor.Execute(ctx, request, decision)
	if err != nil {
		return ChangeResult{}, fmt.Errorf("execute %s intent: %w", tool, err)
	}
	result.Outcome = outcome
	return result, nil
}

func concreteIntent(tool string) bool {
	return tool == "optimize_query" || tool == "ensure_fk_indexes" ||
		tool == "apply_migration"
}

func (backend *ProductionBackend) proposeMaintenancePolicy(
	ctx context.Context, arguments json.RawMessage,
) (PolicyProposalResult, error) {
	var input struct {
		DatabaseID *int64          `json:"database_id"`
		Patch      json.RawMessage `json:"patch"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return PolicyProposalResult{}, fmt.Errorf("decode maintenance policy: %w", err)
	}
	if emptyJSON(input.Patch) {
		return PolicyProposalResult{}, errors.New("maintenance policy patch is required")
	}
	return backend.dependencies.Policy.ProposePolicyChangeDryRun(
		ctx,
		PolicyProposalRequest{DatabaseID: input.DatabaseID, Delta: input.Patch},
	)
}

func removeCallerClaims(arguments json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(arguments, &object) != nil {
		return arguments
	}
	delete(object, "caller_claims")
	cleaned, err := json.Marshal(object)
	if err != nil {
		return arguments
	}
	return cleaned
}

func databaseIDFromJSON(arguments json.RawMessage) *int64 {
	var input struct {
		DatabaseID *int64 `json:"database_id"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		return nil
	}
	return input.DatabaseID
}

func decisionResult(verdict policy.Verdict) string {
	switch verdict {
	case policy.VerdictExecute:
		return "granted"
	case policy.VerdictQueueApproval:
		return "queued"
	case policy.VerdictPark:
		return "parked"
	default:
		return string(verdict)
	}
}
