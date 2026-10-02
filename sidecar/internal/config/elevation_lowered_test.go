package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// LoweredElevation is what the startup WARN and the autonomy API/UI show:
// exactly the elevation settings faster than the spec, each with its
// value and the spec default. Raising a value, or making a gate stricter
// (io_baseline_days: 0), is not lowering.

func TestLoweredElevation_EachKnobReportedAlone(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		want   LoweredSetting
	}{
		{func(c *Config) { c.Trust.RampSafeHours = 1 },
			LoweredSetting{"trust.ramp_safe_hours", 1, 192, "hours"}},
		{func(c *Config) { c.Trust.RampModerateHours = 200 },
			LoweredSetting{"trust.ramp_moderate_hours", 200, 744, "hours"}},
		{func(c *Config) { c.Verify.IOBaselineHours = 2 },
			LoweredSetting{"verify.io_baseline_hours", 2, 168, "hours"}},
		{func(c *Config) { c.Verify.IOBaselineDays = 3 },
			LoweredSetting{"verify.io_baseline_days", 3, 7, "days"}},
		{func(c *Config) { c.SRE.Autonomy.EvaluateIntervalMinutes = 5 },
			LoweredSetting{"sre.autonomy.evaluate_interval_minutes", 5, 60, "minutes"}},
		{func(c *Config) { c.SRE.Autonomy.SafetyWindowDays = 7 },
			LoweredSetting{"sre.autonomy.safety_window_days", 7, 30, "days"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.ShadowWindowHours = 4 },
			LoweredSetting{"sre.autonomy.promotion.shadow_window_hours", 4, 720, "hours"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.ShadowMinReviewed = 3 },
			LoweredSetting{"sre.autonomy.promotion.shadow_min_reviewed", 3, 20, "packets"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.ShadowMinAcceptedPct = 90 },
			LoweredSetting{"sre.autonomy.promotion.shadow_min_accepted_pct", 90, 95, "percent"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.BenchMinTop1Pct = 79.5 },
			LoweredSetting{"sre.autonomy.promotion.bench_min_top1_pct", 79.5, 80, "percent"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.BenchMinPrecisionPct = 50 },
			LoweredSetting{"sre.autonomy.promotion.bench_min_precision_pct", 50, 90, "percent"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.MinSafePassPct = 94 },
			LoweredSetting{"sre.autonomy.promotion.min_safe_pass_pct", 94, 95, "percent"}},
		{func(c *Config) { c.SRE.Autonomy.Promotion.MinLiveRecoveries = 49 },
			LoweredSetting{"sre.autonomy.promotion.min_live_recoveries", 49, 50, "recoveries"}},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		c.mutate(cfg)
		got := cfg.LoweredElevation()
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: lowered = %+v, want exactly %+v", c.want.Key, got, c.want)
		}
	}
}

func TestLoweredElevation_RaisedOrEqualIsNotLowered(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 300, 1000
	cfg.Verify.IOBaselineDays = 7  // equal to the spec
	cfg.Verify.IOBaselineHours = 0 // unset
	cfg.SRE.Autonomy.EvaluateIntervalMinutes = 120
	cfg.SRE.Autonomy.SafetyWindowDays = 90
	p := &cfg.SRE.Autonomy.Promotion
	p.ShadowWindowHours, p.ShadowMinReviewed, p.MinLiveRecoveries = 1000, 40, 80
	p.ShadowMinAcceptedPct, p.BenchMinTop1Pct = 99, 90
	p.BenchMinPrecisionPct, p.MinSafePassPct = 95, 100
	if got := cfg.LoweredElevation(); len(got) != 0 {
		t.Fatalf("raised settings reported as lowered: %+v", got)
	}
	cfg.Verify.IOBaselineHours = 168 // a week in hours equals the spec
	if got := cfg.LoweredElevation(); len(got) != 0 {
		t.Fatalf("a 168-hour baseline reported as lowered: %+v", got)
	}
}

// Disabling the learned baseline makes admission unavailable (stricter),
// so it is not fast elevation.
func TestLoweredElevation_DisabledBaselineIsNotLowered(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Verify.IOBaselineDays = 0
	if got := cfg.LoweredElevation(); len(got) != 0 {
		t.Fatalf("io_baseline_days: 0 reported as lowered: %+v", got)
	}
}

