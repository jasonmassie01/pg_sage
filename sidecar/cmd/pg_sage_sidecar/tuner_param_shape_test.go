package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// tuning-11: the tuner receives tuner.param_shape_min_variants, default 20.
func TestTunerConfig_PassesParamShapeMinVariants(t *testing.T) {
	c := config.DefaultConfig()
	if got := tunerConfig(c).ParamShapeMinVariants; got != 20 {
		t.Fatalf("default param_shape_min_variants reaching the tuner = %d, want 20", got)
	}
	c.Tuner.ParamShapeMinVariants = 7
	if got := tunerConfig(c).ParamShapeMinVariants; got != 7 {
		t.Fatalf("param_shape_min_variants reaching the tuner = %d, want 7", got)
	}
}
