package config

import "fmt"

// liveTrustCeilingFields are the operator's trust ceilings: the executor
// adopts them on its next authorization through the trust_policy owner, so
// lowering one takes autonomy back without a restart (config-01).
// trust.tier3_high_risk is deliberately absent: no executor reads it, and
// HIGH-risk actions always need approval.
var liveTrustCeilingFields = map[string]bool{
	"trust.tier3_safe":              true,
	"trust.tier3_moderate":          true,
	"trust.maintenance_window":      true,
	"trust.rollback_threshold_pct":  true,
	"trust.rollback_window_minutes": true,
	"trust.rollback_cooldown_days":  true,
	"trust.cascade_cooldown_cycles": true,
}

// validateCeiling rejects ceiling values a running executor cannot adopt.
// Zero keeps meaning "use the documented default" where a reader has one.
func (t TrustConfig) validateCeiling() error {
	if t.RollbackThresholdPct < 0 || t.RollbackThresholdPct > 100 {
		return fmt.Errorf("trust.rollback_threshold_pct must be 0-100 (got %d)",
			t.RollbackThresholdPct)
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"trust.rollback_window_minutes", t.RollbackWindowMinutes},
		{"trust.rollback_cooldown_days", t.RollbackCooldownDays},
		{"trust.cascade_cooldown_cycles", t.CascadeCooldownCycles},
	} {
		if field.value < 0 {
			return fmt.Errorf("%s must be non-negative (got %d)", field.name, field.value)
		}
	}
	return nil
}
