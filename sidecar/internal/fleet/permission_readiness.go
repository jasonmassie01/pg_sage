package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

func defaultPermissionReadiness() map[string]CapabilityStatus {
	return map[string]CapabilityStatus{
		"analyze": {
			Status: "unknown",
			Reason: "maintenance privilege not probed",
		},
		"read_stats": {
			Status: "unknown",
			Reason: "pg_stat access depends on configured grants",
		},
	}
}

// extensionHints are the adapter's informative extension states; an
// "unknown" entry says nothing about how to enable the extension.
func extensionHints(adapter map[string]string) map[string]string {
	hints := make(map[string]string, len(adapter))
	for name, state := range adapter {
		if state != "" && state != "unknown" {
			hints[name] = state
		}
	}
	return hints
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
	if caps.PendingProvider != "" {
		blockers = append(blockers,
			"provider re-probe found "+caps.PendingProvider+"; restart pg_sage to apply")
	}
	if caps.IsReplica {
		blockers = append(blockers, "target is a replica")
	}
	// "limited" is informational: ANALYZE is checked per table at execution.
	if p := caps.Permissions["analyze"]; p.Status == "unknown" || p.Status == "" {
		blockers = append(blockers, "ANALYZE permission unknown")
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

// normalizeProviderName is the registry's canonical name; an unrecognised
// name is kept (lower-cased) so readiness shows what was detected.
func normalizeProviderName(provider string) string {
	if name := prov.Canonical(provider); name != "" {
		return name
	}
	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		return p
	}
	return prov.Unknown
}
