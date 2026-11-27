package fleet

import (
	"context"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/tuner"
)

type CapabilityStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type ProviderCapabilities struct {
	Provider         string                      `json:"provider,omitempty"`
	IsReplica        bool                        `json:"is_replica"`
	Permissions      map[string]CapabilityStatus `json:"permissions,omitempty"`
	Extensions       map[string]string           `json:"extensions,omitempty"`
	LogAccess        string                      `json:"log_access,omitempty"`
	Limitations      []string                    `json:"limitations,omitempty"`
	Blockers         []string                    `json:"blockers,omitempty"`
	ActionFamilies   []ActionFamilyReadiness     `json:"action_families,omitempty"`
	ReadyForAutoSafe bool                        `json:"ready_for_auto_safe"`
	// ExtensionHints are the provider adapter's enable paths (for example
	// azure_extensions_allowlist_required), kept apart from the runtime
	// Extensions status, which only catalog evidence sets.
	ExtensionHints map[string]string `json:"extension_hints,omitempty"`
	// PendingProvider is what a re-probe of an unknown provider found where
	// the runtime cannot be rebuilt in place; it applies after a restart.
	PendingProvider string `json:"pending_provider,omitempty"`
	// HintPlan holds whether pg_hint_plan hints reach application sessions
	// (tuning-04); nil when the module is absent or was not probed.
	HintPlan *tuner.HintPlanAvailability `json:"hint_plan,omitempty"`
	// InstanceOwner is which database of the fleet manages this database's
	// instance settings (advisors-08); nil outside fleet mode.
	InstanceOwner *InstanceOwnership `json:"instance_owner,omitempty"`
}

// InstanceOwnership is the owner of a PostgreSQL instance's settings among
// the fleet's databases on it: only the owner asks the instance-level
// questions (WAL, connections) and proposes ALTER SYSTEM or managed
// parameter changes. KeySource is how the instance was identified
// (system_identifier or address); empty with a Reason when it
// could not be, and the database is then advised on its own.
type InstanceOwnership struct {
	Owner     string `json:"owner,omitempty"`
	IsOwner   bool   `json:"is_owner"`
	KeySource string `json:"key_source,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type ActionFamilyReadiness struct {
	ActionType                string   `json:"action_type"`
	Supported                 bool     `json:"supported"`
	Decision                  string   `json:"decision"`
	BlockedReason             string   `json:"blocked_reason,omitempty"`
	RequiresApproval          bool     `json:"requires_approval"`
	RequiresMaintenanceWindow bool     `json:"requires_maintenance_window"`
	Guardrails                []string `json:"guardrails,omitempty"`
}

type FleetReadinessSummary struct {
	TotalDatabases   int `json:"total_databases"`
	ReadyForAutoSafe int `json:"ready_for_auto_safe"`
	Blocked          int `json:"blocked"`
	Unknown          int `json:"unknown"`
}

type FleetReadiness struct {
	Mode      string                `json:"mode"`
	Summary   FleetReadinessSummary `json:"summary"`
	Databases []DatabaseReadiness   `json:"databases"`
}

type DatabaseReadiness struct {
	Name             string               `json:"name"`
	Provider         string               `json:"provider"`
	ReadyForAutoSafe bool                 `json:"ready_for_auto_safe"`
	Blockers         []string             `json:"blockers,omitempty"`
	Capabilities     ProviderCapabilities `json:"capabilities"`
}

// FamilyExplainer reports the standing policy's verdict for action
// families on one database (the executor's gate, one policy snapshot).
// The policy reads run under ctx, so a cancelled caller stops them.
type FamilyExplainer func(
	ctx context.Context, contracts []executor.ActionContract, isReplica bool,
) []executor.ActionPolicyDecision

// ExecutorFamilyExplainer explains families through exec's standing gate
// under the caller's ctx; a nil executor fails closed.
func ExecutorFamilyExplainer(exec *executor.Executor) FamilyExplainer {
	return func(
		ctx context.Context, contracts []executor.ActionContract, isReplica bool,
	) []executor.ActionPolicyDecision {
		return exec.ExplainFamilies(ctx, contracts, isReplica)
	}
}

// InstanceFamilyExplainer explains families through the instance
// executor; a stopped instance or a missing executor fails closed.
func InstanceFamilyExplainer(inst *DatabaseInstance) FamilyExplainer {
	explain := InstanceActionExplainer(inst)
	return func(
		ctx context.Context, contracts []executor.ActionContract, isReplica bool,
	) []executor.ActionPolicyDecision {
		requests := make([]executor.ExplainActionRequest, len(contracts))
		for i, contract := range contracts {
			requests[i].Contract = contract
		}
		return explain(ctx, requests, isReplica)
	}
}

// ActionExplainer reports the standing policy's verdict for concrete
// actions (and families, for a request without SQL) on one database, in
// one policy snapshot. The reads run under ctx.
type ActionExplainer func(
	ctx context.Context, requests []executor.ExplainActionRequest, isReplica bool,
) []executor.ActionPolicyDecision

// InstanceActionExplainer explains actions through the instance executor;
// a stopped instance (emergency stop) or a missing executor fails closed.
func InstanceActionExplainer(inst *DatabaseInstance) ActionExplainer {
	switch {
	case inst == nil:
		return (*executor.Executor)(nil).ExplainActions
	case inst.Stopped:
		return stoppedActionExplainer
	}
	return inst.Executor.ExplainActions
}

func stoppedActionExplainer(
	_ context.Context, requests []executor.ExplainActionRequest, _ bool,
) []executor.ActionPolicyDecision {
	out := make([]executor.ActionPolicyDecision, len(requests))
	for i, request := range requests {
		out[i] = executor.ActionPolicyDecision{Decision: executor.PolicyDecisionBlocked,
			RiskTier: request.Contract.BaseRiskTier, BlockedReason: "emergency stop is active",
			Guardrails: request.Contract.Guardrails}
	}
	return out
}
