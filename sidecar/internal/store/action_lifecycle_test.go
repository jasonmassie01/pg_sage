package store

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateActionLifecycleExpiresStalePendingAction(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	decision := EvaluateActionLifecycle(ActionLifecycleInput{
		Status:          "pending",
		ExpiresAt:       now.Add(-time.Minute),
		EvidencePresent: true,
		Now:             now,
	})

	if decision.State != ActionLifecycleExpired {
		t.Fatalf("state = %q, want %q", decision.State, ActionLifecycleExpired)
	}
	if !decision.Expired {
		t.Fatalf("expired = false, want true")
	}
	if decision.BlockedReason == "" {
		t.Fatalf("blocked reason is empty")
	}
}

func TestEvaluateActionLifecycleMarksEphemeralWhenEvidenceDisappears(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	decision := EvaluateActionLifecycle(ActionLifecycleInput{
		Status:          "waiting_for_approval",
		ExpiresAt:       now.Add(time.Hour),
		EvidencePresent: false,
		Now:             now,
	})

	if decision.State != ActionLifecycleResolvedEphemeral {
		t.Fatalf("state = %q, want %q",
			decision.State, ActionLifecycleResolvedEphemeral)
	}
	if !decision.ResolvedEphemeral {
		t.Fatalf("resolved ephemeral = false, want true")
	}
	if !strings.Contains(decision.BlockedReason, "evidence") {
		t.Fatalf("blocked reason %q does not mention evidence",
			decision.BlockedReason)
	}
}

func TestEvaluateActionLifecycleBlocksDuringCooldown(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	cooldownUntil := now.Add(30 * time.Minute)
	decision := EvaluateActionLifecycle(ActionLifecycleInput{
		Status:          "pending",
		ExpiresAt:       now.Add(time.Hour),
		CooldownUntil:   &cooldownUntil,
		EvidencePresent: true,
		Now:             now,
	})

	if decision.State != ActionLifecycleBlocked {
		t.Fatalf("state = %q, want %q",
			decision.State, ActionLifecycleBlocked)
	}
	if !decision.InCooldown {
		t.Fatalf("in cooldown = false, want true")
	}
	if decision.NextAllowedAt == nil ||
		!decision.NextAllowedAt.Equal(cooldownUntil) {
		t.Fatalf("next allowed at = %v, want %v",
			decision.NextAllowedAt, cooldownUntil)
	}
}

func TestEvaluateActionLifecycleTripsRepeatedFailureCircuit(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	decision := EvaluateActionLifecycle(ActionLifecycleInput{
		Status:                 "failed",
		ExpiresAt:              now.Add(time.Hour),
		AttemptCount:           3,
		MaxAttempts:            3,
		FailureFingerprint:     "permission denied: analyze public.orders",
		LastFailureFingerprint: "permission denied: analyze public.orders",
		EvidencePresent:        true,
		Now:                    now,
	})

	if decision.State != ActionLifecycleBlocked {
		t.Fatalf("state = %q, want %q",
			decision.State, ActionLifecycleBlocked)
	}
	if !decision.CircuitOpen {
		t.Fatalf("circuit open = false, want true")
	}
	if !strings.Contains(decision.BlockedReason, "repeated failure") {
		t.Fatalf("blocked reason %q does not mention repeated failure",
			decision.BlockedReason)
	}
}
