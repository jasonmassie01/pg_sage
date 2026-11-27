package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fleet-04: ha.check_interval_seconds is the cadence of each database's
// one HA monitor. 60 s by default, 5-600 s.

func TestHACheckIntervalDefaultsWithoutAConfigFile(t *testing.T) {
	c := DefaultConfig()
	if c.HA.CheckIntervalSeconds != 60 || c.HA.CheckInterval() != time.Minute {
		t.Fatalf("default = %d (%v), want 60 (1m)", c.HA.CheckIntervalSeconds,
			c.HA.CheckInterval())
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the default config is invalid: %v", err)
	}
}

// A zero-value section (a config built without DefaultConfig) still
// checks at the default cadence rather than never.
func TestHACheckIntervalZeroValueIsTheDefault(t *testing.T) {
	var h HAConfig
	if h.CheckInterval() != time.Minute {
		t.Fatalf("zero-value interval = %v, want 1m", h.CheckInterval())
	}
}

func TestHACheckIntervalLoadsFromYAML(t *testing.T) {
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte("ha:\n  check_interval_seconds: 30\n"),
		0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HA.CheckIntervalSeconds != 30 || cfg.HA.CheckInterval() != 30*time.Second {
		t.Fatalf("loaded = %d (%v), want 30", cfg.HA.CheckIntervalSeconds,
			cfg.HA.CheckInterval())
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("loaded config invalid: %v", err)
	}
}

func TestHACheckIntervalBoundaries(t *testing.T) {
	for _, ok := range []int{5, 60, 600} {
		c := DefaultConfig()
		c.HA.CheckIntervalSeconds = ok
		if err := c.Validate(); err != nil {
			t.Errorf("check_interval_seconds=%d rejected: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 0, 4, 601} {
		c := DefaultConfig()
		c.HA.CheckIntervalSeconds = bad
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "ha.check_interval_seconds") ||
			!strings.Contains(err.Error(), "5-600") {
			t.Errorf("check_interval_seconds=%d: err = %v, want one naming the key "+
				"and its range", bad, err)
		}
	}
}
