package config

import (
	"fmt"
	"strings"
	"testing"
)

// tuning.*: the case-driven tuning agent (roadmap 2.2) and its
// per-database, per-cycle budgets.

func TestTuningDefaults(t *testing.T) {
	want := TuningConfig{Enabled: true, MaxCasesPerCycle: 3, MaxRequestsPerCycle: 12,
		MaxTokensPerCycle: 60000, MaxTurnsPerCase: 6, MaxProposalsPerCycle: 10,
		CalibrationMinOutcomes: 5, CalibrationWindowDays: 180}
	if got := DefaultConfig().Tuning; got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	chdirTemp(t)
	loaded, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Tuning != want {
		t.Fatalf("no config file: %+v, want %+v", loaded.Tuning, want)
	}
}

func TestTuningPartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "tuning:\n  max_cases_per_cycle: 5\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Tuning
	if got.MaxCasesPerCycle != 5 || !got.Enabled || got.MaxTokensPerCycle != 60000 ||
		got.CalibrationMinOutcomes != 5 {
		t.Fatalf("partial section masked defaults: %+v", got)
	}
}

func TestTuningCanBeSwitchedOff(t *testing.T) {
	cfg, err := loadRCAYAML(t, "tuning:\n  enabled: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tuning.Enabled || cfg.Tuning.MaxCasesPerCycle != 3 {
		t.Fatalf("tuning = %+v", cfg.Tuning)
	}
}

func TestTuningValidationRanges(t *testing.T) {
	for _, tc := range []struct {
		key      string
		low, top int
	}{
		{"max_cases_per_cycle", 1, 20},
		{"max_requests_per_cycle", 1, 200},
		{"max_tokens_per_cycle", 1000, 2000000},
		{"max_turns_per_case", 1, 20},
		{"max_proposals_per_cycle", 1, 100},
		{"calibration_min_outcomes", 1, 1000},
		{"calibration_window_days", 1, 3650},
	} {
		for _, v := range []int{tc.low, tc.top} {
			if _, err := loadRCAYAML(t, fmt.Sprintf("tuning:\n  %s: %d\n", tc.key, v)); err != nil {
				t.Errorf("%s=%d is in range: %v", tc.key, v, err)
			}
		}
		for _, v := range []int{tc.low - 1, tc.top + 1} {
			_, err := loadRCAYAML(t, fmt.Sprintf("tuning:\n  %s: %d\n", tc.key, v))
			if err == nil || !strings.Contains(err.Error(), "tuning."+tc.key) {
				t.Errorf("%s=%d: err = %v, want a range error naming the key", tc.key, v, err)
			}
		}
	}
}
