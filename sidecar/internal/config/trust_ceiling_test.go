package config

import (
	"context"
	"strings"
	"testing"
)

// config-01: the operator's trust ceilings are live policy owned by the
// trust_policy owner; trust.tier3_high_risk (read by no executor) stays
// restart-bound.

var liveTrustCeilingPaths = []string{
	"trust.tier3_safe", "trust.tier3_moderate", "trust.maintenance_window",
	"trust.rollback_threshold_pct", "trust.rollback_window_minutes",
	"trust.rollback_cooldown_days", "trust.cascade_cooldown_cycles",
}

func TestTrustCeilingFieldsAreLivePolicy(t *testing.T) {
	for _, path := range append([]string{"trust.level"}, liveTrustCeilingPaths...) {
		meta, ok := LookupFieldLifecycle(path)
		if !ok || meta.Lifecycle != LifecycleLivePolicy || meta.Owner != "trust_policy" {
			t.Errorf("LookupFieldLifecycle(%q) = %+v, %v; want live_policy/trust_policy",
				path, meta, ok)
		}
	}
	for _, path := range []string{"trust.tier3_high_risk", "trust.ramp_safe_hours",
		"trust.ramp_moderate_hours", "trust.ramp_start"} {
		meta, ok := LookupFieldLifecycle(path)
		if !ok || meta.Lifecycle != LifecycleRestart || meta.Owner != "" {
			t.Errorf("LookupFieldLifecycle(%q) = %+v, %v; want restart", path, meta, ok)
		}
	}
}

func TestTrustCeilingWithoutOwnerFailsClosedToRestart(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	for _, path := range liveTrustCeilingPaths {
		if got, _ := controller.EffectiveLifecycle(path); got != LifecycleRestart {
			t.Errorf("unowned %s lifecycle = %q, want restart", path, got)
		}
	}
}

func TestTrustCeilingApplyIsLiveNotPendingRestart(t *testing.T) {
	owner := &ownerStub{name: "trust_policy"}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	candidate := controller.Desired().Config
	candidate.Trust.Tier3Safe = !candidate.Trust.Tier3Safe
	candidate.Trust.MaintenanceWindow = "never"
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.PendingRestart) != 0 {
		t.Fatalf("pending_restart = %v, want none", result.PendingRestart)
	}
	if !containsPath(result.Applied, "trust.tier3_safe") ||
		!containsPath(result.Applied, "trust.maintenance_window") {
		t.Fatalf("applied = %v, want both ceiling fields", result.Applied)
	}
	if owner.prepared == nil || owner.prepared.commit != 1 {
		t.Fatal("the trust_policy owner was not prepared and committed")
	}
	if result.ComponentStatus["trust_policy"] != "applied" {
		t.Fatalf("component status = %v", result.ComponentStatus)
	}
	if got := controller.Active().Config.Trust.MaintenanceWindow; got != "never" {
		t.Fatalf("active window = %q, want never", got)
	}
}

func TestTrustHighRiskStaysRestartBound(t *testing.T) {
	owner := &ownerStub{name: "trust_policy"}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	candidate := controller.Desired().Config
	candidate.Trust.Tier3HighRisk = !candidate.Trust.Tier3HighRisk
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !containsPath(result.PendingRestart, "trust.tier3_high_risk") || owner.prepared != nil {
		t.Fatalf("tier3_high_risk result = %+v (owner prepared %t), want pending restart",
			result, owner.prepared != nil)
	}
}

func TestTrustCeilingValidationRejectsBeforePrepare(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*TrustConfig)
		field string
	}{
		{"negative threshold", func(c *TrustConfig) { c.RollbackThresholdPct = -1 },
			"trust.rollback_threshold_pct"},
		{"threshold above 100", func(c *TrustConfig) { c.RollbackThresholdPct = 101 },
			"trust.rollback_threshold_pct"},
		{"negative window", func(c *TrustConfig) { c.RollbackWindowMinutes = -1 },
			"trust.rollback_window_minutes"},
		{"negative cooldown", func(c *TrustConfig) { c.RollbackCooldownDays = -1 },
			"trust.rollback_cooldown_days"},
		{"negative cascade", func(c *TrustConfig) { c.CascadeCooldownCycles = -1 },
			"trust.cascade_cooldown_cycles"},
		{"invalid window", func(c *TrustConfig) { c.MaintenanceWindow = "sometimes" },
			"trust.maintenance_window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := &ownerStub{name: "trust_policy"}
			controller := NewConfigController(DefaultConfig(), nil, owner)
			candidate := controller.Desired().Config
			tc.set(&candidate.Trust)
			_, err := controller.Apply(context.Background(), 1, candidate)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("apply error = %v, want it to name %s", err, tc.field)
			}
			if owner.prepared != nil {
				t.Fatal("an invalid ceiling reached Prepare")
			}
			if controller.Desired().Generation != 1 {
				t.Fatal("an invalid ceiling advanced the desired generation")
			}
		})
	}
}

func TestTrustCeilingValidationBoundaries(t *testing.T) {
	for _, pct := range []int{0, 100} {
		cfg := DefaultConfig()
		cfg.Trust.RollbackThresholdPct = pct
		// config-02: window and cooldown share the API's 1.. range; the
		// cascade cooldown keeps 0 = default.
		cfg.Trust.RollbackWindowMinutes, cfg.Trust.RollbackCooldownDays = 1, 1
		cfg.Trust.CascadeCooldownCycles = 0
		if err := cfg.validate(); err != nil {
			t.Errorf("rollback_threshold_pct=%d with minimum bounds rejected: %v", pct, err)
		}
	}
}
