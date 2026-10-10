package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// sre.runways.size_interval_seconds (v2.3.x): the databases' total size
// stats every file of every database, so it is measured on a slower
// cadence than the WAL samples. The disk runway forecasts over hours.

func TestSizeInterval_DefaultWithoutAConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.SRE.Runways
	if r.SizeIntervalSeconds != 600 || r.SizeInterval() != 10*time.Minute {
		t.Fatalf("size interval = %d (%s), want 600 s", r.SizeIntervalSeconds,
			r.SizeInterval())
	}
	if DefaultConfig().SRE.Runways.SizeIntervalSeconds != 600 {
		t.Fatal("DefaultConfig and Load(nil) disagree")
	}
	partial, err := loadRCAYAML(t, "sre:\n  runways:\n    interval_seconds: 120\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if partial.SRE.Runways.SizeIntervalSeconds != 600 {
		t.Fatalf("a partial runways section masked the size interval: %d",
			partial.SRE.Runways.SizeIntervalSeconds)
	}
}

func TestSizeInterval_Boundaries(t *testing.T) {
	cases := []struct {
		seconds int
		ok      bool
	}{
		{-1, false}, {0, false}, {14, false}, {15, true}, {600, true}, {86400, true},
		{86401, false},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.seconds), func(t *testing.T) {
			cfg, err := loadRCAYAML(t, "sre:\n  runways:\n    size_interval_seconds: "+
				strconv.Itoa(c.seconds)+"\n")
			if c.ok && (err != nil || cfg.SRE.Runways.SizeIntervalSeconds != c.seconds) {
				t.Fatalf("rejected or lost: %v", err)
			}
			if !c.ok && (err == nil ||
				!strings.Contains(err.Error(), "sre.runways.size_interval_seconds")) {
				t.Fatalf("err = %v, want sre.runways.size_interval_seconds named", err)
			}
		})
	}
}

// The effective period is clamped like the sequences': never shorter
// than the tick, never so long that min_samples miss the lookback. A
// literal zero (a RunwayConfig built in code) is every tick.
func TestSizeInterval_EffectivePeriodIsClamped(t *testing.T) {
	cases := []struct {
		mutate func(*RunwayConfig)
		want   time.Duration
	}{
		{func(*RunwayConfig) {}, 10 * time.Minute},
		{func(c *RunwayConfig) { c.IntervalSeconds = 3600 }, time.Hour},
		{func(c *RunwayConfig) { c.SizeIntervalSeconds = 15 }, time.Minute},
		{func(c *RunwayConfig) { c.LookbackHours = 1 }, 400 * time.Second},
		{func(c *RunwayConfig) { c.SizeIntervalSeconds = 86400 }, 2400 * time.Second},
		{func(c *RunwayConfig) { c.MinSamples = 1000 }, time.Minute},
		{func(c *RunwayConfig) { c.SizeIntervalSeconds = 0 }, time.Minute},
	}
	for i, c := range cases {
		cfg := defaultRunwayConfig()
		c.mutate(&cfg)
		if got := cfg.SizeInterval(); got != c.want {
			t.Errorf("case %d: effective size interval = %s, want %s", i, got, c.want)
		}
	}
	for _, body := range []string{"interval_seconds: 3600", "lookback_hours: 1",
		"min_samples: 1000"} {
		if _, err := loadRCAYAML(t, "sre:\n  runways:\n    "+body+"\n"); err != nil {
			t.Errorf("%s with the default size interval rejected: %v", body, err)
		}
	}
}

func TestSizeInterval_HasADocTag(t *testing.T) {
	doc := collectDocTags(t, DefaultConfig())["sre.runways.size_interval_seconds"]
	if !strings.Contains(doc, "600") || !strings.Contains(doc, "15-86400") {
		t.Fatalf("size_interval_seconds doc tag %q must give the default and range", doc)
	}
}
