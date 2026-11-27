package config

import (
	"strings"
	"testing"
)

// analyzer.inactive_slot_min_minutes: how long a replication slot must stay
// inactive before the analyzer reports it. Default 5, 0 uses the default,
// out of range is refused at load.

func TestInactiveSlotMinMinutes_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.InactiveSlotMinMinutes != 5 || DefaultInactiveSlotMinMinutes != 5 {
		t.Fatalf("default = %d (const %d), want 5", cfg.Analyzer.InactiveSlotMinMinutes,
			DefaultInactiveSlotMinMinutes)
	}
	if DefaultConfig().Analyzer.InactiveSlotMinMinutes != 5 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestInactiveSlotMinMinutes_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "analyzer:\n  slow_query_threshold_ms: 500\n")
	if err != nil || cfg.Analyzer.InactiveSlotMinMinutes != 5 {
		t.Fatalf("got %d (%v), want the default 5 kept", cfg.Analyzer.InactiveSlotMinMinutes, err)
	}
}

func TestInactiveSlotMinMinutes_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{{"0", 0}, {"1", 1}, {"60", 60}, {"10080", 10080}} {
		cfg, err := loadRCAYAML(t, "analyzer:\n  inactive_slot_min_minutes: "+tc.yaml+"\n")
		if err != nil || cfg.Analyzer.InactiveSlotMinMinutes != tc.want {
			t.Errorf("%s: got %d (%v), want %d", tc.yaml,
				cfg.Analyzer.InactiveSlotMinMinutes, err, tc.want)
		}
	}
}

func TestInactiveSlotMinMinutes_OutOfRangeRefused(t *testing.T) {
	for _, bad := range []string{"-1", "10081"} {
		_, err := loadRCAYAML(t, "analyzer:\n  inactive_slot_min_minutes: "+bad+"\n")
		if err == nil || !strings.Contains(err.Error(), "analyzer.inactive_slot_min_minutes") {
			t.Errorf("%s: err = %v, want a refusal naming the key", bad, err)
		}
	}
}
