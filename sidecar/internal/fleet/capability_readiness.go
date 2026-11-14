package fleet

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
)

// BuildProviderCapabilities builds readiness for callers without a request
// context (registration and the fleet overview).
func BuildProviderCapabilities(
	provider string, isReplica bool, explain FamilyExplainer,
) ProviderCapabilities {
	return buildProviderCapabilities(context.Background(), provider, isReplica, explain)
}

func buildProviderCapabilities(
	ctx context.Context, provider string, isReplica bool, explain FamilyExplainer,
) ProviderCapabilities {
	adapter := AdapterForProvider(provider)
	caps := ProviderCapabilities{
		Provider:       adapter.Provider,
		IsReplica:      isReplica,
		Permissions:    defaultPermissionReadiness(),
		Extensions:     adapter.Extensions,
		ExtensionHints: extensionHints(adapter.Extensions),
		LogAccess:      adapter.LogAccess,
		Limitations:    adapter.Limitations,
	}
	caps.ActionFamilies = buildActionFamilyReadiness(ctx, caps, explain)
	caps.Blockers = readinessBlockers(caps)
	caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	return caps
}

// readinessActionTypes are the action families shown in readiness views.
func readinessActionTypes() []string {
	return []string{
		"analyze_table",
		"vacuum_table",
		"alter_system_guc",
		"alter_database_guc",
		"diagnose_lock_blockers",
		"diagnose_runaway_query",
		"diagnose_connection_exhaustion",
		"diagnose_wal_replication",
		"diagnose_standby_conflicts",
		"prepare_sequence_capacity_migration",
		"cancel_backend",
		"terminate_backend",
		"diagnose_freeze_blockers",
		"diagnose_vacuum_pressure",
		"create_index_concurrently",
		"drop_unused_index",
		"set_table_autovacuum",
		"plan_bloat_remediation",
		"reindex_concurrently",
		"prepare_query_rewrite",
		"promote_role_work_mem",
		"create_statistics",
		"prepare_parameterized_query",
		"retire_query_hint",
		"apply_query_hint",
		"ddl_preflight",
		"alter_table",
	}
}

// buildActionFamilyReadiness applies implementation and provider-adapter
// support, then asks the standing gate about the remaining families in
// one batch.
func buildActionFamilyReadiness(
	ctx context.Context, caps ProviderCapabilities, explain FamilyExplainer,
) []ActionFamilyReadiness {
	out := make([]ActionFamilyReadiness, 0, len(readinessActionTypes()))
	var contracts []executor.ActionContract
	var slots []int
	for _, actionType := range readinessActionTypes() {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			continue
		}
		if reason := familyUnsupported(caps, actionType); reason != "" {
			out = append(out, ActionFamilyReadiness{ActionType: actionType,
				Decision: executor.PolicyDecisionBlocked, BlockedReason: reason})
			continue
		}
		slots = append(slots, len(out))
		contracts = append(contracts, contract)
		out = append(out, ActionFamilyReadiness{ActionType: actionType})
	}
	if len(contracts) == 0 {
		return out
	}
	if explain == nil {
		explain = ExecutorFamilyExplainer(nil)
	}
	for i, decision := range explain(ctx, contracts, caps.IsReplica) {
		out[slots[i]] = familyReadiness(contracts[i].ActionType, decision)
	}
	return out
}

func familyUnsupported(caps ProviderCapabilities, actionType string) string {
	if !directExecutionImplemented(actionType) {
		return "direct execution is not implemented"
	}
	if !AdapterForProvider(caps.Provider).SupportsAction(actionType) {
		return "provider adapter does not support action"
	}
	return ""
}

func familyReadiness(
	actionType string, decision executor.ActionPolicyDecision,
) ActionFamilyReadiness {
	return ActionFamilyReadiness{
		ActionType:                actionType,
		Supported:                 decision.Decision != executor.PolicyDecisionBlocked,
		Decision:                  decision.Decision,
		BlockedReason:             permissionBlockedReason(actionType, decision),
		RequiresApproval:          decision.RequiresApproval,
		RequiresMaintenanceWindow: decision.RequiresMaintenanceWindow,
		Guardrails:                decision.Guardrails,
	}
}

func directExecutionImplemented(actionType string) bool {
	switch actionType {
	case "promote_role_work_mem":
		return false
	default:
		return true
	}
}

func CollectProviderCapabilities(
	ctx context.Context,
	pool *pgxpool.Pool,
	provider string,
	explain FamilyExplainer,
) ProviderCapabilities {
	caps := buildProviderCapabilities(ctx, provider, detectReplica(ctx, pool), explain)
	collectRuntimeEvidence(ctx, pool, &caps)
	caps.Blockers = readinessBlockers(caps)
	caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	return caps
}

func BuildFleetReadiness(
	mode string,
	databases []DatabaseStatus,
) FleetReadiness {
	response := FleetReadiness{
		Mode:      mode,
		Databases: make([]DatabaseReadiness, 0, len(databases)),
	}
	for _, db := range databases {
		caps := ProviderCapabilities{}
		if db.Status != nil {
			caps = db.Status.Capabilities
		}
		provider := caps.Provider
		if provider == "" && db.Status != nil {
			provider = db.Status.Platform
		}
		response.Databases = append(response.Databases, DatabaseReadiness{
			Name:             db.Name,
			Provider:         normalizeProviderName(provider),
			ReadyForAutoSafe: caps.ReadyForAutoSafe,
			Blockers:         caps.Blockers,
			Capabilities:     caps,
		})
	}
	response.Summary = SummarizeReadiness(databases)
	return response
}

func SummarizeReadiness(databases []DatabaseStatus) FleetReadinessSummary {
	summary := FleetReadinessSummary{TotalDatabases: len(databases)}
	for _, db := range databases {
		if db.Status == nil {
			summary.Unknown++
			continue
		}
		caps := db.Status.Capabilities
		if caps.Provider == "" || hasUnknownReadiness(caps) {
			summary.Unknown++
		} else if caps.ReadyForAutoSafe {
			summary.ReadyForAutoSafe++
		} else {
			summary.Blocked++
		}
	}
	return summary
}

// EnsureCapabilities fills capabilities and recomputes action-family
// readiness from the instance executor's standing gate. A stopped
// instance, or one without an executor, reports every family blocked.
// FleetStatus has no request context, so readiness explains under
// context.Background.
func EnsureCapabilities(inst *DatabaseInstance, snap *InstanceStatus) *InstanceStatus {
	if snap == nil {
		snap = &InstanceStatus{}
	}
	if snap.Platform == "" {
		snap.Platform = normalizeProviderName(snap.Capabilities.Provider)
	}
	if snap.Platform == "" {
		snap.Platform = "unknown"
	}
	explain := InstanceFamilyExplainer(inst)
	caps := snap.Capabilities
	if caps.Provider == "" {
		caps = BuildProviderCapabilities(snap.Platform, caps.IsReplica, explain)
	} else {
		caps.ActionFamilies = buildActionFamilyReadiness(context.Background(), caps, explain)
		caps.Blockers = readinessBlockers(caps)
		caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	}
	if inst != nil && inst.InstanceOwner != nil {
		o := inst.InstanceOwner()
		caps.InstanceOwner = &o
	}
	snap.Capabilities = caps
	return snap
}
