package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// queuedActionMap converts a QueuedAction to a JSON-friendly map.
func queuedActionMap(a store.QueuedAction) map[string]any {
	m := map[string]any{
		"id":           a.ID,
		"database_id":  a.DatabaseID,
		"finding_id":   a.FindingID,
		"proposed_sql": a.ProposedSQL,
		"rollback_sql": a.RollbackSQL,
		"action_risk":  a.ActionRisk,
		"status":       a.Status,
		"proposed_at":  a.ProposedAt,
		"decided_by":   a.DecidedBy,
		"decided_at":   a.DecidedAt,
		"expires_at":   a.ExpiresAt,
		"reason":       a.Reason,
	}
	addLifecycleFields(m, a)
	addRecommendationFields(m, a)
	addScheduleFields(m, a)
	return m
}

func queuedActionMapWithReadiness(
	a store.QueuedAction,
	exec *executor.Executor,
) map[string]any {
	m := queuedActionMap(a)
	if exec == nil {
		return m
	}
	readiness := exec.ApprovalReadiness(a, time.Now().UTC())
	m["eligible"] = readiness.Eligible
	if readiness.DeferReason != "" {
		m["defer_reason"] = readiness.DeferReason
		m["blocked_reason"] = readiness.DeferReason
	}
	if readiness.PolicyKnown {
		m["policy_detail"] = readiness.Policy
	}
	if contract, ok := executor.ContractForQueuedAction(a); ok {
		m["rollback_class"] = contract.RollbackClass
	}
	m["lifecycle_detail"] = readiness.Lifecycle
	return m
}

func addLifecycleFields(m map[string]any, a store.QueuedAction) {
	if a.ActionType != "" {
		m["action_type"] = a.ActionType
	}
	if a.IdentityKey != "" {
		m["identity_key"] = a.IdentityKey
	}
	if a.PolicyDecision != "" {
		m["policy_decision"] = a.PolicyDecision
	}
	if len(a.Guardrails) > 0 {
		m["guardrails"] = a.Guardrails
	}
	if a.AttemptCount > 0 {
		m["attempt_count"] = a.AttemptCount
	}
	if a.LastAttemptAt != nil {
		m["last_attempt_at"] = a.LastAttemptAt
	}
	if a.CooldownUntil != nil {
		m["cooldown_until"] = a.CooldownUntil
	}
	if a.FailureFingerprint != "" {
		m["failure_fingerprint"] = a.FailureFingerprint
	}
	if a.LastFailureFingerprint != "" {
		m["last_failure_fingerprint"] = a.LastFailureFingerprint
	}
	if a.VerificationStatus != "" {
		m["verification_status"] = a.VerificationStatus
	}
	if a.ShadowToilMinutes > 0 {
		m["shadow_toil_minutes"] = a.ShadowToilMinutes
	}
	if a.ActionLogID != nil {
		m["action_log_id"] = *a.ActionLogID
	}
	if script := actionScriptOutput(a); script != nil {
		m["output_modes"] = []string{"queue_for_approval", "generate_pr_or_script"}
		m["script_output"] = script
	}
}

func actionScriptOutput(a store.QueuedAction) map[string]any {
	if !isDDLScriptAction(a) || strings.TrimSpace(a.ProposedSQL) == "" {
		return nil
	}
	actionType := strings.TrimSpace(a.ActionType)
	if actionType == "" {
		actionType = "ddl_action"
	}
	return map[string]any{
		"filename":         strconv.Itoa(a.FindingID) + "_" + actionType + ".sql",
		"migration_sql":    a.ProposedSQL,
		"rollback_sql":     a.RollbackSQL,
		"verification_sql": actionVerificationSQL(a),
		"pr_title":         "Review pg_sage action: " + actionType,
		"pr_body":          actionPRBody(a),
		"risk_labels":      []string{a.ActionRisk, actionType},
		"format":           "sql",
	}
}

func actionVerificationSQL(a store.QueuedAction) []string {
	switch strings.TrimSpace(a.ActionType) {
	case "create_index_concurrently":
		return []string{
			"-- Verify pg_index.indisvalid and indisready for the new index.",
			"-- Rerun EXPLAIN for the affected query.",
		}
	case "drop_unused_index":
		return []string{
			"-- Verify the index is absent with to_regclass().",
			"-- Watch application errors and query latency after deploy.",
		}
	default:
		return []string{
			"-- Run the case verification query before merging.",
			"-- Rerun pg_sage analyzers after deployment.",
		}
	}
}

func actionPRBody(a store.QueuedAction) string {
	return "Generated from pg_sage finding " + strconv.Itoa(a.FindingID) +
		". Review migration SQL, rollback or mitigation, and verification SQL."
}

func isDDLScriptAction(a store.QueuedAction) bool {
	actionType := strings.ToLower(strings.TrimSpace(a.ActionType))
	if strings.Contains(actionType, "index") ||
		strings.Contains(actionType, "ddl") ||
		strings.Contains(actionType, "alter") ||
		strings.Contains(actionType, "reindex") {
		return true
	}
	sql := strings.ToUpper(strings.TrimSpace(a.ProposedSQL))
	return strings.HasPrefix(sql, "CREATE ") ||
		strings.HasPrefix(sql, "ALTER ") ||
		strings.HasPrefix(sql, "DROP ") ||
		strings.HasPrefix(sql, "REINDEX ")
}
