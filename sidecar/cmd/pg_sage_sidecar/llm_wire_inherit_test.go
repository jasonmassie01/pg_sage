package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// optimizerLLMConfig mirrors llm.NewOptimizerClient, including the
// provider wire overrides llm.token_parameter and llm.tool_reasoning_effort.
func TestOptimizerLLMConfig_InheritsWireOverrides(t *testing.T) {
	parent := config.LLMConfig{Enabled: true, Endpoint: "http://p", APIKey: "k",
		Model: "gpt-6-luna", TokenParameter: "max_tokens", ToolReasoningEffort: "omit",
		OptimizerLLM: config.OptimizerLLMConfig{Enabled: true}}
	merged := optimizerLLMConfig(parent)
	if merged.TokenParameter != "max_tokens" || merged.ToolReasoningEffort != "omit" {
		t.Fatalf("merged wire overrides = %q/%q, want inherited",
			merged.TokenParameter, merged.ToolReasoningEffort)
	}
	general := derivedLLMConfig(parent, llmRoleGeneral, true)
	if general.TokenParameter != "max_tokens" || general.ToolReasoningEffort != "omit" {
		t.Fatalf("general wire overrides = %q/%q", general.TokenParameter,
			general.ToolReasoningEffort)
	}
}
