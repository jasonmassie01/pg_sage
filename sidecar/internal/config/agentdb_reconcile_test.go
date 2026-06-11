package config

import "testing"

// TestAgentDBReconcileIntervalDefault verifies the agent-DB reconcile
// interval gets its intended default (not 0, which would disable the
// reconciler) when no config file sets it (F4 / default-value masking).
func TestAgentDBReconcileIntervalDefault(t *testing.T) {
	cfg := newDefaults()
	if cfg.AgentDB.ReconcileIntervalSeconds != DefaultAgentDBReconcileInterval {
		t.Errorf("ReconcileIntervalSeconds = %d, want %d",
			cfg.AgentDB.ReconcileIntervalSeconds,
			DefaultAgentDBReconcileInterval)
	}
	if DefaultAgentDBReconcileInterval <= 0 {
		t.Error("default reconcile interval must be > 0 or the reconciler is disabled")
	}
}
