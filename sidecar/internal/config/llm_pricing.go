package config

import (
	"fmt"
	"math"
	"sort"
)

// LLMPrice is one model's operator-supplied price in USD per million
// tokens (llm-04). Input and output are required; reasoning, when set,
// prices the reasoning share of the completion tokens instead of output.
// pg_sage ships no prices and never guesses one: a model without an entry
// is recorded with a NULL cost.
type LLMPrice struct {
	InputPerM     *float64 `yaml:"input_per_m" json:"input_per_m"`
	OutputPerM    *float64 `yaml:"output_per_m" json:"output_per_m"`
	ReasoningPerM *float64 `yaml:"reasoning_per_m,omitempty" json:"reasoning_per_m,omitempty"`
}

// validatePricing refuses an empty model name and a missing, negative or
// non-finite price, naming the key. Unknown price kinds are refused by the
// strict YAML decoder.
func (l LLMConfig) validatePricing() error {
	models := make([]string, 0, len(l.Pricing))
	for model := range l.Pricing {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if model == "" {
			return fmt.Errorf("llm.pricing has an entry with an empty model name")
		}
		p := l.Pricing[model]
		for _, kind := range []struct {
			name     string
			value    *float64
			required bool
		}{{"input_per_m", p.InputPerM, true}, {"output_per_m", p.OutputPerM, true},
			{"reasoning_per_m", p.ReasoningPerM, false}} {
			if err := validatePrice(model, kind.name, kind.value, kind.required); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePrice(model, kind string, value *float64, required bool) error {
	key := "llm.pricing." + model + "." + kind
	switch {
	case value == nil && required:
		return fmt.Errorf("%s is required: a priced model needs input_per_m and "+
			"output_per_m (USD per million tokens; 0 for a free model)", key)
	case value == nil:
		return nil
	case math.IsNaN(*value) || math.IsInf(*value, 0):
		return fmt.Errorf("%s must be a finite number of USD per million tokens", key)
	case *value < 0:
		return fmt.Errorf("%s %v must be >= 0 USD per million tokens", key, *value)
	}
	return nil
}
