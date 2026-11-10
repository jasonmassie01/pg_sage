package config

import (
	"strings"
	"testing"
)

// llm-05: llm.context_budget_tokens is the trim target for attached
// context; 0 means no extra trim and a negative value is a typo that
// fails at load.

func TestContextBudgetDefaultWithoutConfigFile(t *testing.T) {
	cfg := loadNoConfigFile(t)
	if cfg.LLM.ContextBudgetTokens != 8192 || DefaultLLMContextBudget != 8192 {
		t.Fatalf("context_budget_tokens = %d (default %d), want 8192",
			cfg.LLM.ContextBudgetTokens, DefaultLLMContextBudget)
	}
	partial := loadYAMLConfig(t, "llm:\n  model: m\n")
	if partial.LLM.ContextBudgetTokens != 8192 {
		t.Fatalf("partial llm section = %d, want 8192 (default masking)",
			partial.LLM.ContextBudgetTokens)
	}
}

func TestContextBudgetZeroLoads(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  context_budget_tokens: 0\n")
	if cfg.LLM.ContextBudgetTokens != 0 {
		t.Fatalf("context_budget_tokens = %d, want an explicit 0 kept",
			cfg.LLM.ContextBudgetTokens)
	}
}

func TestContextBudgetNegativeRejected(t *testing.T) {
	for _, v := range []int{-1, -8192} {
		cfg := DefaultConfig()
		cfg.LLM.ContextBudgetTokens = v
		err := cfg.validate()
		if err == nil || !strings.Contains(err.Error(), "llm.context_budget_tokens") {
			t.Fatalf("budget %d: err = %v, want a context_budget_tokens error", v, err)
		}
	}
	for _, v := range []int{0, 1, 2048, 1 << 20} {
		cfg := DefaultConfig()
		cfg.LLM.ContextBudgetTokens = v
		if err := cfg.LLM.validateWire(); err != nil {
			t.Fatalf("budget %d rejected: %v", v, err)
		}
	}
}
