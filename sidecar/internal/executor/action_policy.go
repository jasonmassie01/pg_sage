package executor

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

const (
	PolicyDecisionExecute       = "execute"
	PolicyDecisionQueueApproval = "queue_for_approval"
	PolicyDecisionBlocked       = "blocked"
	PolicyDecisionObserveOnly   = "observe_only"
	PolicyDecisionParked        = "parked"
)

type ActionPolicyDecision struct {
	Decision                  string   `json:"decision"`
	RiskTier                  string   `json:"risk_tier"`
	RequiresApproval          bool     `json:"requires_approval"`
	RequiresMaintenanceWindow bool     `json:"requires_maintenance_window"`
	BlockedReason             string   `json:"blocked_reason,omitempty"`
	Detail                    string   `json:"detail,omitempty"`
	Guardrails                []string `json:"guardrails,omitempty"`
	Provider                  string   `json:"provider,omitempty"`
	EvidenceID                string   `json:"evidence_id,omitempty"`
	DecisionID                int64    `json:"decision_id,omitempty"`
	// LockCeilingMS is the policy lock ceiling carried from an execute
	// verdict; in-transaction DDL caps lock_timeout by it (0 = none).
	LockCeilingMS int64 `json:"lock_ceiling_ms,omitempty"`
	// SerializeMode is the policy's serialize_mode carried from an execute
	// verdict: a change lease conflict parks (or refuses an operator) or
	// waits in the lease queue.
	SerializeMode string `json:"serialize_mode,omitempty"`
}

func inMaintenanceWindowForPolicy(
	cfg *config.Config,
	now time.Time,
) bool {
	if cfg == nil {
		return false
	}
	window := strings.TrimSpace(cfg.Trust.MaintenanceWindow)
	if window == "" {
		return false
	}
	if now.IsZero() {
		return inMaintenanceWindow(window)
	}
	return inMaintenanceWindowAt(window, now)
}
