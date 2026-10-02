package config

import (
	"strings"
	"testing"
)

// llm.token_parameter and llm.tool_reasoning_effort tell the client how to
// name the completion cap and which reasoning_effort to send with tools.
// Both default to "auto": adapt to the provider's 400 once per model.

func TestLLMWireDefaultsAreAuto(t *testing.T) {
	built := DefaultConfig()
	if built.LLM.TokenParameter != "auto" || built.LLM.ToolReasoningEffort != "auto" {
		t.Fatalf("DefaultConfig wire = %q/%q, want auto/auto",
			built.LLM.TokenParameter, built.LLM.ToolReasoningEffort)
	}
	if DefaultLLMTokenParameter != "auto" || DefaultLLMToolReasoningEffort != "auto" {
		t.Fatal("exported defaults are not auto")
	}
	loaded := loadNoConfigFile(t)
	if loaded.LLM.TokenParameter != "auto" || loaded.LLM.ToolReasoningEffort != "auto" {
		t.Fatalf("Load without a file = %q/%q, want auto/auto",
			loaded.LLM.TokenParameter, loaded.LLM.ToolReasoningEffort)
	}
	partial := loadYAMLConfig(t, "llm:\n  model: gpt-6-luna\n")
	if partial.LLM.TokenParameter != "auto" || partial.LLM.ToolReasoningEffort != "auto" {
		t.Fatalf("partial llm section = %q/%q, want auto/auto (default masking)",
			partial.LLM.TokenParameter, partial.LLM.ToolReasoningEffort)
	}
}

func TestLLMWireValuesLoadFromYAML(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  token_parameter: max_completion_tokens\n"+
		"  tool_reasoning_effort: none\n")
	if cfg.LLM.TokenParameter != "max_completion_tokens" ||
		cfg.LLM.ToolReasoningEffort != "none" {
		t.Fatalf("loaded = %q/%q", cfg.LLM.TokenParameter, cfg.LLM.ToolReasoningEffort)
	}
}

func TestLLMWireValidation(t *testing.T) {
	for _, v := range []string{"", "auto", "max_tokens", "max_completion_tokens"} {
		cfg := DefaultConfig()
		cfg.LLM.TokenParameter = v
		if err := cfg.LLM.validateWire(); err != nil {
			t.Errorf("token_parameter %q rejected: %v", v, err)
		}
	}
	for _, v := range []string{"", "auto", "none", "low", "medium", "high", "omit"} {
		cfg := DefaultConfig()
		cfg.LLM.ToolReasoningEffort = v
		if err := cfg.LLM.validateWire(); err != nil {
			t.Errorf("tool_reasoning_effort %q rejected: %v", v, err)
		}
	}
	bad := []struct{ token, effort, field string }{
		{"max-tokens", "auto", "llm.token_parameter"},
		{"MAX_TOKENS", "auto", "llm.token_parameter"},
		{" auto", "auto", "llm.token_parameter"},
		{"auto", "minimal", "llm.tool_reasoning_effort"},
		{"auto", "None", "llm.tool_reasoning_effort"},
		{"auto", "off", "llm.tool_reasoning_effort"},
	}
	for _, b := range bad {
		cfg := DefaultConfig()
		cfg.LLM.TokenParameter, cfg.LLM.ToolReasoningEffort = b.token, b.effort
		err := cfg.LLM.validateWire()
		if err == nil || !strings.Contains(err.Error(), b.field) {
			t.Errorf("%q/%q: err = %v, want a %s error", b.token, b.effort, err, b.field)
		}
	}
}

// Config.validate (Load and reload) refuses an invalid value.
func TestLLMWireValidationRunsOnLoad(t *testing.T) {
	_, err := loadYAMLResult(t, standaloneIOYAML+
		"llm:\n  token_parameter: max_output_tokens\n")
	if err == nil || !strings.Contains(err.Error(), "llm.token_parameter") {
		t.Fatalf("err = %v, want llm.token_parameter validation error", err)
	}
}

func TestLLMWireDocsAndLifecycle(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"llm.token_parameter", "llm.tool_reasoning_effort"} {
		if n := len(docs[key]); n < 20 || n > 200 {
			t.Errorf("%s doc length %d outside [20,200]", key, n)
		}
		lc, ok := LookupFieldLifecycle(key)
		if !ok || lc.Lifecycle != LifecycleReconfigure || lc.Owner != "llm" {
			t.Errorf("%s lifecycle = %+v (%v), want reconfigure by llm", key, lc, ok)
		}
	}
}
