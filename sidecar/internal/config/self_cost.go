package config

import "fmt"

// DefaultSelfCostBudgetMs is pg_sage's own database time per collector
// cycle, in milliseconds, above which the analyzer raises a
// sage_self_cost finding: 5% of one core at the default 60 s collector
// interval, the performance gate's budget (perf v1.8.3).
const DefaultSelfCostBudgetMs = 3000

// maxSelfCostBudgetMs is one hour of database time per cycle.
const maxSelfCostBudgetMs = 3_600_000

// validateSelfCostBudget refuses a budget outside 0-3600000 (0 disables).
func (a AnalyzerConfig) validateSelfCostBudget() error {
	if a.SelfCostBudgetMs < 0 || a.SelfCostBudgetMs > maxSelfCostBudgetMs {
		return fmt.Errorf("analyzer.self_cost_budget_ms must be 0-%d, got %d",
			maxSelfCostBudgetMs, a.SelfCostBudgetMs)
	}
	return nil
}
