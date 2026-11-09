package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// forecast-03: the runtime hands the checkpoint threshold to the
// forecaster; a dropped field would silently fall back to the default.
// No concurrent access tests: forecasterConfig copies one value struct.
func TestForecasterConfigCarriesCheckpointThreshold(t *testing.T) {
	c := config.DefaultConfig()
	if got := forecasterConfig(c).CheckpointRequestedPerHour; got != 2 {
		t.Fatalf("default threshold = %v, want 2", got)
	}
	c.Forecaster.CheckpointRequestedPerHour = 4.5
	if got := forecasterConfig(c).CheckpointRequestedPerHour; got != 4.5 {
		t.Fatalf("threshold = %v, want the configured 4.5", got)
	}
}
