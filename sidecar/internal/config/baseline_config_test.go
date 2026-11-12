package config

import (
	"strings"
	"testing"
	"time"
)

// market-02: the baseline section configures the hour-of-week baselines
// behind workload_anomaly. Defaults hold without a config file; an
// unknown time zone or an inverted z pair fails the load.
// No concurrent access tests: the section is a value validated once per load.

func TestBaseline_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := BaselineConfig{Enabled: true, Weeks: 4, Timezone: "UTC", ZWarning: 4,
		ZCritical: 6, MaxQueryKeys: 500, MinDeltaMs: 5}
	if cfg.Baseline != want {
		t.Fatalf("baseline = %+v, want %+v", cfg.Baseline, want)
	}
	if DefaultConfig().Baseline != want || DefaultBaseline() != want {
		t.Fatal("DefaultConfig / DefaultBaseline disagree with Load(nil)")
	}
	if cfg.Baseline.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", cfg.Baseline.Location())
	}
}

func TestBaseline_PartialSectionKeepsOtherDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "baseline:\n  timezone: America/Chicago\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := cfg.Baseline
	if b.Timezone != "America/Chicago" || !b.Enabled || b.Weeks != 4 || b.ZWarning != 4 {
		t.Fatalf("baseline = %+v, want Chicago and the other defaults", b)
	}
	if b.Location().String() != "America/Chicago" {
		t.Fatalf("location = %v, want America/Chicago", b.Location())
	}
	off, err := loadRCAYAML(t, "baseline:\n  enabled: false\n")
	if err != nil || off.Baseline.Enabled {
		t.Fatalf("enabled: false = %+v (%v), want disabled", off.Baseline, err)
	}
}

// Invalid input: an unknown time zone fails config load with the key.
func TestBaseline_UnknownTimezoneFailsLoad(t *testing.T) {
	_, err := loadRCAYAML(t, "baseline:\n  timezone: Mars/Olympus_Mons\n")
	if err == nil || !strings.Contains(err.Error(), "baseline.timezone") {
		t.Fatalf("unknown timezone = %v, want an error naming baseline.timezone", err)
	}
}

// Boundary: weeks 3-52, z_warning above 0, z_critical at least z_warning,
// max_query_keys 0-5000, min_delta_ms 0 or more.
func TestBaseline_ValidateRanges(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*BaselineConfig)
		key  string // empty: valid
	}{
		{"defaults", func(*BaselineConfig) {}, ""},
		{"weeks 3", func(b *BaselineConfig) { b.Weeks = 3 }, ""},
		{"weeks 2", func(b *BaselineConfig) { b.Weeks = 2 }, "baseline.weeks"},
		{"weeks 52", func(b *BaselineConfig) { b.Weeks = 52 }, ""},
		{"weeks 53", func(b *BaselineConfig) { b.Weeks = 53 }, "baseline.weeks"},
		{"z_warning 0", func(b *BaselineConfig) { b.ZWarning = 0 }, "baseline.z_warning"},
		{"z_critical below warning", func(b *BaselineConfig) { b.ZCritical = 3.5 },
			"baseline.z_critical"},
		{"z_critical equal", func(b *BaselineConfig) { b.ZCritical = 4 }, ""},
		{"keys 0", func(b *BaselineConfig) { b.MaxQueryKeys = 0 }, ""},
		{"keys -1", func(b *BaselineConfig) { b.MaxQueryKeys = -1 },
			"baseline.max_query_keys"},
		{"keys 5001", func(b *BaselineConfig) { b.MaxQueryKeys = 5001 },
			"baseline.max_query_keys"},
		{"min delta -1", func(b *BaselineConfig) { b.MinDeltaMs = -1 },
			"baseline.min_delta_ms"},
		{"disabled skips nothing", func(b *BaselineConfig) {
			b.Enabled = false
			b.Timezone = "Nowhere/Land"
		}, "baseline.timezone"},
	}
	for _, c := range cases {
		b := DefaultBaseline()
		c.mod(&b)
		err := b.validate()
		switch {
		case c.key == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.key != "" && (err == nil || !strings.Contains(err.Error(), c.key)):
			t.Errorf("%s: error %v, want one naming %s", c.name, err, c.key)
		}
	}
}

// Nil/zero: a zero section (a literal Config in a test) reads as UTC.
func TestBaseline_ZeroValueLocationIsUTC(t *testing.T) {
	var b BaselineConfig
	if b.Location() != time.UTC {
		t.Fatalf("zero location = %v, want UTC", b.Location())
	}
}
