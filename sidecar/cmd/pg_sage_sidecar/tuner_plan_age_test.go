package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// tuning-08: the tuner receives tuner.plan_max_age_minutes, default 1440.
func TestTunerConfig_PassesPlanMaxAge(t *testing.T) {
	c := config.DefaultConfig()
	if got := tunerConfig(c).PlanMaxAgeMinutes; got != 1440 {
		t.Fatalf("default plan_max_age_minutes reaching the tuner = %d, want 1440", got)
	}
	c.Tuner.PlanMaxAgeMinutes = 90
	if got := tunerConfig(c).PlanMaxAgeMinutes; got != 90 {
		t.Fatalf("plan_max_age_minutes reaching the tuner = %d, want 90", got)
	}
}
