package config

import (
	"strings"
	"testing"
)

// forecast-03: forecaster.checkpoint_requested_per_hour is the requested
// (WAL-triggered) checkpoint rate at which the checkpoint forecast warns.

// No concurrent access tests: Load reads one file into a fresh Config.

func TestCheckpointRequestedPerHour_DefaultsWithoutConfig(t *testing.T) {
	cfg, _, err := loadWithWarnings(t, "mode: standalone\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Forecaster.CheckpointRequestedPerHour; got != 2 {
		t.Fatalf("checkpoint_requested_per_hour = %v, want the default 2", got)
	}
	if got := DefaultConfig().Forecaster.CheckpointRequestedPerHour; got != 2 {
		t.Fatalf("DefaultConfig checkpoint_requested_per_hour = %v, want 2", got)
	}
}

func TestCheckpointRequestedPerHour_LoadsAValue(t *testing.T) {
	cfg, _, err := loadWithWarnings(t,
		"forecaster:\n  checkpoint_requested_per_hour: 3.5\n  lookback_days: 14\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Forecaster.CheckpointRequestedPerHour != 3.5 || cfg.Forecaster.LookbackDays != 14 {
		t.Fatalf("forecaster = %+v", cfg.Forecaster)
	}
}

func TestCheckpointRequestedPerHour_RejectsOutOfRange(t *testing.T) {
	for _, v := range []string{"0", "-1", "3601", ".nan", ".inf"} {
		_, _, err := loadWithWarnings(t,
			"forecaster:\n  checkpoint_requested_per_hour: "+v+"\n")
		if err == nil {
			t.Fatalf("checkpoint_requested_per_hour %s loaded", v)
		}
		if !strings.Contains(err.Error(), "forecaster.checkpoint_requested_per_hour") {
			t.Fatalf("value %s: error %q does not name the key", v, err)
		}
	}
}

func TestCheckpointRequestedPerHour_Boundaries(t *testing.T) {
	for _, v := range []string{"0.01", "3600"} {
		if _, _, err := loadWithWarnings(t,
			"forecaster:\n  checkpoint_requested_per_hour: "+v+"\n"); err != nil {
			t.Fatalf("checkpoint_requested_per_hour %s rejected: %v", v, err)
		}
	}
}
