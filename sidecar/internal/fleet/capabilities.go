package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
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
type FamilyExplainer func(
	contracts []executor.ActionContract, isReplica bool,
) []executor.ActionPolicyDecision

// ExecutorFamilyExplainer explains families through exec's standing gate;
// a nil executor fails closed.
func ExecutorFamilyExplainer(exec *executor.Executor) FamilyExplainer {
	return func(contracts []executor.ActionContract, isReplica bool) []executor.ActionPolicyDecision {
		return exec.ExplainFamilies(context.Background(), contracts, isReplica)
	}
}

func BuildProviderCapabilities(
	provider string, isReplica bool, explain FamilyExplainer,
) ProviderCapabilities {
	adapter := AdapterForProvider(provider)
	caps := ProviderCapabilities{
		Provider:    adapter.Provider,
		IsReplica:   isReplica,
		Permissions: defaultPermissionReadiness(),
		Extensions:  adapter.Extensions,
		LogAccess:   adapter.LogAccess,
		Limitations: adapter.Limitations,
	}
	caps.ActionFamilies = buildActionFamilyReadiness(caps, explain)
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
	caps ProviderCapabilities, explain FamilyExplainer,
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
	for i, decision := range explain(contracts, caps.IsReplica) {
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
	case "create_statistics", "promote_role_work_mem":
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
	caps := BuildProviderCapabilities(provider, detectReplica(ctx, pool), explain)
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
		caps.ActionFamilies = buildActionFamilyReadiness(caps, explain)
		caps.Blockers = readinessBlockers(caps)
		caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	}
	snap.Capabilities = caps
	return snap
}

// InstanceFamilyExplainer explains families through the instance
// executor; a stopped instance or a missing executor fails closed.
func InstanceFamilyExplainer(inst *DatabaseInstance) FamilyExplainer {
	if inst == nil {
		return ExecutorFamilyExplainer(nil)
	}
	if inst.Stopped {
		return func(contracts []executor.ActionContract, _ bool) []executor.ActionPolicyDecision {
			out := make([]executor.ActionPolicyDecision, len(contracts))
			for i, contract := range contracts {
				out[i] = executor.ActionPolicyDecision{Decision: executor.PolicyDecisionBlocked,
					RiskTier: contract.BaseRiskTier, BlockedReason: "emergency stop is active",
					Guardrails: contract.Guardrails}
			}
			return out
		}
	}
	return ExecutorFamilyExplainer(inst.Executor)
}

func defaultPermissionReadiness() map[string]CapabilityStatus {
	return map[string]CapabilityStatus{
		"analyze": {
			Status: "unknown",
			Reason: "table-specific ANALYZE permission checked at execution",
		},
		"create_schema_object": {
			Status: "unknown",
			Reason: "schema CREATE privilege not checked in this slice",
		},
		"read_stats": {
			Status: "unknown",
			Reason: "pg_stat access depends on configured grants",
		},
	}
}

func defaultExtensionReadiness() map[string]string {
	return map[string]string{
		"pg_stat_statements": "unknown",
		"hypopg":             "unknown",
		"pg_hint_plan":       "unknown",
		"auto_explain":       "unknown",
	}
}

func readinessBlockers(caps ProviderCapabilities) []string {
	var blockers []string
	if caps.Provider == "" || caps.Provider == "unknown" {
		blockers = append(blockers, "provider unknown")
	}
	if caps.IsReplica {
		blockers = append(blockers, "target is a replica")
	}
	if p := caps.Permissions["analyze"]; p.Status != "ok" {
		blockers = append(blockers, "ANALYZE permission "+p.Status)
	}
	if s := caps.Extensions["pg_stat_statements"]; s != "available" {
		blockers = append(blockers, "pg_stat_statements "+s)
	}
	for _, family := range caps.ActionFamilies {
		if family.ActionType == "analyze_table" &&
			family.Decision != executor.PolicyDecisionExecute &&
			family.BlockedReason != "" {
			blockers = append(blockers, family.BlockedReason)
			break
		}
	}
	return blockers
}

func readyForAutoSafe(caps ProviderCapabilities) bool {
	if len(caps.Blockers) > 0 {
		return false
	}
	for _, family := range caps.ActionFamilies {
		if family.ActionType == "analyze_table" &&
			family.Decision == executor.PolicyDecisionExecute {
			return true
		}
	}
	return false
}

func hasUnknownReadiness(caps ProviderCapabilities) bool {
	if caps.Provider == "" || caps.Provider == "unknown" {
		return true
	}
	for _, p := range caps.Permissions {
		if p.Status == "unknown" {
			return true
		}
	}
	for _, status := range caps.Extensions {
		if status == "unknown" {
			return true
		}
	}
	return false
}

func permissionBlockedReason(
	actionType string,
	decision executor.ActionPolicyDecision,
) string {
	if decision.BlockedReason != "" {
		return decision.BlockedReason
	}
	if actionType == "analyze_table" {
		return "table-specific permission checked at execution"
	}
	return ""
}

func detectReplica(ctx context.Context, pool *pgxpool.Pool) bool {
	if pool == nil {
		return false
	}
	var replica bool
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.QueryRow(qctx, "SELECT pg_is_in_recovery()").Scan(&replica); err != nil {
		return false
	}
	return replica
}

func normalizeProviderName(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "", "unknown":
		return "unknown"
	case "cloudsql", "cloud sql", "gcp-cloud-sql":
		return "cloud-sql"
	case "aurora-postgresql":
		return "aurora"
	case "aws-rds":
		return "rds"
	case "azure-flexible", "azure-single", "azure-postgres":
		return "azure"
	case "postgresql", "self-managed":
		return "postgres"
	default:
		return p
	}
}
