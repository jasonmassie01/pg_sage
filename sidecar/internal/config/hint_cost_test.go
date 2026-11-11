package config

import (
	"strings"
	"testing"
)

// tuning-05: tuner.hint_cost_tolerance_pct defaults to 10 with no config
// file, keeps an explicit 0 (no default masking) and is validated to 0-100.

func TestDefaultConfig_HintCostTolerancePct(t *testing.T) {
	if got := DefaultConfig().Tuner.HintCostTolerancePct; got != 10 ||
		DefaultTunerHintCostTolerancePct != 10 {
		t.Fatalf("hint_cost_tolerance_pct default = %g, want 10", got)
	}
}

func TestLoad_HintCostTolerancePct(t *testing.T) {
	for body, want := range map[string]float64{
		"  hint_cost_tolerance_pct: 0\n":    0,
		"  hint_cost_tolerance_pct: 100\n":  100,
		"  hint_cost_tolerance_pct: 12.5\n": 12.5,
		"  enabled: true\n":                 10,
	} {
		cfg, err := Load([]string{"--config", writeParamShapeConfig(t, body)})
		if err != nil || cfg.Tuner.HintCostTolerancePct != want {
			t.Errorf("%q: %v, %v, want %g", body, cfg, err, want)
		}
	}
}

// Boundary and invalid input: just outside 0-100 is refused, naming the key.
func TestLoad_HintCostTolerancePctBounds(t *testing.T) {
	for _, v := range []string{"-0.1", "100.1", "-5", "1000"} {
		_, err := Load([]string{"--config", writeParamShapeConfig(t,
			"  hint_cost_tolerance_pct: "+v+"\n")})
		if err == nil || !strings.Contains(err.Error(), "tuner.hint_cost_tolerance_pct") ||
			!strings.Contains(err.Error(), "0-100") {
			t.Errorf("%s: err = %v, want a 0-100 range error naming the key", v, err)
		}
	}
}
