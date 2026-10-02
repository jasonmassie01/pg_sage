package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Fast elevation: every timer and threshold that gates trust elevation
// (the trust ramp, the learned IO baseline, the earned-autonomy promotion
// bar) is configurable so a dogfood database can elevate in hours. The
// spec values are the defaults: no file, a partial section or an absent
// key never changes behaviour, and no knob accepts 0 to mean "skip".

func specPromotion() SREPromotionConfig {
	return SREPromotionConfig{ShadowWindowHours: 720, ShadowMinReviewed: 20,
		ShadowMinAcceptedPct: 95, BenchMinTop1Pct: 80, BenchMinPrecisionPct: 90,
		MinSafePassPct: 95, MinLiveRecoveries: 50}
}

func assertSpecElevation(t *testing.T, cfg *Config) {
	t.Helper()
	tr := cfg.Trust
	if tr.RampSafeHours != 192 || tr.RampModerateHours != 744 {
		t.Errorf("trust ramp = %d/%d hours, want 192/744", tr.RampSafeHours,
			tr.RampModerateHours)
	}
	if tr.SafeRamp() != 8*24*time.Hour || tr.ModerateRamp() != 31*24*time.Hour {
		t.Errorf("trust ramp durations = %s/%s, want 8d/31d", tr.SafeRamp(),
			tr.ModerateRamp())
	}
	if cfg.Verify.IOBaselineHours != 0 || cfg.Verify.EffectiveIOBaselineDays() != 7 {
		t.Errorf("IO baseline = %dh / %v days, want unset / 7", cfg.Verify.IOBaselineHours,
			cfg.Verify.EffectiveIOBaselineDays())
	}
	p := cfg.SRE.Autonomy.Promotion
	if p != specPromotion() {
		t.Errorf("promotion = %+v, want the spec %+v", p, specPromotion())
	}
	if p.ShadowWindow() != 30*24*time.Hour {
		t.Errorf("shadow window = %s, want 720h", p.ShadowWindow())
	}
	if got := cfg.LoweredElevation(); len(got) != 0 {
		t.Errorf("spec defaults reported as lowered: %+v", got)
	}
}

func TestElevationDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertSpecElevation(t, cfg)
}

func TestElevationDefaults_DefaultConfig(t *testing.T) {
	assertSpecElevation(t, DefaultConfig())
}

func TestElevationDefaults_PartialSectionsKeepTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "trust:\n  level: advisory\n"+
		"verify:\n  min_samples: 5\n"+
		"sre:\n  autonomy:\n    promotion:\n      min_live_recoveries: 60\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trust.RampSafeHours != 192 || cfg.Trust.RampModerateHours != 744 {
		t.Fatalf("partial trust section masked the ramp: %+v", cfg.Trust)
	}
	if cfg.Verify.EffectiveIOBaselineDays() != 7 {
		t.Fatalf("partial verify section masked the baseline: %+v", cfg.Verify)
	}
	want := specPromotion()
	want.MinLiveRecoveries = 60
	if cfg.SRE.Autonomy.Promotion != want {
		t.Fatalf("promotion = %+v, want %+v", cfg.SRE.Autonomy.Promotion, want)
	}
}

const fastProfileYAML = `trust:
  ramp_safe_hours: 1
  ramp_moderate_hours: 4
verify:
  io_baseline_hours: 2
sre:
  autonomy:
    evaluate_interval_minutes: 5
    promotion:
      shadow_window_hours: 4
      shadow_min_reviewed: 3
      min_live_recoveries: 3
`

