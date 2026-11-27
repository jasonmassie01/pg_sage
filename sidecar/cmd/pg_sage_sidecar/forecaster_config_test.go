package main

import (
	"testing"
	"time"

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

// forecast-04: the runtime hands the cache day gate to the forecaster.
func TestForecasterConfigCarriesCacheMinBlocks(t *testing.T) {
	c := config.DefaultConfig()
	if got := forecasterConfig(c).CacheMinBlocksPerDay; got != 100_000 {
		t.Fatalf("default gate = %d, want 100000", got)
	}
	c.Forecaster.CacheMinBlocksPerDay = 7
	if got := forecasterConfig(c).CacheMinBlocksPerDay; got != 7 {
		t.Fatalf("gate = %d, want the configured 7", got)
	}
}

// forecast-08: the runtime hands the collector interval to the forecaster,
// which measures each day's coverage with it; a dropped field would turn
// the coverage gate off.
func TestForecasterConfigCarriesCollectorInterval(t *testing.T) {
	c := config.DefaultConfig()
	if got := forecasterConfig(c).CollectorInterval; got != config.DefaultCollectorInterval {
		t.Fatalf("default interval = %v, want %v", got, config.DefaultCollectorInterval)
	}
	c.Collector.IntervalSeconds = 300
	if got := forecasterConfig(c).CollectorInterval; got != 5*time.Minute {
		t.Fatalf("interval = %v, want the configured 5m", got)
	}
	if got := forecasterConfig(c).MinRSquared; got != config.DefaultForecasterMinRSquared {
		t.Fatalf("min_r_squared = %v, want the default %v", got,
			config.DefaultForecasterMinRSquared)
	}
}
