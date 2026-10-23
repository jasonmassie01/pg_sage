package config

import (
	"strings"
	"testing"
	"time"
)

// agents.capabilities.max_duration_minutes and
// agents.reconcile_interval_seconds (spec §6.6, §9): every grant is
// time-boxed, and the leader's reconciler revokes it on schedule.
// No concurrency tests: these are pure value checks.

func TestAgentsGrantDefaults(t *testing.T) {
	a := DefaultConfig().Agents
	if a.Capabilities.MaxDurationMinutes != 240 || a.ReconcileIntervalSeconds != 60 {
		t.Fatalf("defaults: max %d interval %d", a.Capabilities.MaxDurationMinutes,
			a.ReconcileIntervalSeconds)
	}
	if a.Capabilities.MaxDuration() != 4*time.Hour || a.ReconcileInterval() != time.Minute {
		t.Fatalf("durations: %v %v", a.Capabilities.MaxDuration(), a.ReconcileInterval())
	}
	path := wave5WriteYAML(t, "agents:\n  exposed_roles: [web]\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.Capabilities.MaxDurationMinutes != 240 ||
		c.Agents.ReconcileIntervalSeconds != 60 {
		t.Fatalf("a YAML agents block without the keys keeps the defaults: %+v", c.Agents)
	}
}

func TestAgentsGrantLoadsAndBounds(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  capabilities:\n    max_duration_minutes: 30\n"+
		"  reconcile_interval_seconds: 15\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.Capabilities.MaxDurationMinutes != 30 || c.Agents.ReconcileIntervalSeconds != 15 {
		t.Fatalf("loaded %+v", c.Agents)
	}
	cases := []struct {
		max, interval int
		ok            bool
		key           string
	}{
		{1, 60, true, ""}, {10080, 60, true, ""}, {0, 60, false, "max_duration_minutes"},
		{-1, 60, false, "max_duration_minutes"}, {10081, 60, false, "max_duration_minutes"},
		{240, 10, true, ""}, {240, 3600, true, ""},
		{240, 9, false, "reconcile_interval_seconds"},
		{240, 3601, false, "reconcile_interval_seconds"},
		{240, 0, false, "reconcile_interval_seconds"},
	}
	for _, tc := range cases {
		a := DefaultConfig().Agents
		a.Capabilities.MaxDurationMinutes, a.ReconcileIntervalSeconds = tc.max, tc.interval
		err := a.validate()
		if tc.ok != (err == nil) {
			t.Errorf("max %d interval %d: err %v", tc.max, tc.interval, err)
		}
		if err != nil && !strings.Contains(err.Error(), tc.key) {
			t.Errorf("error names the key %s: %v", tc.key, err)
		}
	}
}
