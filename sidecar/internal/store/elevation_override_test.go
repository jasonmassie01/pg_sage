package store

import (
	"strings"
	"testing"
)

// Fast elevation settings loosen trust gates, so the persisted override
// path (API config writes) refuses them: shortening a ramp or lowering the
// promotion bar takes a YAML change and a restart, which logs the
// fast-elevation WARN.
func TestElevationKeysAreNotRuntimeOverrides(t *testing.T) {
	for _, key := range []string{"trust.ramp_safe_hours", "trust.ramp_moderate_hours",
		"verify.io_baseline_hours", "verify.drop_window_hours",
		"sre.autonomy.promotion.shadow_window_hours",
		"sre.autonomy.promotion.shadow_min_reviewed",
		"sre.autonomy.promotion.shadow_min_accepted_pct",
		"sre.autonomy.promotion.bench_min_top1_pct",
		"sre.autonomy.promotion.bench_min_precision_pct",
		"sre.autonomy.promotion.min_safe_pass_pct",
		"sre.autonomy.promotion.min_live_recoveries"} {
		err := ValidateConfigOverride(key, "1")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("ValidateConfigOverride(%q) = %v, want a refusal naming the key", key, err)
		}
		if !excludedExactKeys[key] {
			t.Errorf("%s is not registered as a YAML-only key", key)
		}
	}
}