func TestLoweredElevation_FastProfileListsEveryLoweredValueInOrder(t *testing.T) {
	cfg, err := loadRCAYAML(t, fastProfileYAML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.LoweredElevation()
	want := []string{"trust.ramp_safe_hours", "trust.ramp_moderate_hours",
		"verify.io_baseline_hours", "sre.autonomy.evaluate_interval_minutes",
		"sre.autonomy.promotion.shadow_window_hours",
		"sre.autonomy.promotion.shadow_min_reviewed",
		"sre.autonomy.promotion.min_live_recoveries"}
	if len(got) != len(want) {
		t.Fatalf("lowered = %+v, want keys %v", got, want)
	}
	for i, key := range want {
		if got[i].Key != key || got[i].Value >= got[i].Default {
			t.Errorf("lowered[%d] = %+v, want %s below its default", i, got[i], key)
		}
	}
}

func TestLoweredElevation_NilConfig(t *testing.T) {
	var cfg *Config
	if got := cfg.LoweredElevation(); got == nil || len(got) != 0 {
		t.Fatalf("nil config lowered = %#v, want an empty, non-nil list", got)
	}
}

func TestElevationKeysAreRestartBoundAndDocumented(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"trust.ramp_safe_hours", "trust.ramp_moderate_hours",
		"verify.io_baseline_hours", "sre.autonomy.promotion.shadow_window_hours",
		"sre.autonomy.promotion.shadow_min_reviewed",
		"sre.autonomy.promotion.shadow_min_accepted_pct",
		"sre.autonomy.promotion.bench_min_top1_pct",
		"sre.autonomy.promotion.bench_min_precision_pct",
		"sre.autonomy.promotion.min_safe_pass_pct",
		"sre.autonomy.promotion.min_live_recoveries"} {
		field, ok := LookupFieldLifecycle(key)
		if !ok || field.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart: lowering a gate must "+
				"go through the startup warning", key, field, ok)
		}
		if strings.TrimSpace(docs[key]) == "" {
			t.Errorf("%s has no doc tag", key)
		}
	}
}

// The documented dogfood profile (docs/configuration.md) and the commented
// block in config.example.yaml load as written and lower exactly the
// timing and volume settings, never the accuracy bar.
func TestDocumentedFastElevationProfileLoads(t *testing.T) {
	root := wave5RepoRoot(t)
	doc := wave5ReadFile(t, filepath.Join(root, "docs", "configuration.md"))
	fromDocs := between(t, doc, "<!-- fast-elevation-profile:start -->",
		"<!-- fast-elevation-profile:end -->")
	fromDocs = strings.TrimPrefix(strings.TrimSpace(fromDocs), "```yaml")
	fromDocs = strings.TrimSuffix(strings.TrimSpace(fromDocs), "```")
	example := wave5ReadFile(t, filepath.Join(root, "config.example.yaml"))
	block := between(t, example, "# --- fast elevation (dogfood) profile: start ---",
		"# --- fast elevation (dogfood) profile: end ---")
	var uncommented []string
	for _, line := range strings.Split(block, "\n") {
		if line = strings.TrimRight(line, "\r"); strings.HasPrefix(line, "# ") {
			uncommented = append(uncommented, strings.TrimPrefix(line, "# "))
		}
	}
	for name, body := range map[string]string{"docs": fromDocs,
		"config.example.yaml": strings.Join(uncommented, "\n") + "\n"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadRCAYAML(t, strings.TrimSpace(body)+"\n")
			if err != nil {
				t.Fatalf("profile does not load: %v\n%s", err, body)
			}
			assertDogfoodProfile(t, cfg)
		})
	}
}

func assertDogfoodProfile(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.Trust.RampSafeHours != 1 || cfg.Trust.RampModerateHours != 4 ||
		cfg.Verify.IOBaselineHours != 2 || cfg.SRE.Autonomy.EvaluateIntervalMinutes != 5 {
		t.Fatalf("profile timings = %+v / %+v", cfg.Trust, cfg.Verify)
	}
	p := cfg.SRE.Autonomy.Promotion
	if p.ShadowWindowHours != 4 || p.MinLiveRecoveries != 3 || p.ShadowMinReviewed != 3 {
		t.Fatalf("profile promotion = %+v", p)
	}
	spec := specPromotion()
	if p.ShadowMinAcceptedPct != spec.ShadowMinAcceptedPct ||
		p.BenchMinTop1Pct != spec.BenchMinTop1Pct ||
		p.BenchMinPrecisionPct != spec.BenchMinPrecisionPct ||
		p.MinSafePassPct != spec.MinSafePassPct {
		t.Fatalf("the dogfood profile lowers the accuracy bar: %+v", p)
	}
	if got := cfg.LoweredElevation(); len(got) != 7 {
		t.Fatalf("profile lowered %d settings, want 7: %+v", len(got), got)
	}
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	j := strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("markers %q .. %q not found", start, end)
	}
	return s[i+len(start) : j]
}
