package executor

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// isApprovalRequiredGuardrail recognises the approval guardrail however a
// contract spells it ("approval required", "approval_required", ...).
func isApprovalRequiredGuardrail(value string) bool {
	normalized := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' {
			return ' '
		}
		return unicode.ToLower(r)
	}, value)
	return strings.Join(strings.Fields(normalized), " ") == "approval required"
}

// changeClassForActionType derives the policy change class from the typed
// contract. Unknown action types have no class, which fails closed.
func changeClassForActionType(actionType string) string {
	switch actionType {
	case "create_index_concurrently", "drop_unused_index", "reindex_concurrently",
		"revert_created_index":
		return string(policy.ChangeIndex)
	case "analyze_table":
		return string(policy.ChangeAnalyze)
	case "vacuum_table":
		return string(policy.ChangeVacuum)
	case "set_table_autovacuum":
		return string(policy.ChangeAutovacuumTuning)
	case "alter_system_guc", "alter_database_guc":
		return string(policy.ChangeConfigGUC)
	case "cancel_backend", "terminate_backend":
		return string(policy.ChangeBackendSignal)
	case "apply_query_hint", "retire_query_hint":
		return string(policy.ChangeQueryHint)
	case "alter_table":
		return string(policy.ChangeSchemaChange)
	case "retention_delete":
		return string(policy.ChangeRetention)
	default:
		return ""
	}
}

// standingRuntimeState is the live authority snapshot handed to the gate. It
// carries the config ceilings (tier3 flags, trust ramp, configured
// maintenance window) so the gate enforces the documented ramp.
func (e *Executor) standingRuntimeState(
	ctx context.Context, request policy.ActionRequest,
) policy.RuntimeState {
	cfg, mode, enabled := e.policySnapshot()
	state := policy.RuntimeState{
		ExecutorEnabled: enabled, EmergencyStop: e.checkEmergencyStop(ctx),
		IsReplica: request.IsReplica, ExecutionMode: mode, RampStart: e.rampStart,
		SQLValidationDegraded: !ASTValidationAvailable(),
	}
	if cfg == nil {
		return state
	}
	state.TrustLevel = cfg.Trust.Level
	state.Tier3Safe = cfg.Trust.Tier3Safe
	state.Tier3Moderate = cfg.Trust.Tier3Moderate
	state.InConfiguredWindow = inMaintenanceWindowForPolicy(cfg, time.Now())
	state.Provider = cfg.CloudEnvironment
	state.WindowConfigured = strings.TrimSpace(cfg.Trust.MaintenanceWindow) != ""
	return state
}

// standingUsage reports self-initiated executions in the rolling 24-hour
// window for the gate's rate and blast-radius limits. Operator-approved
// decisions are excluded: they are not self-initiated.
func (e *Executor) standingUsage(
	ctx context.Context, _ policy.ActionRequest,
) (policy.LimitUsage, error) {
	var usage policy.LimitUsage
	if e.pool == nil {
		return usage, fmt.Errorf("policy usage requires a database pool")
	}
	err := e.pool.QueryRow(ctx, standingUsageSQL, operatorDecisionIntent).Scan(
		&usage.SelfInitiatedChangesInWindow, &usage.TablesInWindow)
	if err != nil {
		return usage, fmt.Errorf("read policy usage: %w", err)
	}
	return usage, nil
}

const standingUsageSQL = `/* pg_sage */
WITH recent AS (
	SELECT al.id, d.target_objects
	  FROM sage.action_log al
	  JOIN sage.decision d ON d.id = al.decision_id
	 WHERE al.executed_at > now() - interval '24 hours'
	   AND d.intent <> $1
)
SELECT (SELECT count(*) FROM recent),
       (SELECT count(DISTINCT target.obj)
          FROM recent CROSS JOIN LATERAL
               jsonb_array_elements_text(recent.target_objects) AS target(obj))`

// policySnapshot copies the live config under the hot-reload read lock so
// authorization never reads a half-applied config change.
func (e *Executor) policySnapshot() (*config.Config, string, bool) {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	if e.cfg == nil {
		return nil, e.execMode, !e.executorDisabled
	}
	config.RLockForHotReload()
	cfgCopy := *e.cfg
	config.RUnlockForHotReload()
	if e.trustLevelOverride != "" {
		cfgCopy.Trust.Level = e.trustLevelOverride
	}
	return &cfgCopy, e.execMode, !e.executorDisabled
}

// noteRecentAction records an action for the cascade cooldown. The map is
// shared by RunCycle and the autonomy goroutine, so every access is locked.
func (e *Executor) noteRecentAction(objectID string) {
	e.recentMu.Lock()
	defer e.recentMu.Unlock()
	if e.recentActions == nil {
		e.recentActions = make(map[string]time.Time)
	}
	e.recentActions[objectID] = time.Now()
}

// isCascadeCooldown returns true if an action was recently
// executed for the given object identifier.
func (e *Executor) isCascadeCooldown(objID string) bool {
	e.recentMu.Lock()
	t, ok := e.recentActions[objID]
	e.recentMu.Unlock()
	if !ok {
		return false
	}
	if time.Since(t) < e.cascadeCooldown() {
		e.logFn("executor",
			"cascade guard: skipping %q (action %v ago)",
			objID, time.Since(t),
		)
		return true
	}
	return false
}

// pruneRecentActions removes entries older than the cascade
// cooldown to prevent unbounded map growth.
func (e *Executor) pruneRecentActions() {
	maxAge := e.cascadeCooldown()
	e.recentMu.Lock()
	defer e.recentMu.Unlock()
	for k, t := range e.recentActions {
		if time.Since(t) > maxAge {
			delete(e.recentActions, k)
		}
	}
}

// operatorDecisionIntent marks ledger decisions recorded for operator-run
// actions (manual "take action" and approved queue items).
const operatorDecisionIntent = "operator_approved"
