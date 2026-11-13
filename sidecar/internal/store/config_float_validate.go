package store

import (
	"fmt"
	"strconv"
)

// floatRanges are the inclusive bounds of the float validation types.
var floatRanges = map[string][2]float64{
	"float01":       {0, 1},
	"float_share":   {0.01, 1}, // a share of a maximum: (0, 1], as the config file holds it
	"float_pos":     {0.001, 1e9},
	"float_nonneg":  {0, 1e9},
	"float_pct_pos": {0.001, 100},
}

func validateFloatRange(
	key, value string, min, max float64,
) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf(
			"validate: %s must be a number, got %q", key, value)
	}
	if !(f >= min && f <= max) { // NaN compares false with both bounds
		return fmt.Errorf(
			"validate: %s must be %g-%g, got %g", key, min, max, f)
	}
	return nil
}

// validateOpenPercent accepts 0 < value < 100, the range the config file
// allows for custodian.freeze.red_buffer_pct (vacuum-11b).
func validateOpenPercent(key, value string) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf(
			"validate: %s must be a number, got %q", key, value)
	}
	if !(f > 0 && f < 100) {
		return fmt.Errorf(
			"validate: %s must be greater than 0 and less than 100, got %g", key, f)
	}
	return nil
}
