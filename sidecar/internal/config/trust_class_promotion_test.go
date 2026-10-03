package config

import (
	"strings"
	"testing"
)

// Roadmap 1.2: the promotion bar of self-initiated classes (index
// create/drop, GUC, reloption, vacuum, analyze, hints, retention) lives
// under sre.autonomy.class_promotion. The spec values are the defaults;
// lowering one is fast elevation (startup WARN, listed in the API/UI) and
// no knob accepts 0 to mean "skip".

func TestClassPromotionDefaults(t *testing.T) {
	for name, cfg := range map[string]*Config{"default": DefaultConfig()} {
		p := cfg.SRE.Autonomy.ClassPromotion
		if p.MinSuccessesL2 != 3 || p.MinSuccessesL3 != 10 || p.MinSuccessRatePct != 80 {
			t.Fatalf("%s class promotion = %+v", name, p)
		}
	}
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.SRE.Autonomy.ClassPromotion; p.MinSuccessesL2 != 3 ||
		p.MinSuccessesL3 != 10 || p.MinSuccessRatePct != 80 {
		t.Fatalf("no config file: %+v", p)
	}
}

func TestClassPromotionPartialSectionKeepsTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  autonomy:\n    class_promotion:\n"+
		"      min_successes_l2: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.SRE.Autonomy.ClassPromotion
	if p.MinSuccessesL2 != 1 || p.MinSuccessesL3 != 10 || p.MinSuccessRatePct != 80 {
		t.Fatalf("partial section = %+v", p)
	}
}

func TestClassPromotionValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SREClassPromotionConfig)
		want   string
	}{
		{"l2 zero", func(p *SREClassPromotionConfig) { p.MinSuccessesL2 = 0 },
			"min_successes_l2"},
		{"l2 too high", func(p *SREClassPromotionConfig) { p.MinSuccessesL2 = 1001 },
			"min_successes_l2"},
		{"l3 zero", func(p *SREClassPromotionConfig) { p.MinSuccessesL3 = 0 },
			"min_successes_l3"},
		{"l3 below l2", func(p *SREClassPromotionConfig) {
			p.MinSuccessesL2, p.MinSuccessesL3 = 5, 4
		}, "at least"},
		{"rate below 50", func(p *SREClassPromotionConfig) { p.MinSuccessRatePct = 49.9 },
			"min_success_rate_pct"},
		{"rate above 100", func(p *SREClassPromotionConfig) { p.MinSuccessRatePct = 101 },
			"min_success_rate_pct"},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		c.mutate(&cfg.SRE.Autonomy.ClassPromotion)
		err := cfg.SRE.Autonomy.validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
	}
	ok := DefaultConfig()
	ok.SRE.Autonomy.ClassPromotion = SREClassPromotionConfig{MinSuccessesL2: 1,
		MinSuccessesL3: 1, MinSuccessRatePct: 50}
	if err := ok.SRE.Autonomy.validate(); err != nil {
		t.Fatalf("the lowest valid bar was refused: %v", err)
	}
}

func TestClassPromotionLoweredIsFastElevation(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		want   LoweredSetting
	}{
		{func(c *Config) { c.SRE.Autonomy.ClassPromotion.MinSuccessesL2 = 1 },
			LoweredSetting{"sre.autonomy.class_promotion.min_successes_l2", 1, 3, "actions"}},
		{func(c *Config) { c.SRE.Autonomy.ClassPromotion.MinSuccessesL3 = 4 },
			LoweredSetting{"sre.autonomy.class_promotion.min_successes_l3", 4, 10, "actions"}},
		{func(c *Config) { c.SRE.Autonomy.ClassPromotion.MinSuccessRatePct = 70 },
			LoweredSetting{"sre.autonomy.class_promotion.min_success_rate_pct", 70, 80,
				"percent"}},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		c.mutate(cfg)
		got := cfg.LoweredElevation()
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: lowered = %+v, want exactly %+v", c.want.Key, got, c.want)
		}
	}
	raised := DefaultConfig()
	raised.SRE.Autonomy.ClassPromotion = SREClassPromotionConfig{MinSuccessesL2: 5,
		MinSuccessesL3: 20, MinSuccessRatePct: 90}
	if got := raised.LoweredElevation(); len(got) != 0 {
		t.Fatalf("a raised bar is reported as lowered: %+v", got)
	}
}

func TestClassPromotionKeysAreRestartBoundAndDocumented(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"sre.autonomy.class_promotion.min_successes_l2",
		"sre.autonomy.class_promotion.min_successes_l3",
		"sre.autonomy.class_promotion.min_success_rate_pct"} {
		field, ok := LookupFieldLifecycle(key)
		if !ok || field.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart", key, field, ok)
		}
		if strings.TrimSpace(docs[key]) == "" {
			t.Errorf("%s has no doc tag", key)
		}
	}
}

// The ramp settings keep loading and validating as before; their doc now
// says what they mean in the unified trust system.
func TestTrustRampDocsExplainTheFloor(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"trust.ramp_safe_hours", "trust.ramp_moderate_hours",
		"trust.ramp_start"} {
		if !strings.Contains(docs[key], "promotion") {
			t.Errorf("%s doc does not explain the promotion floor: %q", key, docs[key])
		}
	}
	if !strings.Contains(docs["trust.level"], "ceiling") {
		t.Errorf("trust.level doc does not say it is the ceiling: %q", docs["trust.level"])
	}
}
