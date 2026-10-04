package config

import (
	"strings"
	"testing"
	"time"
)

// Phase 1.3: an index drop is verified over a business cycle,
// verify.drop_window_hours (default 168 = 7 days). A shorter window is a
// fast-elevation setting: allowed, never silent (LoweredElevation, the
// startup WARN and the autonomy API/UI list it).

func TestDropWindowDefaultsToABusinessWeek(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Verify.DropWindowHours != DefaultVerifyDropWindowHours ||
		DefaultVerifyDropWindowHours != 168 {
		t.Fatalf("drop window = %dh, want 168h", cfg.Verify.DropWindowHours)
	}
	if cfg.Verify.DropWindow() != 7*24*time.Hour {
		t.Fatalf("DropWindow() = %s, want 168h", cfg.Verify.DropWindow())
	}
	chdirTemp(t)
	loaded, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Verify.DropWindowHours != 168 {
		t.Fatalf("loaded drop window = %d, want 168 with no config file",
			loaded.Verify.DropWindowHours)
	}
}

func TestDropWindowPartialVerifySectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "verify:\n  min_samples: 5\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Verify.DropWindowHours != 168 {
		t.Fatalf("partial verify section masked the drop window: %d",
			cfg.Verify.DropWindowHours)
	}
}

func TestDropWindowFastElevationIsReported(t *testing.T) {
	cfg, err := loadRCAYAML(t, "verify:\n  drop_window_hours: 2\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Verify.DropWindow() != 2*time.Hour {
		t.Fatalf("DropWindow() = %s, want 2h", cfg.Verify.DropWindow())
	}
	found := false
	for _, s := range cfg.LoweredElevation() {
		if s.Key == "verify.drop_window_hours" {
			found = s.Value == 2 && s.Default == 168 && s.Unit == "hours"
		}
	}
	if !found {
		t.Fatalf("a 2h drop window is not reported as lowered: %+v", cfg.LoweredElevation())
	}
}

func TestDropWindowRaisedIsNotLowered(t *testing.T) {
	cfg, err := loadRCAYAML(t, "verify:\n  drop_window_hours: 336\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.LoweredElevation() {
		if s.Key == "verify.drop_window_hours" {
			t.Fatalf("a longer window is stricter, not lowered: %+v", s)
		}
	}
}

func TestDropWindowValidation(t *testing.T) {
	for _, bad := range []string{"0", "-1", "8761"} {
		_, err := loadRCAYAML(t, "verify:\n  drop_window_hours: "+bad+"\n")
		if err == nil || !strings.Contains(err.Error(), "verify.drop_window_hours") {
			t.Errorf("drop_window_hours=%s: err = %v, want a range error", bad, err)
		}
	}
	for _, good := range []string{"1", "8760"} {
		if _, err := loadRCAYAML(t, "verify:\n  drop_window_hours: "+good+"\n"); err != nil {
			t.Errorf("drop_window_hours=%s rejected: %v", good, err)
		}
	}
}
