package config

import (
	"strings"
	"testing"
)

// vacuum-08: analyzer.wraparound_freeze_share replaces the absolute
// analyzer.wraparound_freeze_xid_age, which is retired: a config that
// still sets it loads, with one warning.

const retiredXIDAgeWarning = "analyzer.wraparound_freeze_xid_age"

func TestWraparoundFreezeShareDefaultsWithoutConfigFile(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Analyzer.WraparoundFreezeShare != 0.75 ||
		DefaultWraparoundFreezeShare != 0.75 {
		t.Fatalf("share = %v (default %v), want 0.75", cfg.Analyzer.WraparoundFreezeShare,
			DefaultWraparoundFreezeShare)
	}
	loaded, out, err := loadWithWarnings(t, "analyzer:\n  interval_seconds: 600\n")
	if err != nil || loaded.Analyzer.WraparoundFreezeShare != 0.75 {
		t.Fatalf("file without the key: share %v (%v)", loaded.Analyzer.WraparoundFreezeShare, err)
	}
	if strings.Contains(out, retiredXIDAgeWarning) {
		t.Fatalf("absent retired key warned: %q", out)
	}
}

func TestRetiredWraparoundXIDAgeLoadsWithOneWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t,
		"analyzer:\n  wraparound_freeze_xid_age: 120000000\n  interval_seconds: 300\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, retiredXIDAgeWarning); got != 1 {
		t.Fatalf("warnings %q mention %s %d times, want once", out, retiredXIDAgeWarning, got)
	}
	if !strings.Contains(out, "WARNING:") || !strings.Contains(out, "wraparound_freeze_share") {
		t.Fatalf("warning %q does not name the replacement key", out)
	}
	if cfg.Analyzer.IntervalSeconds != 300 || cfg.Analyzer.WraparoundFreezeShare != 0.75 {
		t.Fatalf("sibling keys lost: interval %d share %v", cfg.Analyzer.IntervalSeconds,
			cfg.Analyzer.WraparoundFreezeShare)
	}
}

func TestWraparoundFreezeShareBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"0.5", true}, {"1", true}, {"0.01", true}, {"0", false}, {"-0.1", false},
		{"1.01", false}, {"abc", false}} {
		cfg, _, err := loadWithWarnings(t, "analyzer:\n  wraparound_freeze_share: "+
			tc.value+"\n")
		if tc.ok && (err != nil || cfg == nil) {
			t.Errorf("share %s rejected: %v", tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("share %s accepted, want a load error", tc.value)
		}
		if !tc.ok && err != nil && tc.value != "abc" &&
			!strings.Contains(err.Error(), "wraparound_freeze_share") {
			t.Errorf("share %s error %q does not name the key", tc.value, err)
		}
	}
}
