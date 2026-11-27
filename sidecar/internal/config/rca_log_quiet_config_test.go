package config

import (
	"strings"
	"testing"
	"time"
)

// rca-07: rca.log_quiet_minutes and rca.recurrence_renotify_hours stop
// periodic log events from paging detect/resolve pairs all day.

// No concurrent access tests: these are pure accessors and load-time
// validation of an immutable config value.

// Default-value masking guard: with no config file the quiet period is the
// 30 min dedup window and recurrences within 6 h are not paged again.
func TestRCALogQuietDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.RCA.LogQuiet(); got != 30*time.Minute {
		t.Errorf("LogQuiet() = %s, want 30m", got)
	}
	if cfg.RCA.RecurrenceRenotifyHours != 6 || DefaultRCARecurrenceRenotifyHours != 6 {
		t.Errorf("recurrence_renotify_hours = %d (default %d), want 6",
			cfg.RCA.RecurrenceRenotifyHours, DefaultRCARecurrenceRenotifyHours)
	}
	if got := cfg.RCA.RecurrenceRenotify(); got != 6*time.Hour {
		t.Errorf("RecurrenceRenotify() = %s, want 6h", got)
	}
}

// Nil/zero: a config built without the keys gets the defaults, never
// "resolve at once" or "always page".
func TestRCALogQuiet_ZeroValueStruct(t *testing.T) {
	var r RCAConfig
	if got := r.LogQuiet(); got != 30*time.Minute {
		t.Errorf("zero LogQuiet() = %s, want 30m", got)
	}
	if got := r.RecurrenceRenotify(); got != 6*time.Hour {
		t.Errorf("zero RecurrenceRenotify() = %s, want 6h", got)
	}
	r.DedupWindowMinutes = 45
	if got := r.LogQuiet(); got != 45*time.Minute {
		t.Errorf("LogQuiet() follows the dedup window: got %s, want 45m", got)
	}
	r.LogQuietMinutes = 10
	if got := r.LogQuiet(); got != 10*time.Minute {
		t.Errorf("LogQuiet() override = %s, want 10m", got)
	}
}

func TestRCALogQuiet_YAMLValues(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  log_quiet_minutes: 90\n"+
		"  recurrence_renotify_hours: 12\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.LogQuiet() != 90*time.Minute || cfg.RCA.RecurrenceRenotify() != 12*time.Hour {
		t.Errorf("quiet/renotify = %s/%s, want 90m/12h",
			cfg.RCA.LogQuiet(), cfg.RCA.RecurrenceRenotify())
	}
}

// Invalid input and boundaries: out-of-range values are rejected at load
// with the key named; the range ends are accepted.
func TestRCALogQuiet_Bounds(t *testing.T) {
	tests := []struct {
		yaml, key string
		ok        bool
	}{
		{"log_quiet_minutes: 1", "", true},
		{"log_quiet_minutes: 1440", "", true},
		{"log_quiet_minutes: 1441", "rca.log_quiet_minutes", false},
		{"log_quiet_minutes: -1", "rca.log_quiet_minutes", false},
		{"recurrence_renotify_hours: 1", "", true},
		{"recurrence_renotify_hours: 168", "", true},
		{"recurrence_renotify_hours: 0", "rca.recurrence_renotify_hours", false},
		{"recurrence_renotify_hours: 169", "rca.recurrence_renotify_hours", false},
		// A quiet period longer than the stale window would never apply.
		{"stale_after_hours: 1\n  log_quiet_minutes: 61", "rca.log_quiet_minutes", false},
		{"stale_after_hours: 1\n  log_quiet_minutes: 60", "", true},
	}
	for _, tc := range tests {
		_, err := loadRCAYAML(t, "rca:\n  "+tc.yaml+"\n")
		if tc.ok && err != nil {
			t.Errorf("%q rejected: %v", tc.yaml, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), tc.key)) {
			t.Errorf("%q: err = %v, want one naming %s", tc.yaml, err, tc.key)
		}
	}
}
