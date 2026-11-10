package store

import (
	"strings"
	"testing"
)

// llm-05: llm.context_budget_tokens is a trim target where 0 turns the
// extra trim off, as it does in YAML; a negative value is refused.
func TestValidateConfigOverride_ContextBudgetTokens(t *testing.T) {
	for _, ok := range []string{"0", "1", "2048", "8192"} {
		if err := ValidateConfigOverride("llm.context_budget_tokens", ok); err != nil {
			t.Errorf("context_budget_tokens %s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"-1", "abc", ""} {
		err := ValidateConfigOverride("llm.context_budget_tokens", bad)
		if err == nil || !strings.Contains(err.Error(), "context_budget_tokens") {
			t.Errorf("context_budget_tokens %q: err = %v, want a keyed refusal", bad, err)
		}
	}
}
