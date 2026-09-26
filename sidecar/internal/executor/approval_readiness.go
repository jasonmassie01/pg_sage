package executor

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

const approvalMaxAttempts = 3

type ApprovalReadiness struct {
	Eligible     bool
	DeferReason  string
	Lifecycle    store.ActionLifecycleDecision
	Policy       ActionPolicyDecision
	PolicyKnown  bool
	LifecycleNow time.Time
}

func (e *Executor) ApprovalReadiness(
	action store.QueuedAction,
	now time.Time,
) ApprovalReadiness {
	return e.ApprovalReadinessWithEvidence(action, now, true)
}

func (e *Executor) ApprovalReadinessWithEvidence(
	action store.QueuedAction,
	now time.Time,
	evidencePresent bool,
) ApprovalReadiness {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	readiness := lifecycleReadiness(action, now, evidencePresent)
	if !readiness.Eligible {
		return readiness
	}
	contract, ok := ContractForQueuedAction(action)
	if !ok {
		return readinessForUnknownContract(readiness, action)
	}
	return e.withPolicyReadiness(readiness, contract, now)
}

func ContractForQueuedAction(action store.QueuedAction) (ActionContract, bool) {
	return ContractForActionType(actionTypeForReadiness(action))
}

func lifecycleReadiness(
	action store.QueuedAction,
	now time.Time,
	evidencePresent bool,
) ApprovalReadiness {
	readiness := ApprovalReadiness{Eligible: true, LifecycleNow: now}
	readiness.Lifecycle = store.EvaluateActionLifecycle(
		store.ActionLifecycleInput{
			Status:                 action.Status,
			ExpiresAt:              action.ExpiresAt,
			CooldownUntil:          action.CooldownUntil,
			AttemptCount:           action.AttemptCount,
			MaxAttempts:            approvalMaxAttempts,
			FailureFingerprint:     action.FailureFingerprint,
			LastFailureFingerprint: action.LastFailureFingerprint,
			EvidencePresent:        evidencePresent,
			Now:                    now,
		})
	if readiness.Lifecycle.State != store.ActionLifecycleReady {
		readiness.Eligible = false
		readiness.DeferReason = readiness.Lifecycle.BlockedReason
	}
	return readiness
}

func actionTypeForReadiness(action store.QueuedAction) string {
	if strings.TrimSpace(action.ActionType) != "" {
		return strings.TrimSpace(action.ActionType)
	}
	sql := strings.ToUpper(strings.TrimSpace(action.ProposedSQL))
	switch {
	case strings.HasPrefix(sql, "ANALYZE "):
		return "analyze_table"
	case strings.HasPrefix(sql, "CREATE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(sql, "DROP INDEX "):
		return "drop_unused_index"
	case strings.HasPrefix(sql, "ALTER TABLE "):
		return "alter_table"
	default:
		return ""
	}
}

func readinessForUnknownContract(
	readiness ApprovalReadiness,
	action store.QueuedAction,
) ApprovalReadiness {
	risk := strings.ToLower(strings.TrimSpace(action.ActionRisk))
	if risk == "moderate" || risk == "high" || risk == "high_risk" {
		readiness.Eligible = false
		readiness.DeferReason = "action contract is unavailable"
	}
	return readiness
}

func (e *Executor) withPolicyReadiness(
	readiness ApprovalReadiness,
	contract ActionContract,
	now time.Time,
) ApprovalReadiness {
	readiness.PolicyKnown = true
	cfg, mode, enabled := e.policySnapshot()
	policyCtx := ActionPolicyContext{
		Config: cfg, ExecutionMode: mode, ExecutorEnabled: &enabled,
		Now: now, RampStart: e.rampStart,
	}
	readiness.Policy = EvaluateActionPolicy(contract, policyCtx)
	if reason := operatorApprovalBlock(contract, policyCtx); reason != "" {
		readiness.Eligible = false
		readiness.DeferReason = reason
		return readiness
	}
	// An operator's explicit approval is not gated by automatic-execution
	// eligibility (trust ramp, tier3 flags, auto mode): present it as a
	// ready approval rather than the auto-execution verdict.
	if readiness.Policy.Decision != PolicyDecisionExecute {
		readiness.Policy = queueForApproval(readiness.Policy)
		readiness.Policy.BlockedReason = ""
	}
	return readiness
}

// operatorApprovalBlock returns why an operator-approved action may not run
// now: hard stops, observation/unknown trust, unsupported provider, or a
// configured maintenance window that is closed for moderate/high actions.
// An empty trust.maintenance_window does not block operator approvals.
func operatorApprovalBlock(contract ActionContract, ctx ActionPolicyContext) string {
	cfg := snapshotPolicyConfig(ctx.Config)
	if reason := hardBlockReason(contract, ctx, normalizedProvider(cfg)); reason != "" {
		return reason
	}
	if cfg == nil {
		return "execution policy is unavailable"
	}
	switch cfg.Trust.Level {
	case "observation":
		return "policy is observe_only"
	case "advisory", "autonomous":
	default:
		return "unknown trust level"
	}
	risky := contract.BaseRiskTier == "moderate" || contract.BaseRiskTier == "high"
	window := strings.TrimSpace(cfg.Trust.MaintenanceWindow)
	if risky && window != "" && !inMaintenanceWindowForPolicy(cfg, ctx.Now) {
		return "outside maintenance window"
	}
	return ""
}
