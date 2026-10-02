package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sage SRE M6 reactive detector thresholds (sre.detectors.*): today's
// conservative values are the defaults, an absent file, a partial section
// or explicit zeros cannot mask them, each knob reaches the loaded config
// and out-of-range values are refused with an error naming the key.

func TestSREDetectorsDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.SRE.Detectors
	if d.WindowSeconds != 300 || d.CheckpointRequested != 3 || d.TempFileMB != 1024 ||
		d.LWLockWaiters != 8 || d.LWLockPolls != 3 || d.CooldownMinutes != 30 {
		t.Fatalf("detector defaults = %+v", d)
	}
	if d.Window() != 5*time.Minute || d.TempBytes() != 1<<30 ||
		d.Cooldown() != 30*time.Minute {
		t.Fatalf("detector durations: window %s, temp %d, cooldown %s", d.Window(),
			d.TempBytes(), d.Cooldown())
	}
	if DefaultConfig().SRE.Detectors != d {
		t.Fatal("DefaultConfig and Load(nil) disagree on detector defaults")
	}
}

func TestSREDetectors_PartialSectionKeepsTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  detectors:\n    lwlock_waiters: 12\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.SRE.Detectors
	if d.LWLockWaiters != 12 || d.WindowSeconds != 300 || d.CheckpointRequested != 3 ||
		d.TempFileMB != 1024 || d.LWLockPolls != 3 || d.CooldownMinutes != 30 {
		t.Fatalf("detectors = %+v, want only lwlock_waiters changed", d)
	}
}

// Each knob reaches the loaded config with the value the operator wrote.
func TestSREDetectors_EachKnobIsHonoured(t *testing.T) {
	cases := []struct {
		key   string
		value int
		got   func(SREDetectorsConfig) int
	}{
		{"window_seconds", 600, func(d SREDetectorsConfig) int { return d.WindowSeconds }},
		{"checkpoint_requested", 7,
			func(d SREDetectorsConfig) int { return d.CheckpointRequested }},
		{"temp_file_mb", 4096, func(d SREDetectorsConfig) int { return d.TempFileMB }},
		{"lwlock_waiters", 20, func(d SREDetectorsConfig) int { return d.LWLockWaiters }},
		{"lwlock_polls", 5, func(d SREDetectorsConfig) int { return d.LWLockPolls }},
		{"cooldown_minutes", 90, func(d SREDetectorsConfig) int { return d.CooldownMinutes }},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			cfg, err := loadRCAYAML(t, "sre:\n  detectors:\n    "+c.key+": "+
				strconv.Itoa(c.value)+"\n")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.got(cfg.SRE.Detectors); got != c.value {
				t.Fatalf("%s = %d, want %d", c.key, got, c.value)
			}
		})
	}
	cfg, err := loadRCAYAML(t, "sre:\n  detectors:\n    window_seconds: 120\n"+
		"    temp_file_mb: 3\n    cooldown_minutes: 1\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.SRE.Detectors
	if d.Window() != 2*time.Minute || d.TempBytes() != 3<<20 || d.Cooldown() != time.Minute {
		t.Fatalf("durations: window %s, temp %d, cooldown %s", d.Window(), d.TempBytes(),
			d.Cooldown())
	}
}

