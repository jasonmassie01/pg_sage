package executor

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
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
	readiness.Policy = EvaluateActionPolicy(contract, ActionPolicyContext{
		Config:        e.configForPolicy(),
		ExecutionMode: e.ExecutionMode(),
		Now:           now,
		RampStart:     e.rampStart,
	})
	if readiness.Policy.BlockedReason != "" {
		readiness.Eligible = false
		readiness.DeferReason = readiness.Policy.BlockedReason
		return readiness
	}
	if readiness.Policy.Decision == PolicyDecisionBlocked ||
		readiness.Policy.Decision == PolicyDecisionObserveOnly {
		readiness.Eligible = false
		readiness.DeferReason = "policy does not allow execution"
	}
	return readiness
}

func (e *Executor) configForPolicy() *config.Config {
	if e == nil {
		return nil
	}
	return e.cfg
}
