package config

import (
	"strings"
	"testing"
	"time"
)

// self_config: the operator's switch and soak period for derived
// settings. On by default (shadow-first is the safe default), a 24 h soak.

func TestSelfConfigDefaults(t *testing.T) {
	c := DefaultConfig()
	if !c.SelfConfig.Enabled {
		t.Fatal("self_config.enabled defaults to false; derived settings start in shadow")
	}
	if c.SelfConfig.SoakHours != DefaultSelfConfigSoakHours || DefaultSelfConfigSoakHours != 24 {
		t.Fatalf("soak_hours = %d (default const %d), want 24",
			c.SelfConfig.SoakHours, DefaultSelfConfigSoakHours)
	}
	if got := c.SelfConfig.Soak(); got != 24*time.Hour {
		t.Fatalf("Soak() = %v, want 24h", got)
	}
}

func TestSelfConfigSoakBoundaries(t *testing.T) {
	for _, tc := range []struct {
		hours int
		ok    bool
	}{{0, false}, {-1, false}, {1, true}, {24, true}, {720, true}, {721, false}} {
		c := DefaultConfig()
		c.SelfConfig.SoakHours = tc.hours
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("soak_hours=%d rejected: %v", tc.hours, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("soak_hours=%d accepted", tc.hours)
			} else if !strings.Contains(err.Error(), "self_config.soak_hours") {
				t.Errorf("soak_hours=%d error does not name the key: %v", tc.hours, err)
			}
		}
	}
}

func TestSelfConfigLoadsFromYAML(t *testing.T) {
	path := wave5WriteYAML(t, "self_config:\n  enabled: false\n  soak_hours: 6\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.SelfConfig.Enabled || c.SelfConfig.SoakHours != 6 {
		t.Fatalf("loaded %+v", c.SelfConfig)
	}
	if c.SelfConfig.Soak() != 6*time.Hour {
		t.Fatalf("Soak() = %v", c.SelfConfig.Soak())
	}
}

func TestSelfConfigRejectsUnknownKey(t *testing.T) {
	path := wave5WriteYAML(t, "self_config:\n  soak_minutes: 5\n")
	if err := loadYAML(path, DefaultConfig()); err == nil {
		t.Fatal("unknown self_config key accepted")
	}
}

func TestValidateIsTheLoadValidation(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	c.Collector.IntervalSeconds = 0
	if err := c.Validate(); err == nil ||
		!strings.Contains(err.Error(), "collector.interval_seconds") {
		t.Fatalf("Validate() = %v, want the collector interval error", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err == nil {
		t.Fatal("nil config validated")
	}
}

func TestSelfConfigKeysAreRestartBound(t *testing.T) {
	for _, key := range []string{"self_config.enabled", "self_config.soak_hours"} {
		f, ok := LookupFieldLifecycle(key)
		if !ok || f.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart", key, f, ok)
		}
	}
}
