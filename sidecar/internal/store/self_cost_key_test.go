package store

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// analyzer.self_cost_budget_ms is an API-settable non-negative number of
// milliseconds (0 disables the finding); it is listed with its value.
func TestSelfCostBudgetOverride(t *testing.T) {
	const key = "analyzer.self_cost_budget_ms"
	for _, ok := range []string{"0", "3000", "600000"} {
		if err := ValidateConfigOverride(key, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"-1", "three", "", "1.5"} {
		if err := ValidateConfigOverride(key, bad); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want a refusal naming the key", bad, err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Analyzer.SelfCostBudgetMs = 4321
	m := map[string]any{}
	addAnalyzerFields(m, &cfg.Analyzer)
	field, ok := m[key].(map[string]any)
	if !ok || field["value"] != 4321 {
		t.Fatalf("listed field = %#v, want value 4321", m[key])
	}
}
