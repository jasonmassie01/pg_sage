package config

// LoweredSetting is one elevation setting below its spec default.
type LoweredSetting struct {
	Key     string  `json:"key"`
	Value   float64 `json:"value"`
	Default float64 `json:"default"`
	Unit    string  `json:"unit"`
}

// LoweredElevation lists, in a stable order, every trust-elevation
// setting faster than the spec: a shorter trust ramp, IO baseline, shadow
// window, evaluation interval or safety window, or a lower promotion bar.
// Raising a value or disabling the learned baseline (stricter) is not
// lowering. It is never nil.
func (c *Config) LoweredElevation() []LoweredSetting {
	out := []LoweredSetting{}
	if c == nil {
		return out
	}
	add := func(key string, value, def float64, unit string) {
		if value < def {
			out = append(out, LoweredSetting{Key: key, Value: value, Default: def, Unit: unit})
		}
	}
	add("trust.ramp_safe_hours", float64(c.Trust.RampSafeHours), DefaultRampSafeHours,
		"hours")
	add("trust.ramp_moderate_hours", float64(c.Trust.RampModerateHours),
		DefaultRampModerateHours, "hours")
	if c.Verify.IOBaselineHours > 0 {
		add("verify.io_baseline_hours", float64(c.Verify.IOBaselineHours),
			DefaultIOBaselineDays*24, "hours")
	} else if c.Verify.IOBaselineDays > 0 {
		add("verify.io_baseline_days", float64(c.Verify.IOBaselineDays),
			DefaultIOBaselineDays, "days")
	}
	add("verify.drop_window_hours", float64(c.Verify.DropWindowHours),
		DefaultVerifyDropWindowHours, "hours")
	a, def := c.SRE.Autonomy, defaultSREAutonomyConfig()
	add("sre.autonomy.evaluate_interval_minutes", float64(a.EvaluateIntervalMinutes),
		float64(def.EvaluateIntervalMinutes), "minutes")
	add("sre.autonomy.safety_window_days", float64(a.SafetyWindowDays),
		float64(def.SafetyWindowDays), "days")
	p, dp := a.Promotion, def.Promotion
	const pre = "sre.autonomy.promotion."
	add(pre+"shadow_window_hours", float64(p.ShadowWindowHours),
		float64(dp.ShadowWindowHours), "hours")
	add(pre+"shadow_min_reviewed", float64(p.ShadowMinReviewed),
		float64(dp.ShadowMinReviewed), "packets")
	add(pre+"shadow_min_accepted_pct", p.ShadowMinAcceptedPct, dp.ShadowMinAcceptedPct,
		"percent")
	add(pre+"bench_min_top1_pct", p.BenchMinTop1Pct, dp.BenchMinTop1Pct, "percent")
	add(pre+"bench_min_precision_pct", p.BenchMinPrecisionPct, dp.BenchMinPrecisionPct,
		"percent")
	add(pre+"min_safe_pass_pct", p.MinSafePassPct, dp.MinSafePassPct, "percent")
	add(pre+"min_live_recoveries", float64(p.MinLiveRecoveries),
		float64(dp.MinLiveRecoveries), "recoveries")
	return out
}
