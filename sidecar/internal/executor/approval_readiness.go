package executor

import (
	"context"
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
	if actionID, ok := restoreQueueActionID(action); ok {
		return e.dropRestoreReadiness(action, now, actionID)
	}
	if IsRevertCard(action) {
		return e.revertReadiness(action, now)
	}
	readiness := lifecycleReadiness(action, now, evidencePresent)
	if !readiness.Eligible {
		return readiness
	}
	if _, ok := ContractForQueuedAction(action); !ok {
		return readinessForUnknownContract(readiness, action)
	}
	return withPolicyReadiness(readiness,
		e.explainOperatorAction(context.Background(), action.ProposedSQL))
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
	case IsIndexReplaceSQL(action.ProposedSQL):
		return ActionTypeReplaceIndex
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

// withPolicyReadiness applies the gate's explanation of an operator
// approval of the item.
func withPolicyReadiness(
	readiness ApprovalReadiness,
	decision ActionPolicyDecision,
) ApprovalReadiness {
	readiness.PolicyKnown = true
	readiness.Policy = decision
	if readiness.Policy.Decision != PolicyDecisionExecute {
		readiness.Eligible = false
		readiness.DeferReason = humanPolicyReason(readiness.Policy)
		return readiness
	}
	// Present an authorizable operator approval as a ready approval.
	readiness.Policy.Decision = PolicyDecisionQueueApproval
	readiness.Policy.RequiresApproval = true
	readiness.Policy.BlockedReason = ""
	return readiness
}