func TestElevationKnobsHonoured(t *testing.T) {
	cfg, err := loadRCAYAML(t, fastProfileYAML+
		"      shadow_min_accepted_pct: 90\n      bench_min_top1_pct: 75\n"+
		"      bench_min_precision_pct: 85.5\n      min_safe_pass_pct: 92\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trust.SafeRamp() != time.Hour || cfg.Trust.ModerateRamp() != 4*time.Hour {
		t.Fatalf("ramp = %s/%s, want 1h/4h", cfg.Trust.SafeRamp(), cfg.Trust.ModerateRamp())
	}
	if got := cfg.Verify.EffectiveIOBaselineDays(); math.Abs(got-2.0/24) > 1e-12 {
		t.Fatalf("effective IO baseline = %v days, want 2h", got)
	}
	want := SREPromotionConfig{ShadowWindowHours: 4, ShadowMinReviewed: 3,
		ShadowMinAcceptedPct: 90, BenchMinTop1Pct: 75, BenchMinPrecisionPct: 85.5,
		MinSafePassPct: 92, MinLiveRecoveries: 3}
	p := cfg.SRE.Autonomy.Promotion
	if p != want || p.ShadowWindow() != 4*time.Hour {
		t.Fatalf("promotion = %+v (%s), want %+v", p, p.ShadowWindow(), want)
	}
	if cfg.SRE.Autonomy.EvaluateInterval() != 5*time.Minute {
		t.Fatalf("evaluate interval = %s", cfg.SRE.Autonomy.EvaluateInterval())
	}
}

func TestElevationBoundaries(t *testing.T) {
	cases := []struct {
		name, yaml, wantKey string
	}{
		{"safe ramp 0", "trust:\n  ramp_safe_hours: 0\n", "trust.ramp_safe_hours"},
		{"safe ramp negative", "trust:\n  ramp_safe_hours: -1\n", "trust.ramp_safe_hours"},
		{"safe ramp 1", "trust:\n  ramp_safe_hours: 1\n", ""},
		{"safe ramp max", "trust:\n  ramp_safe_hours: 744\n", ""},
		{"safe ramp over max", "trust:\n  ramp_safe_hours: 8761\n  ramp_moderate_hours: 8761\n",
			"trust.ramp_safe_hours"},
		{"moderate ramp 0", "trust:\n  ramp_safe_hours: 1\n  ramp_moderate_hours: 0\n",
			"trust.ramp_moderate_hours"},
		{"moderate ramp max", "trust:\n  ramp_moderate_hours: 8760\n", ""},
		{"moderate ramp over max", "trust:\n  ramp_moderate_hours: 8761\n",
			"trust.ramp_moderate_hours"},
		{"moderate below safe", "trust:\n  ramp_safe_hours: 10\n  ramp_moderate_hours: 9\n",
			"trust.ramp_moderate_hours"},
		{"moderate alone below default safe", "trust:\n  ramp_moderate_hours: 4\n",
			"trust.ramp_moderate_hours"},
		{"moderate equals safe", "trust:\n  ramp_safe_hours: 5\n  ramp_moderate_hours: 5\n", ""},
		{"baseline hours negative", "verify:\n  io_baseline_hours: -1\n",
			"verify.io_baseline_hours"},
		{"baseline hours 0 uses days", "verify:\n  io_baseline_hours: 0\n", ""},
		{"baseline hours 1", "verify:\n  io_baseline_hours: 1\n", ""},
		{"baseline hours fill retention", "verify:\n  io_baseline_hours: 336\n", ""},
		{"baseline hours beyond retention", "verify:\n  io_baseline_hours: 337\n",
			"io_sample_retention_days"},
		{"baseline hours over max",
			"verify:\n  io_baseline_hours: 8761\n  io_sample_retention_days: 400\n",
			"verify.io_baseline_hours"},
	}
	runElevationCases(t, cases)
}

