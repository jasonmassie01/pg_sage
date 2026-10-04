package srebench

import (
	"strings"
	"testing"
)

// Owner addition B (2026-10-04): the nightly arm's model is not
// hard-coded in the workflow. pg_sage has no default LLM model setting
// (llm.model defaults to empty), so the live arm's default lives here,
// next to the prices its spend cap is computed with; the repository
// variable PG_SAGE_BENCH_OPENAI_MODEL overrides it. A model other than
// the default must come with its own prices: the spend cap is only as
// good as the prices, so a run with another model and no prices fails
// closed before any call.

func liveEnv(m map[string]string) func(string) string {
	base := map[string]string{EnvLLMURL: "https://api.openai.com/v1", EnvLLMKey: "k"}
	for k, v := range m {
		base[k] = v
	}
	return env(base)
}

func TestLiveArmEnv_DefaultModelWithItsPrices(t *testing.T) {
	for name, in := range map[string]map[string]string{
		"unset":            {},
		"blank":            {EnvLLMModel: "  "},
		"default by name":  {EnvLLMModel: DefaultLiveModel},
		"blank prices too": {EnvLLMPriceIn: " ", EnvLLMPriceOut: ""},
	} {
		got, err := LiveArmEnv(liveEnv(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got(EnvLLMModel) != DefaultLiveModel || got(EnvLLMPriceIn) != DefaultLivePriceIn ||
			got(EnvLLMPriceOut) != DefaultLivePriceOut {
			t.Fatalf("%s: model %q prices %q/%q", name, got(EnvLLMModel), got(EnvLLMPriceIn),
				got(EnvLLMPriceOut))
		}
		if got(EnvLLMKey) != "k" || got(EnvLLMURL) == "" {
			t.Fatalf("%s: other settings changed", name)
		}
	}
	if DefaultLiveModel == "" || DefaultLivePriceIn == "" || DefaultLivePriceOut == "" {
		t.Fatal("the default model and its prices must be set")
	}
}

func TestLiveArmEnv_ExplicitPricesWinForTheDefaultModel(t *testing.T) {
	got, err := LiveArmEnv(liveEnv(map[string]string{EnvLLMPriceIn: "0.2",
		EnvLLMPriceOut: "0.9"}))
	if err != nil || got(EnvLLMModel) != DefaultLiveModel || got(EnvLLMPriceIn) != "0.2" ||
		got(EnvLLMPriceOut) != "0.9" {
		t.Fatalf("model %q prices %q/%q (%v)", got(EnvLLMModel), got(EnvLLMPriceIn),
			got(EnvLLMPriceOut), err)
	}
}

func TestLiveArmEnv_AnotherModelNeedsItsOwnPrices(t *testing.T) {
	for name, c := range map[string]struct {
		in      map[string]string
		missing []string
	}{
		"no prices": {map[string]string{EnvLLMModel: "o9-large"},
			[]string{EnvLLMPriceIn, EnvLLMPriceOut}},
		"no output price": {map[string]string{EnvLLMModel: "o9-large", EnvLLMPriceIn: "3"},
			[]string{EnvLLMPriceOut}},
		"no input price": {map[string]string{EnvLLMModel: "o9-large", EnvLLMPriceOut: "9"},
			[]string{EnvLLMPriceIn}},
	} {
		_, err := LiveArmEnv(liveEnv(c.in))
		if err == nil {
			t.Fatalf("%s: another model without its prices was accepted", name)
		}
		for _, k := range append(c.missing, "o9-large") {
			if !strings.Contains(err.Error(), k) {
				t.Errorf("%s: error %q does not name %s", name, err, k)
			}
		}
	}
	got, err := LiveArmEnv(liveEnv(map[string]string{EnvLLMModel: "o9-large",
		EnvLLMPriceIn: "3", EnvLLMPriceOut: "9"}))
	if err != nil || got(EnvLLMModel) != "o9-large" || got(EnvLLMPriceIn) != "3" ||
		got(EnvLLMPriceOut) != "9" {
		t.Fatalf("priced model: %q %q/%q (%v)", got(EnvLLMModel), got(EnvLLMPriceIn),
			got(EnvLLMPriceOut), err)
	}
}

// Without an endpoint there is no live model to default: the settings
// pass through unchanged (and the arm refuses to measure the fake).
func TestLiveArmEnv_NoEndpointIsUnchanged(t *testing.T) {
	got, err := LiveArmEnv(env(map[string]string{}))
	if err != nil || got(EnvLLMModel) != "" || got(EnvLLMPriceIn) != "" {
		t.Fatalf("model %q price %q (%v)", got(EnvLLMModel), got(EnvLLMPriceIn), err)
	}
}

// The defaulted settings make a complete live configuration.
func TestLiveArmEnv_FeedsALiveConfiguration(t *testing.T) {
	in := map[string]string{EnvLiveLLM: "1", EnvLLMMaxRequests: "10",
		EnvLLMMaxTokens: "1000", EnvLLMMaxWall: "1m", EnvLLMMaxSpend: "1"}
	getenv, err := LiveArmEnv(liveEnv(in))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LLMConfigFromEnv(getenv)
	if err != nil || cfg.Mode != LLMLive || cfg.Model != DefaultLiveModel ||
		cfg.Caps.InputUSDPerMTok <= 0 || cfg.Caps.OutputUSDPerMTok <= 0 {
		t.Fatalf("config = %+v (%v)", cfg, err)
	}
}
