package config

import (
	"math"
	"strings"
	"testing"
)

// llm-04: llm.pricing is operator-supplied per-model prices in USD per
// million tokens. It is empty by default, and pg_sage never guesses one.
//
// No concurrent-access tests: validation is a pure function of the config.
// No database: prices are config only.

func TestLLMPricingDefaultIsEmpty(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.LLM.Pricing) != 0 {
		t.Fatalf("llm.pricing default = %v, want empty", cfg.LLM.Pricing)
	}
}

func TestLLMPricingLoadsFromYAML(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+`llm:
  pricing:
    gpt-4o-mini: {input_per_m: 0.15, output_per_m: 0.6}
    o3-mini: {input_per_m: 1.1, output_per_m: 4.4, reasoning_per_m: 4.4}
    local-llama: {input_per_m: 0, output_per_m: 0}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.LLM.Pricing
	mini, o3, local := p["gpt-4o-mini"], p["o3-mini"], p["local-llama"]
	if len(p) != 3 || mini.InputPerM == nil || *mini.InputPerM != 0.15 ||
		*mini.OutputPerM != 0.6 || mini.ReasoningPerM != nil {
		t.Fatalf("pricing = %+v, want gpt-4o-mini 0.15/0.6 without a reasoning price", p)
	}
	if o3.ReasoningPerM == nil || *o3.ReasoningPerM != 4.4 {
		t.Fatalf("o3-mini = %+v, want reasoning 4.4", o3)
	}
	if local.InputPerM == nil || *local.InputPerM != 0 || *local.OutputPerM != 0 {
		t.Fatalf("local-llama = %+v, want an explicit zero price", local)
	}
}

// Invalid input: a negative, non-finite or missing price, an unknown price
// kind and an empty model name are refused at load, naming the key.
func TestLLMPricingRefusesInvalid(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"negative input": {"m: {input_per_m: -1, output_per_m: 1}",
			"llm.pricing.m.input_per_m"},
		"negative reasoning": {"m: {input_per_m: 1, output_per_m: 1, reasoning_per_m: -0.1}",
			"llm.pricing.m.reasoning_per_m"},
		"nan output":     {"m: {input_per_m: 1, output_per_m: .nan}", "llm.pricing.m.output_per_m"},
		"inf input":      {"m: {input_per_m: .inf, output_per_m: 1}", "llm.pricing.m.input_per_m"},
		"missing output": {"m: {input_per_m: 1}", "llm.pricing.m.output_per_m"},
		"missing input":  {"m: {output_per_m: 1}", "llm.pricing.m.input_per_m"},
		"unknown kind":   {"m: {input_per_m: 1, output_per_m: 1, cached_per_m: 1}", "cached_per_m"},
		"empty model":    {`"": {input_per_m: 1, output_per_m: 1}`, "llm.pricing"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAMLResult(t, standaloneIOYAML+"llm:\n  pricing:\n    "+c.yaml+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one naming %s", err, c.want)
			}
		})
	}
}

// Boundary: a zero price is valid (a free local model); the smallest
// negative is not.
func TestLLMPricingValidateBoundary(t *testing.T) {
	zero, tiny := 0.0, -math.SmallestNonzeroFloat64
	ok := LLMConfig{Pricing: map[string]LLMPrice{"m": {InputPerM: &zero, OutputPerM: &zero}}}
	if err := ok.validatePricing(); err != nil {
		t.Fatalf("zero price refused: %v", err)
	}
	bad := LLMConfig{Pricing: map[string]LLMPrice{"m": {InputPerM: &tiny, OutputPerM: &zero}}}
	if err := bad.validatePricing(); err == nil {
		t.Fatal("a negative price passed")
	}
	if err := (LLMConfig{}).validatePricing(); err != nil {
		t.Fatalf("nil pricing refused: %v", err)
	}
}

func TestLLMPricingDocAndLifecycle(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	if n := len(docs["llm.pricing"]); n < 20 || n > 200 {
		t.Errorf("llm.pricing doc length %d outside [20,200]", n)
	}
	lc, ok := LookupFieldLifecycle("llm.pricing")
	if !ok || lc.Lifecycle != LifecycleRestart {
		t.Errorf("llm.pricing lifecycle = %+v (%v), want restart", lc, ok)
	}
}
