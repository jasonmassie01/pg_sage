package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
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

func BuildProviderCapabilities(
	cfg *config.Config,
	provider string,
	isReplica bool,
	mode string,
	stopped bool,
	now time.Time,
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
	caps.ActionFamilies = BuildActionFamilyReadiness(cfg, caps, mode, stopped, now)
	caps.Blockers = readinessBlockers(caps)
	caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	return caps
}

func BuildActionFamilyReadiness(
	cfg *config.Config,
	caps ProviderCapabilities,
	mode string,
	stopped bool,
	now time.Time,
) []ActionFamilyReadiness {
	actionTypes := []string{
		"analyze_table",
		"vacuum_table",
		"diagnose_lock_blockers",
		"diagnose_runaway_query",
		"diagnose_connection_exhaustion",
		"diagnose_wal_replication",
		"diagnose_freeze_blockers",
		"diagnose_vacuum_pressure",
		"create_index_concurrently",
		"drop_unused_index",
		"reindex_concurrently",
		"prepare_query_rewrite",
		"promote_role_work_mem",
		"create_statistics",
		"prepare_parameterized_query",
		"retire_query_hint",
		"alter_table",
	}
	out := make([]ActionFamilyReadiness, 0, len(actionTypes))
	for _, actionType := range actionTypes {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			continue
		}
		if !AdapterForProvider(caps.Provider).SupportsAction(actionType) {
			out = append(out, ActionFamilyReadiness{
				ActionType:    actionType,
				Supported:     false,
				Decision:      executor.PolicyDecisionBlocked,
				BlockedReason: "provider adapter does not support action",
			})
			continue
		}
		ctx := readinessPolicyContext(cfg, caps, mode, stopped, now)
		decision := executor.EvaluateActionPolicy(contract, ctx)
		out = append(out, ActionFamilyReadiness{
			ActionType:                actionType,
			Supported:                 decision.Decision != executor.PolicyDecisionBlocked,
			Decision:                  decision.Decision,
			BlockedReason:             permissionBlockedReason(actionType, decision),
			RequiresApproval:          decision.RequiresApproval,
			RequiresMaintenanceWindow: decision.RequiresMaintenanceWindow,
			Guardrails:                decision.Guardrails,
		})
	}
	return out
}

func CollectProviderCapabilities(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg *config.Config,
	provider string,
	mode string,
	stopped bool,
	now time.Time,
) ProviderCapabilities {
	return BuildProviderCapabilities(
		cfg, provider, detectReplica(ctx, pool), mode, stopped, now)
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

func EnsureCapabilities(
	cfg *config.Config,
	inst *DatabaseInstance,
	snap *InstanceStatus,
	now time.Time,
) *InstanceStatus {
	if snap == nil {
		snap = &InstanceStatus{}
	}
	if snap.Platform == "" {
		snap.Platform = normalizeProviderName(snap.Capabilities.Provider)
	}
	if snap.Platform == "" {
		snap.Platform = "unknown"
	}
	if snap.Capabilities.Provider == "" ||
		len(snap.Capabilities.ActionFamilies) == 0 {
		mode := "auto"
		if inst != nil && inst.Config.ExecutionMode != "" {
			mode = inst.Config.ExecutionMode
		}
		stopped := inst != nil && inst.Stopped
		snap.Capabilities = BuildProviderCapabilities(
			cfg, snap.Platform, snap.Capabilities.IsReplica,
			mode, stopped, now)
	}
	return snap
}

func readinessPolicyContext(
	cfg *config.Config,
	caps ProviderCapabilities,
	mode string,
	stopped bool,
	now time.Time,
) executor.ActionPolicyContext {
	copied := &config.Config{}
	if cfg != nil {
		c := *cfg
		copied = &c
	}
	copied.CloudEnvironment = caps.Provider
	return executor.ActionPolicyContext{
		Config:          copied,
		ExecutionMode:   mode,
		Now:             now,
		RampStart:       now.Add(-365 * 24 * time.Hour),
		IsReplica:       caps.IsReplica,
		EmergencyStop:   stopped,
		SafeActionLimit: 3,
	}
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
	case "postgresql", "self-managed":
		return "postgres"
	default:
		return p
	}
}
