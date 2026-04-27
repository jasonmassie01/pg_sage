package store

import (
	"fmt"
	"time"
)

const (
	ActionLifecycleReady             = "ready"
	ActionLifecycleBlocked           = "blocked"
	ActionLifecycleExpired           = "expired"
	ActionLifecycleResolvedEphemeral = "resolved_ephemeral"
)

type ActionLifecycleInput struct {
	Status                 string
	ExpiresAt              time.Time
	CooldownUntil          *time.Time
	AttemptCount           int
	MaxAttempts            int
	FailureFingerprint     string
	LastFailureFingerprint string
	EvidencePresent        bool
	Now                    time.Time
}

type ActionLifecycleDecision struct {
	State             string     `json:"state"`
	BlockedReason     string     `json:"blocked_reason,omitempty"`
	Expired           bool       `json:"expired"`
	InCooldown        bool       `json:"in_cooldown"`
	CircuitOpen       bool       `json:"circuit_open"`
	ResolvedEphemeral bool       `json:"resolved_ephemeral"`
	NextAllowedAt     *time.Time `json:"next_allowed_at,omitempty"`
}

func EvaluateActionLifecycle(
	input ActionLifecycleInput,
) ActionLifecycleDecision {
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !input.EvidencePresent && actionNeedsEvidence(input.Status) {
		return ActionLifecycleDecision{
			State:             ActionLifecycleResolvedEphemeral,
			BlockedReason:     "underlying evidence disappeared",
			ResolvedEphemeral: true,
		}
	}
	if !input.ExpiresAt.IsZero() && !now.Before(input.ExpiresAt) {
		return ActionLifecycleDecision{
			State:         ActionLifecycleExpired,
			BlockedReason: "action proposal expired",
			Expired:       true,
		}
	}
	if input.CooldownUntil != nil && now.Before(*input.CooldownUntil) {
		return ActionLifecycleDecision{
			State:         ActionLifecycleBlocked,
			BlockedReason: "action is in cooldown",
			InCooldown:    true,
			NextAllowedAt: input.CooldownUntil,
		}
	}
	if repeatedFailureCircuitOpen(input) {
		return ActionLifecycleDecision{
			State:         ActionLifecycleBlocked,
			BlockedReason: repeatedFailureReason(input),
			CircuitOpen:   true,
		}
	}
	return ActionLifecycleDecision{State: ActionLifecycleReady}
}

func actionNeedsEvidence(status string) bool {
	switch status {
	case "pending", "approved", "waiting_for_approval", "queued":
		return true
	default:
		return false
	}
}

func repeatedFailureCircuitOpen(input ActionLifecycleInput) bool {
	if input.MaxAttempts <= 0 || input.AttemptCount < input.MaxAttempts {
		return false
	}
	if input.FailureFingerprint == "" {
		return false
	}
	return input.FailureFingerprint == input.LastFailureFingerprint
}

func repeatedFailureReason(input ActionLifecycleInput) string {
	return fmt.Sprintf(
		"repeated failure circuit breaker after %d attempts",
		input.AttemptCount,
	)
}
