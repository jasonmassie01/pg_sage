package config

import "fmt"

// Bounds of tuner.hint_cost_tolerance_pct (tuning-05): 0 refuses any cost
// rise; past 100 a hint could double the estimated cost and still pass.
const (
	minTunerHintCostTolerancePct = 0
	maxTunerHintCostTolerancePct = 100
)

func (t TunerConfig) validateHintCostTolerancePct() error {
	v := t.HintCostTolerancePct
	if v < minTunerHintCostTolerancePct || v > maxTunerHintCostTolerancePct || v != v {
		return fmt.Errorf("tuner.hint_cost_tolerance_pct must be %d-%d, got %g",
			minTunerHintCostTolerancePct, maxTunerHintCostTolerancePct, v)
	}
	return nil
}
