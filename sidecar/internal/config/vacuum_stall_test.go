package config

import (
	"strings"
	"testing"
	"time"
)

// vacuum-05: safety.vacuum_stall_minutes bounds how long the VACUUM
// watchdog lets a heap phase make no progress; it defaults to 15 minutes,
// is bounded to 5-240 and is never disabled.

// No concurrent access tests: SafetyConfig methods read one struct value.

func TestVacuumStallDefaultWithoutAnyConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Safety.VacuumStallMinutes != 15 {
		t.Fatalf("default vacuum_stall_minutes = %d, want 15", cfg.Safety.VacuumStallMinutes)
	}
	if got := cfg.Safety.VacuumStall(); got != 15*time.Minute {
		t.Fatalf("default VacuumStall = %s, want 15m", got)
	}
}

func TestVacuumStallZeroValueUsesDefault(t *testing.T) {
	var s SafetyConfig
	if got := s.VacuumStall(); got != 15*time.Minute {
		t.Fatalf("zero VacuumStall = %s, want 15m (never disabled)", got)
	}
	s.VacuumStallMinutes = -3
	if got := s.VacuumStall(); got != 15*time.Minute {
		t.Fatalf("negative VacuumStall = %s, want 15m", got)
	}
	s.VacuumStallMinutes = 30
	if got := s.VacuumStall(); got != 30*time.Minute {
		t.Fatalf("configured VacuumStall = %s, want 30m", got)
	}
}

func TestVacuumStallBoundaries(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		ok      bool
	}{{-1, false}, {0, true}, {1, false}, {4, false}, {5, true}, {240, true},
		{241, false}} {
		cfg := DefaultConfig()
		cfg.Safety.VacuumStallMinutes = tc.minutes
		err := cfg.validate()
		if tc.ok && err != nil {
			t.Errorf("vacuum_stall_minutes=%d: unexpected error %v", tc.minutes, err)
		}
		if !tc.ok && (err == nil ||
			!strings.Contains(err.Error(), "safety.vacuum_stall_minutes") ||
			!strings.Contains(err.Error(), "5-240")) {
			t.Errorf("vacuum_stall_minutes=%d: error %v, want one naming the key and 5-240",
				tc.minutes, err)
		}
	}
}

func TestVacuumStallLoadsFromYAML(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+`
safety:
  vacuum_stall_minutes: 45
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Safety.VacuumStallMinutes != 45 {
		t.Fatalf("loaded vacuum_stall_minutes = %d, want 45", cfg.Safety.VacuumStallMinutes)
	}
	if _, err := loadYAMLResult(t, standaloneIOYAML+`
safety:
  vacuum_stall_minutes: 300
`); err == nil || !strings.Contains(err.Error(), "safety.vacuum_stall_minutes") {
		t.Fatalf("load of 300 min = %v, want a range error", err)
	}
}