func TestPromotionBoundaries(t *testing.T) {
	const pre = "sre:\n  autonomy:\n    promotion:\n      "
	k := "sre.autonomy.promotion."
	cases := []struct {
		name, yaml, wantKey string
	}{
		{"shadow 0", pre + "shadow_window_hours: 0\n", k + "shadow_window_hours"},
		{"shadow 1", pre + "shadow_window_hours: 1\n", ""},
		{"shadow max", pre + "shadow_window_hours: 8760\n", ""},
		{"shadow over max", pre + "shadow_window_hours: 8761\n", k + "shadow_window_hours"},
		{"reviewed 2", pre + "shadow_min_reviewed: 2\n", k + "shadow_min_reviewed"},
		{"reviewed 3", pre + "shadow_min_reviewed: 3\n", ""},
		{"reviewed max", pre + "shadow_min_reviewed: 1000\n", ""},
		{"reviewed over", pre + "shadow_min_reviewed: 1001\n", k + "shadow_min_reviewed"},
		{"recoveries 0", pre + "min_live_recoveries: 0\n", k + "min_live_recoveries"},
		{"recoveries 1", pre + "min_live_recoveries: 1\n", ""},
		{"recoveries max", pre + "min_live_recoveries: 10000\n", ""},
		{"recoveries over", pre + "min_live_recoveries: 10001\n", k + "min_live_recoveries"},
		{"unknown key", pre + "min_top1: 0.5\n", "min_top1"},
	}
	for _, key := range []string{"shadow_min_accepted_pct", "bench_min_top1_pct",
		"bench_min_precision_pct", "min_safe_pass_pct"} {
		cases = append(cases,
			struct{ name, yaml, wantKey string }{key + " 0", pre + key + ": 0\n", k + key},
			struct{ name, yaml, wantKey string }{key + " 49.9", pre + key + ": 49.9\n", k + key},
			struct{ name, yaml, wantKey string }{key + " 50", pre + key + ": 50\n", ""},
			struct{ name, yaml, wantKey string }{key + " 100", pre + key + ": 100\n", ""},
			struct{ name, yaml, wantKey string }{key + " 100.1", pre + key + ": 100.1\n",
				k + key},
			struct{ name, yaml, wantKey string }{key + " nan", pre + key + ": .nan\n", k + key},
		)
	}
	runElevationCases(t, cases)
}

func runElevationCases(t *testing.T, cases []struct{ name, yaml, wantKey string }) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadRCAYAML(t, c.yaml)
			if c.wantKey == "" && err != nil {
				t.Fatalf("%q rejected: %v", c.yaml, err)
			}
			if c.wantKey != "" && (err == nil || !strings.Contains(err.Error(), c.wantKey)) {
				t.Fatalf("%q: err = %v, want one naming %s", c.yaml, err, c.wantKey)
			}
		})
	}
}

// verify.io_baseline_hours, when set, takes precedence over
// io_baseline_days, including an io_baseline_days of 0.
func TestIOBaselineHoursPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		hours int
		days  int
		want  float64
	}{
		{"hours win over days", 2, 7, 2.0 / 24},
		{"hours enable a disabled days baseline", 2, 0, 2.0 / 24},
		{"days when hours unset", 0, 3, 3},
		{"both zero disables", 0, 0, 0},
		{"a day of hours", 24, 7, 1},
	}
	for _, c := range cases {
		v := VerifyConfig{IOBaselineHours: c.hours, IOBaselineDays: c.days}
		if got := v.EffectiveIOBaselineDays(); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: effective = %v days, want %v", c.name, got, c.want)
		}
	}
}

// A zero ramp (a Config built without defaults) is not "no ramp": the
// duration accessors report it, and the gate takes the spec ramp for it.
func TestZeroRampHoursReportZeroDuration(t *testing.T) {
	var tr TrustConfig
	if tr.SafeRamp() != 0 || tr.ModerateRamp() != 0 {
		t.Fatalf("zero-value ramp = %s/%s, want 0/0", tr.SafeRamp(), tr.ModerateRamp())
	}
	var p SREPromotionConfig
	if p.ShadowWindow() != 0 {
		t.Fatalf("zero-value shadow window = %s", p.ShadowWindow())
	}
}
