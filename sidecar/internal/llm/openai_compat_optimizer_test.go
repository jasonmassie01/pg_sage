package llm

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// The dedicated optimizer client inherits the wire overrides of llm.*
// (like json_mode): they describe the provider, not the tier.
func TestNewOptimizerClient_InheritsWireOverrides(t *testing.T) {
	parent := &config.LLMConfig{Enabled: true, Endpoint: "http://p", APIKey: "k",
		Model: "gpt-6-luna", TokenParameter: "max_completion_tokens",
		ToolReasoningEffort: "none"}
	c := NewOptimizerClient(parent, &config.OptimizerLLMConfig{Enabled: true}, noopLog)
	cfg, _ := c.configSnapshot()
	if cfg.TokenParameter != "max_completion_tokens" || cfg.ToolReasoningEffort != "none" {
		t.Fatalf("optimizer wire overrides = %q/%q, want inherited",
			cfg.TokenParameter, cfg.ToolReasoningEffort)
	}
}
