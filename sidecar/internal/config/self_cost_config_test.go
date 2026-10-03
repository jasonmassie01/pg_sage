package config

import (
	"strings"
	"testing"
)

// analyzer.self_cost_budget_ms: pg_sage's own database time per collector
// cycle above which the analyzer raises a sage_self_cost finding. Default
// 3000 ms (5% of one core at the default 60 s collector interval, the
// performance gate's budget), 0 disables, out of range is refused at load.

func TestSelfCostBudget_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SelfCostBudgetMs != 3000 || DefaultSelfCostBudgetMs != 3000 {
		t.Fatalf("default = %d (const %d), want 3000", cfg.Analyzer.SelfCostBudgetMs,
			DefaultSelfCostBudgetMs)
	}
	if DefaultConfig().Analyzer.SelfCostBudgetMs != 3000 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestSelfCostBudget_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "analyzer:\n  slow_query_threshold_ms: 500\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Analyzer.SelfCostBudgetMs != 3000 || cfg.Analyzer.SlowQueryThresholdMs != 500 {
		t.Fatalf("analyzer = %+v, want the default budget kept", cfg.Analyzer)
	}
}

func TestSelfCostBudget_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{{"0", 0}, {"1", 1}, {"10000", 10000}, {"3600000", 3600000}} {
		cfg, err := loadRCAYAML(t, "analyzer:\n  self_cost_budget_ms: "+tc.yaml+"\n")
		if err != nil || cfg.Analyzer.SelfCostBudgetMs != tc.want {
			t.Errorf("%s: got %d (%v), want %d", tc.yaml, cfg.Analyzer.SelfCostBudgetMs,
				err, tc.want)
		}
	}
}

func TestSelfCostBudget_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"-1", "3600001"} {
		_, err := loadRCAYAML(t, "analyzer:\n  self_cost_budget_ms: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "analyzer.self_cost_budget_ms") {
			t.Errorf("%s: err = %v, want a refusal naming the key", v, err)
		}
	}
}
