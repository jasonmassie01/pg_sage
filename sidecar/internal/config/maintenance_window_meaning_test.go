package config

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The unified window grammar (D2, v1.7) changed the meaning of some
// trust.maintenance_window values. Operators must be told, not surprised.
func TestMaintenanceWindowMeaningChange(t *testing.T) {
	for value, want := range map[string]string{
		"weeknights":              "weeknights",
		"  WEEKNIGHTS ":           "weeknights",
		"Mon,Wed,Fri 22:00-04:00": "crosses midnight",
		"weekdays 23:00-02:00":    "crosses midnight",
		"30 * * * *":              "every hour",
		"0 2 * * 1-5":             "ranges, lists or steps",
		"*/15 3 * * *":            "ranges, lists or steps",
		"0 2,4 * * *":             "ranges, lists or steps",
	} {
		got := MaintenanceWindowMeaningChange(value)
		if !strings.Contains(got, want) {
			t.Errorf("MaintenanceWindowMeaningChange(%q) = %q, want it to mention %q",
				value, got, want)
		}
	}
}

func TestMaintenanceWindowMeaningUnchanged(t *testing.T) {
	for _, value := range []string{
		"", "   ", "never", "off", "always", "weekends",
		"weekdays 01:00-05:00", "Sat 02:00-06:00", "0 2 * * *",
		"daily 01:00-03:00", "not a window",
	} {
		if got := MaintenanceWindowMeaningChange(value); got != "" {
			t.Errorf("MaintenanceWindowMeaningChange(%q) = %q, want no notice", value, got)
		}
	}
}

// The notices describe the new grammar; pin those descriptions to what
// policy.ParseWindow actually does so the text cannot drift from behaviour.
func TestMaintenanceWindowMeaningMatchesParser(t *testing.T) {
	at := func(day, hour, minute int) time.Time {
		// 2026-09-28 is a Monday; day 0 = Monday.
		return time.Date(2026, 9, 28+day, hour, minute, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		window string
		at     time.Time
		open   bool
	}{
		{"weeknights", at(4, 23, 0), true},              // Friday night
		{"weeknights", at(5, 3, 0), true},               // into Saturday morning
		{"weeknights", at(6, 23, 0), false},             // Sunday night
		{"weeknights", at(0, 3, 0), false},              // Monday early hours
		{"Mon,Wed,Fri 22:00-04:00", at(5, 2, 0), true},  // Fri night -> Sat
		{"Mon,Wed,Fri 22:00-04:00", at(2, 2, 0), false}, // Wed early hours
		{"30 * * * *", at(0, 5, 45), true},
		{"30 * * * *", at(0, 6, 10), true},
	} {
		window, err := policy.ParseWindow(tc.window)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.window, err)
		}
		if got := window.Contains(tc.at); got != tc.open {
			t.Errorf("%q at %s = %v, want %v", tc.window, tc.at.Format("Mon 15:04"), got, tc.open)
		}
	}
}