func TestSREDetectors_Boundaries(t *testing.T) {
	cases := []struct {
		key    string
		value  int
		wantOK bool
	}{
		{"window_seconds", 0, false}, {"window_seconds", 59, false},
		{"window_seconds", 60, true}, {"window_seconds", 3600, true},
		{"window_seconds", 3601, false}, {"window_seconds", -300, false},
		{"checkpoint_requested", 0, false}, {"checkpoint_requested", 1, true},
		{"checkpoint_requested", 1000, true}, {"checkpoint_requested", 1001, false},
		{"temp_file_mb", 0, false}, {"temp_file_mb", 1, true},
		{"temp_file_mb", 1048576, true}, {"temp_file_mb", 1048577, false},
		{"temp_file_mb", -1, false},
		{"lwlock_waiters", 0, false}, {"lwlock_waiters", 1, true},
		{"lwlock_waiters", 10000, true}, {"lwlock_waiters", 10001, false},
		{"lwlock_polls", 0, false}, {"lwlock_polls", 1, true},
		{"lwlock_polls", 100, true}, {"lwlock_polls", 101, false},
		{"cooldown_minutes", 0, false}, {"cooldown_minutes", 1, true},
		{"cooldown_minutes", 1440, true}, {"cooldown_minutes", 1441, false},
		{"cooldown_minutes", -5, false},
	}
	for _, c := range cases {
		t.Run(c.key+"="+strconv.Itoa(c.value), func(t *testing.T) {
			_, err := loadRCAYAML(t, "sre:\n  detectors:\n    "+c.key+": "+
				strconv.Itoa(c.value)+"\n")
			if c.wantOK && err != nil {
				t.Fatalf("%s=%d rejected: %v", c.key, c.value, err)
			}
			if !c.wantOK && (err == nil ||
				!strings.Contains(err.Error(), "sre.detectors."+c.key)) {
				t.Fatalf("%s=%d: err = %v, want one naming sre.detectors.%s", c.key,
					c.value, err, c.key)
			}
		})
	}
}

// A slow trigger poll is not a configuration error: the window and the
// trigger interval are validated independently (an existing
// trigger_interval_seconds of 600 must keep loading after the upgrade).
func TestSREDetectors_SlowTriggerIntervalStillLoads(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  trigger_interval_seconds: 600\n")
	if err != nil {
		t.Fatalf("trigger_interval_seconds 600 with the default window rejected: %v", err)
	}
	if cfg.SRE.Detectors.WindowSeconds != 300 {
		t.Fatalf("window = %d, want the default 300", cfg.SRE.Detectors.WindowSeconds)
	}
}

func TestSREDetectors_UnknownKeyRejected(t *testing.T) {
	if _, err := loadRCAYAML(t, "sre:\n  detectors:\n    temp_bytes: 5\n"); err == nil {
		t.Fatal("an unknown sre.detectors key was accepted")
	}
}

func TestSREDetectors_KeysHaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"window_seconds", "checkpoint_requested", "temp_file_mb",
		"lwlock_waiters", "lwlock_polls", "cooldown_minutes"} {
		doc := strings.TrimSpace(docs["sre.detectors."+key])
		if doc == "" {
			t.Errorf("sre.detectors.%s has no doc tag", key)
			continue
		}
		if !strings.Contains(doc, "Default:") {
			t.Errorf("sre.detectors.%s doc does not state its default: %q", key, doc)
		}
	}
}

// Stored PGIncidentBench and game-day reports age out after
// sre.autonomy.report_retention_days (default 90). The floor is the
// spec's 30-day evidence window, so retention never removes a game day
// the promotion evidence still counts.
func TestSREAutonomyReportRetention_DefaultAndBoundaries(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.Autonomy.ReportRetentionDays != 90 {
		t.Fatalf("report_retention_days = %d, want 90",
			cfg.SRE.Autonomy.ReportRetentionDays)
	}
	if DefaultConfig().SRE.Autonomy.ReportRetentionDays != 90 {
		t.Fatal("DefaultConfig report_retention_days is not 90")
	}
	for _, c := range []struct {
		value  int
		wantOK bool
	}{{0, false}, {-1, false}, {29, false}, {30, true}, {3650, true}, {3651, false}} {
		got, err := loadRCAYAML(t, "sre:\n  autonomy:\n    report_retention_days: "+
			strconv.Itoa(c.value)+"\n")
		if c.wantOK && (err != nil || got.SRE.Autonomy.ReportRetentionDays != c.value) {
			t.Errorf("report_retention_days=%d: %v", c.value, err)
		}
		if !c.wantOK && (err == nil ||
			!strings.Contains(err.Error(), "sre.autonomy.report_retention_days")) {
			t.Errorf("report_retention_days=%d: err = %v, want one naming the key",
				c.value, err)
		}
	}
	docs := collectDocTags(t, DefaultConfig())
	if strings.TrimSpace(docs["sre.autonomy.report_retention_days"]) == "" {
		t.Error("sre.autonomy.report_retention_days has no doc tag")
	}
}
