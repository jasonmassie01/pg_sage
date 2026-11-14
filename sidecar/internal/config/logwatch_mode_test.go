package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rca-09b: logwatch.enabled is auto (unset), on (true) or off (false).
// No concurrent access tests: Mode, IsActive and ParseLogWatchEnabled are
// pure functions of their value receiver or argument.

func loadLogWatchYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	err := loadYAML(path, cfg)
	return cfg, err
}

// Defaults (default-value masking): with no config file the switch is
// unset and the mode is auto, not off.
func TestLogWatchModeDefaultIsAuto(t *testing.T) {
	lw := DefaultConfig().LogWatch
	if lw.Enabled != nil {
		t.Fatalf("default logwatch.enabled = %v, want unset (auto)", *lw.Enabled)
	}
	if got := lw.Mode(); got != LogWatchAuto {
		t.Fatalf("default mode = %q, want %q", got, LogWatchAuto)
	}
	if lw.SlowQueryEnabled {
		t.Fatal("logwatch.slow_query_enabled must stay opt-in (it reads statement text)")
	}
	if lw.PollIntervalMs != DefaultLogWatchPollIntervalMs ||
		lw.MaxLinesPerCycle != DefaultLogWatchMaxLinesPerCycle {
		t.Fatalf("tunable defaults lost: %+v", lw)
	}
}

// State transitions and boundaries: every mode against both detection
// outcomes.
func TestLogWatchModeAndIsActive(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name               string
		enabled            *bool
		mode, setting      string
		detected, notFound bool
	}{
		{"unset is auto", nil, LogWatchAuto, "auto", true, false},
		{"true is on", &yes, LogWatchOn, "true", true, true},
		{"false is off", &no, LogWatchOff, "false", false, false},
	}
	for _, c := range cases {
		lw := LogWatchConfig{Enabled: c.enabled}
		if got := lw.Mode(); got != c.mode {
			t.Errorf("%s: Mode() = %q, want %q", c.name, got, c.mode)
		}
		if got := lw.Setting(); got != c.setting {
			t.Errorf("%s: Setting() = %q, want %q", c.name, got, c.setting)
		}
		if got := lw.IsActive(true); got != c.detected {
			t.Errorf("%s: IsActive(detected) = %v, want %v", c.name, got, c.detected)
		}
		if got := lw.IsActive(false); got != c.notFound {
			t.Errorf("%s: IsActive(not detected) = %v, want %v", c.name, got, c.notFound)
		}
	}
}

// Happy path: YAML accepts true, false and auto; other keys of the
// section still decode and unset tunables keep their defaults.
func TestLogWatchYAMLEnabledValues(t *testing.T) {
	cases := []struct{ value, mode string }{
		{"true", LogWatchOn}, {"false", LogWatchOff}, {"auto", LogWatchAuto},
		{"AUTO", LogWatchAuto}, {`"auto"`, LogWatchAuto}, {"~", LogWatchAuto},
		{"", LogWatchAuto},
	}
	for _, c := range cases {
		body := "logwatch:\n  enabled: " + c.value + "\n  poll_interval_ms: 250\n"
		cfg, err := loadLogWatchYAML(t, body)
		if err != nil {
			t.Fatalf("enabled: %s: %v", c.value, err)
		}
		if got := cfg.LogWatch.Mode(); got != c.mode {
			t.Errorf("enabled: %s gave mode %q, want %q", c.value, got, c.mode)
		}
		if cfg.LogWatch.PollIntervalMs != 250 {
			t.Errorf("enabled: %s lost poll_interval_ms: %d", c.value,
				cfg.LogWatch.PollIntervalMs)
		}
		if cfg.LogWatch.MaxLinesPerCycle != DefaultLogWatchMaxLinesPerCycle {
			t.Errorf("enabled: %s reset max_lines_per_cycle to %d", c.value,
				cfg.LogWatch.MaxLinesPerCycle)
		}
	}
}

// Nil/empty: an empty logwatch section, and a file without one, are auto.
func TestLogWatchYAMLEmptySectionIsAuto(t *testing.T) {
	for _, body := range []string{"logwatch:\n", "logwatch: {}\n", "mode: standalone\n"} {
		cfg, err := loadLogWatchYAML(t, body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if cfg.LogWatch.Mode() != LogWatchAuto || cfg.LogWatch.Enabled != nil {
			t.Fatalf("%q: mode %q, want auto", body, cfg.LogWatch.Mode())
		}
		if cfg.LogWatch.TempFileMinBytes != DefaultLogWatchTempFileMinBytes {
			t.Fatalf("%q: temp_file_min_bytes = %d, want the default", body,
				cfg.LogWatch.TempFileMinBytes)
		}
	}
}

// Invalid input: an unknown value is refused with the key named, and the
// section still rejects unknown fields (strict decoding is kept).
func TestLogWatchYAMLRejectsInvalid(t *testing.T) {
	for _, body := range []string{
		"logwatch:\n  enabled: maybe\n",
		"logwatch:\n  enabled: [true]\n",
		"logwatch:\n  enabled: 2\n",
	} {
		_, err := loadLogWatchYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), "logwatch.enabled") {
			t.Fatalf("%q: err = %v, want a refusal naming logwatch.enabled", body, err)
		}
	}
	_, err := loadLogWatchYAML(t, "logwatch:\n  enabled: auto\n  bogus_key: 1\n")
	if err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Fatalf("unknown logwatch field: err = %v, want it refused by name", err)
	}
	_, err = loadLogWatchYAML(t, "logwatch:\n  poll_interval_ms: soon\n")
	if err == nil {
		t.Fatal("a non-integer poll_interval_ms was accepted")
	}
	_, err = loadLogWatchYAML(t, "logwatch: [1, 2]\n")
	if err == nil {
		t.Fatal("a logwatch sequence was accepted")
	}
}

// Invalid input and happy path of the API grammar.
func TestParseLogWatchEnabled(t *testing.T) {
	for _, v := range []string{"auto", "AUTO", " auto "} {
		got, err := ParseLogWatchEnabled(v)
		if err != nil || got != nil {
			t.Fatalf("%q = (%v, %v), want unset", v, got, err)
		}
	}
	for v, want := range map[string]bool{"true": true, "false": false, "TRUE": true, "0": false} {
		got, err := ParseLogWatchEnabled(v)
		if err != nil || got == nil || *got != want {
			t.Fatalf("%q = (%v, %v), want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"yes-please", "", "maybe", "on"} {
		_, err := ParseLogWatchEnabled(v)
		if err == nil || !strings.Contains(err.Error(), "logwatch.enabled") {
			t.Fatalf("%q: err = %v, want a refusal naming logwatch.enabled", v, err)
		}
	}
}

// State isolation: a clone does not share the switch with its original.
func TestCloneDetachesLogWatchEnabled(t *testing.T) {
	on := true
	original := DefaultConfig()
	original.LogWatch.Enabled = &on
	cloned := Clone(original)
	*original.LogWatch.Enabled = false
	if cloned.LogWatch.Mode() != LogWatchOn {
		t.Fatalf("clone mode = %q after the original changed, want on",
			cloned.LogWatch.Mode())
	}
}
