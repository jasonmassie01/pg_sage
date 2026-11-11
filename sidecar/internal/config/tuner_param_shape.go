package config

import "fmt"

// Bounds of tuner.param_shape_min_variants (tuning-11): one variant is not
// a shape explosion, and past 1000 the trigger could never fire within a
// collector snapshot.
const (
	minTunerParamShapeMinVariants = 2
	maxTunerParamShapeMinVariants = 1000
)

func (t TunerConfig) validateParamShapeMinVariants() error {
	v := t.ParamShapeMinVariants
	if v < minTunerParamShapeMinVariants || v > maxTunerParamShapeMinVariants {
		return fmt.Errorf("tuner.param_shape_min_variants must be %d-%d, got %d",
			minTunerParamShapeMinVariants, maxTunerParamShapeMinVariants, v)
	}
	return nil
}
