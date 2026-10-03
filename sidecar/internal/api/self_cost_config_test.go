package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// analyzer.self_cost_budget_ms can be written through the API: the
// override reaches the configuration the analyzer reads.
func TestHotReloadAnalyzer_SelfCostBudgetMs(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, tc := range []struct {
		value string
		want  int
	}{{"5000", 5000}, {"0", 0}, {"1", 1}} {
		hotReloadAnalyzer(cfg, "analyzer.self_cost_budget_ms", tc.value)
		if cfg.Analyzer.SelfCostBudgetMs != tc.want {
			t.Fatalf("after %q: %d, want %d", tc.value, cfg.Analyzer.SelfCostBudgetMs, tc.want)
		}
	}
}
