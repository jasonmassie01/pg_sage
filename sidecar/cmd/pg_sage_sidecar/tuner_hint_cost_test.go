package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// tuning-05: the tuner receives tuner.hint_cost_tolerance_pct, default 10,
// and an explicit 0 stays 0.
func TestTunerConfig_PassesHintCostTolerancePct(t *testing.T) {
	c := config.DefaultConfig()
	if got := tunerConfig(c).HintCostTolerancePct; got != 10 {
		t.Fatalf("default hint_cost_tolerance_pct reaching the tuner = %g, want 10", got)
	}
	c.Tuner.HintCostTolerancePct = 0
	if got := tunerConfig(c).HintCostTolerancePct; got != 0 {
		t.Fatalf("hint_cost_tolerance_pct 0 reaching the tuner = %g, want 0", got)
	}
}
