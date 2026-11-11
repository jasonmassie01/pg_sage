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

// tuning-05: the sidecar's only production Tuner has the hinted EXPLAIN
// gate on, so every hint it or the tuning agent proposes is planned first.
func TestNewTuner_TurnsHintComparisonOn(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	cfg.Tuner.Enabled = true
	rt := featureTestRuntime(t, state.Pool, detectPGVersion(state.Pool), false)
	tu := rt.newTuner()
	if tu == nil || !tu.HintComparisonEnabled() {
		t.Fatalf("production tuner %v has no hinted EXPLAIN gate", tu)
	}
}
