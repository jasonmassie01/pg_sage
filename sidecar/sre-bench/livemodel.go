package srebench

import (
	"fmt"
	"slices"
	"strings"
)

// The nightly live-model arm's default model and the prices its spend cap
// is computed with (USD per million input and output tokens). pg_sage
// has no default LLM model setting (llm.model is empty unless an
// operator sets it), so the default lives here, next to its prices,
// rather than in the workflow; the repository variable
// PG_SAGE_BENCH_OPENAI_MODEL overrides it, and a model other than the
// default must come with its own prices (PG_SAGE_BENCH_LLM_USD_PER_MTOK_IN
// and _OUT repository variables).
const (
	DefaultLiveModel    = "gpt-4o-mini"
	DefaultLivePriceIn  = "0.15"
	DefaultLivePriceOut = "0.60"
)

// LiveArmEnv is getenv with the live arm's defaults: with a live endpoint
// set and no model, the default model; with the default model and no
// prices, its prices. Another model without both prices is refused
// before any call: the spend cap is only as good as the prices. Without
// an endpoint getenv is returned unchanged.
func LiveArmEnv(getenv func(string) string) (func(string) string, error) {
	if strings.TrimSpace(getenv(EnvLLMURL)) == "" {
		return getenv, nil
	}
	set := map[string]string{}
	model := strings.TrimSpace(getenv(EnvLLMModel))
	if model == "" {
		model = DefaultLiveModel
		set[EnvLLMModel] = model
	}
	var missing []string
	for k, def := range map[string]string{EnvLLMPriceIn: DefaultLivePriceIn,
		EnvLLMPriceOut: DefaultLivePriceOut} {
		if strings.TrimSpace(getenv(k)) != "" {
			continue
		}
		if model != DefaultLiveModel {
			missing = append(missing, k)
			continue
		}
		set[k] = def
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("model %q is not the default (%s): set %s for it (the "+
			"spend cap is computed from them)", model, DefaultLiveModel,
			strings.Join(missing, " and "))
	}
	return func(k string) string {
		if v, ok := set[k]; ok {
			return v
		}
		return getenv(k)
	}, nil
}
