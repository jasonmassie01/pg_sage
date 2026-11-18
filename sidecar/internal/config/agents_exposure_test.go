package config

import (
	"strings"
	"testing"
	"time"
)

// agents.exposure.sweep_seconds (fleetpol-02a): how often the exposure
// sweep reads the catalog fingerprint. Default 60, range 15-600.

// No concurrent access tests: these are pure functions of a config value.

func TestAgentsExposureSweepDefault(t *testing.T) {
	a := DefaultConfig().Agents
	if a.Exposure.SweepSeconds != 60 {
		t.Fatalf("sweep_seconds = %d, want 60", a.Exposure.SweepSeconds)
	}
	if got := a.Exposure.SweepInterval(); got != time.Minute {
		t.Fatalf("SweepInterval = %s, want 1m", got)
	}
}

// Default value masking: a file without agents.exposure keeps 60, and a
// file without any agents key does too.
func TestAgentsExposureSweepDefaultSurvivesAFileWithoutIt(t *testing.T) {
	for _, body := range []string{"mode: standalone\n",
		"agents:\n  posture:\n    daily_at: \"04:00\"\n"} {
		path := wave5WriteYAML(t, body)
		c := DefaultConfig()
		if err := loadYAML(path, c); err != nil {
			t.Fatal(err)
		}
		if c.Agents.Exposure.SweepSeconds != 60 {
			t.Fatalf("%q: sweep_seconds = %d, want 60", body, c.Agents.Exposure.SweepSeconds)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%q: Validate: %v", body, err)
		}
	}
}

func TestAgentsExposureSweepLoadsFromYAML(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  exposure:\n    sweep_seconds: 30\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.Exposure.SweepSeconds != 30 ||
		c.Agents.Exposure.SweepInterval() != 30*time.Second {
		t.Fatalf("exposure = %+v", c.Agents.Exposure)
	}
}

// Boundary: 15 and 600 are accepted; 14, 601, 0 and negatives are refused
// with an error naming the key and the range.
func TestAgentsExposureSweepRange(t *testing.T) {
	for _, ok := range []int{15, 16, 60, 599, 600} {
		c := DefaultConfig()
		c.Agents.Exposure.SweepSeconds = ok
		if err := c.Validate(); err != nil {
			t.Errorf("sweep_seconds %d refused: %v", ok, err)
		}
	}
	for _, bad := range []int{14, 601, 0, -60} {
		c := DefaultConfig()
		c.Agents.Exposure.SweepSeconds = bad
		err := c.Validate()
		if err == nil {
			t.Errorf("sweep_seconds %d accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "agents.exposure.sweep_seconds") ||
			!strings.Contains(err.Error(), "15-600") {
			t.Errorf("sweep_seconds %d: err = %v, want the key and 15-600", bad, err)
		}
	}
}

// An exposed role name with a quote is refused: posture names the role
// in manual scripts and messages, and no real anonymous role needs one.
func TestValidExposedRoleRefusesQuotes(t *testing.T) {
	for _, bad := range []string{`web"anon`, `web'anon`, `"anon"`, "anon\x00"} {
		if err := ValidExposedRole(bad); err == nil {
			t.Errorf("ValidExposedRole(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"anon", "web_anon", "Web-Anon", "role with space"} {
		if err := ValidExposedRole(ok); err != nil {
			t.Errorf("ValidExposedRole(%q) = %v", ok, err)
		}
	}
	c := DefaultConfig()
	c.Agents.ExposedRoles = []string{`we"b`}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "agents.exposed_roles") {
		t.Fatalf("Validate = %v, want an agents.exposed_roles error", err)
	}
}

func TestAgentsExposureKeyClass(t *testing.T) {
	class, ok := KeyClassOf("agents.exposure.sweep_seconds")
	if !ok || class != KeyOperatorPreference {
		t.Fatalf("class = %q (known=%v), want operator_preference", class, ok)
	}
}
