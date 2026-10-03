package config

import (
	"fmt"
	"time"
)

// Fast elevation: the timers and thresholds that gate trust elevation are
// configurable so an operator can let a dogfood database elevate in hours.
// The spec values are the defaults, every knob has a validated minimum
// (none accepts 0 to mean "skip"), and LoweredElevation lists each value
// below the spec so the startup WARN and the autonomy API/UI show it.

// Spec defaults for the elevation settings.
const (
	DefaultRampSafeHours     = 192 // 8 days
	DefaultRampModerateHours = 744 // 31 days
	maxElevationHours        = 8760
	minAccuracyPct           = 50
)

// SREPromotionConfig is the earned-autonomy promotion bar (AI-SRE-SPEC
// §7.3). Irreversible classes, L4, admin approval and the downgrade
// signals are not configurable.
type SREPromotionConfig struct {
	ShadowWindowHours    int     `yaml:"shadow_window_hours" doc:"Hours of shadow reviews (from the first review) a family needs for L2, and the window its reviewed and accepted packets are counted in, 1-8760. Default: 720 (30 days)." warning:"Lower values let pg_sage earn L2 on less operator review."`
	ShadowMinReviewed    int     `yaml:"shadow_min_reviewed" doc:"Reviewed packets needed inside the shadow window for L2, 3-1000. Default: 20."`
	ShadowMinAcceptedPct float64 `yaml:"shadow_min_accepted_pct" doc:"Operator-accepted share of reviewed packets for L2, 50-100. Default: 95."`
	BenchMinTop1Pct      float64 `yaml:"bench_min_top1_pct" doc:"PGIncidentBench top-1 accuracy on every gated arm for L2, 50-100. Default: 80."`
	BenchMinPrecisionPct float64 `yaml:"bench_min_precision_pct" doc:"PGIncidentBench factual (mechanism) precision on every gated arm for L2, 50-100. Default: 90."`
	MinSafePassPct       float64 `yaml:"min_safe_pass_pct" doc:"Safe Pass on the family's bench fault programs and game days for L3, 50-100. Default: 95."`
	MinLiveRecoveries    int     `yaml:"min_live_recoveries" doc:"Verified live recoveries at L2 a pair needs for L3, 1-10000. Default: 50." warning:"Lower values let pg_sage auto-execute after fewer verified recoveries."`
}

func defaultSREPromotionConfig() SREPromotionConfig {
	return SREPromotionConfig{ShadowWindowHours: 720, ShadowMinReviewed: 20,
		ShadowMinAcceptedPct: 95, BenchMinTop1Pct: 80, BenchMinPrecisionPct: 90,
		MinSafePassPct: 95, MinLiveRecoveries: 50}
}

// ShadowWindow is the shadow-review window.
func (p SREPromotionConfig) ShadowWindow() time.Duration {
	return time.Duration(p.ShadowWindowHours) * time.Hour
}

func (p SREPromotionConfig) validate() error {
	for _, c := range []rangeCheck{
		{"shadow_window_hours", p.ShadowWindowHours, 1, maxElevationHours},
		{"shadow_min_reviewed", p.ShadowMinReviewed, 3, 1000},
		{"min_live_recoveries", p.MinLiveRecoveries, 1, 10000},
	} {
		if c.value < c.lo || c.value > c.hi {
			return fmt.Errorf("sre.autonomy.promotion.%s must be %d-%d, got %d",
				c.key, c.lo, c.hi, c.value)
		}
	}
	for _, c := range []struct {
		key   string
		value float64
	}{
		{"shadow_min_accepted_pct", p.ShadowMinAcceptedPct},
		{"bench_min_top1_pct", p.BenchMinTop1Pct},
		{"bench_min_precision_pct", p.BenchMinPrecisionPct},
		{"min_safe_pass_pct", p.MinSafePassPct},
	} {
		// The negated form also rejects NaN.
		if !(c.value >= minAccuracyPct && c.value <= 100) {
			return fmt.Errorf("sre.autonomy.promotion.%s must be %d-100, got %v",
				c.key, minAccuracyPct, c.value)
		}
	}
	return nil
}

// SafeRamp is trust.ramp_safe_hours as a duration (0 when unset; the
// gate then applies the spec ramp).
func (t TrustConfig) SafeRamp() time.Duration {
	return time.Duration(t.RampSafeHours) * time.Hour
}

// ModerateRamp is trust.ramp_moderate_hours as a duration.
func (t TrustConfig) ModerateRamp() time.Duration {
	return time.Duration(t.RampModerateHours) * time.Hour
}

// EffectiveIOBaselineDays is the learned-baseline observation admission
// waits for: io_baseline_hours when set, else io_baseline_days (0
// disables the learned baseline).
func (v VerifyConfig) EffectiveIOBaselineDays() float64 {
	if v.IOBaselineHours > 0 {
		return float64(v.IOBaselineHours) / 24
	}
	return float64(v.IOBaselineDays)
}

// validateElevation checks the trust ramp and the hour-scale baseline.
func (c *Config) validateElevation() error {
	t := c.Trust
	if t.RampSafeHours < 1 || t.RampSafeHours > maxElevationHours {
		return fmt.Errorf("trust.ramp_safe_hours must be 1-%d, got %d",
			maxElevationHours, t.RampSafeHours)
	}
	if t.RampModerateHours < 1 || t.RampModerateHours > maxElevationHours {
		return fmt.Errorf("trust.ramp_moderate_hours must be 1-%d, got %d",
			maxElevationHours, t.RampModerateHours)
	}
	if t.RampModerateHours < t.RampSafeHours {
		return fmt.Errorf("trust.ramp_moderate_hours (%d) must be at least "+
			"trust.ramp_safe_hours (%d)", t.RampModerateHours, t.RampSafeHours)
	}
	if err := c.Verify.validateDropWindow(); err != nil {
		return err
	}
	h := c.Verify.IOBaselineHours
	if h < 0 || h > maxElevationHours {
		return fmt.Errorf("verify.io_baseline_hours must be 0 (use io_baseline_days) "+
			"or 1-%d, got %d", maxElevationHours, h)
	}
	if h > c.Verify.IOSampleDays*24 {
		return fmt.Errorf("verify.io_sample_retention_days (%d) must cover "+
			"verify.io_baseline_hours (%d)", c.Verify.IOSampleDays, h)
	}
	return nil
}
