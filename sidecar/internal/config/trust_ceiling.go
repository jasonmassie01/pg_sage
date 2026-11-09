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
// The threshold, window and cooldown ranges are the API's (value_ranges.go);
// a zero cascade cooldown keeps meaning "use the documented default".
func (t TrustConfig) validateCeiling() error {
	if t.CascadeCooldownCycles < 0 {
		return fmt.Errorf("trust.cascade_cooldown_cycles must be non-negative (got %d)",
			t.CascadeCooldownCycles)
	}
	return nil
}
