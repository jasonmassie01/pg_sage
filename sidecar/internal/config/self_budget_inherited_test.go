package config

import (
	"testing"
	"time"
)

// footprint-02: the database-time budget inherited from
// analyzer.self_cost_budget_ms is per collector cycle, so its hourly value
// depends on the period that database's collector runs at. A database
// whose collector runs every 120 s has half the cycles, and so half the
// inherited hourly budget, of one at 60 s.
func TestInheritedBudget_ScalesThePerCycleBudgetToAnHour(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budgetMs int
		period   time.Duration
		want     float64
	}{
		{"60 s cycles", 3000, 60 * time.Second, 180_000},
		{"120 s cycles", 3000, 120 * time.Second, 90_000},
		{"90 s cycles", 3000, 90 * time.Second, 120_000},
		{"15 s cycles", 3000, 15 * time.Second, 720_000},
		{"sub-second period", 1, 500 * time.Millisecond, 7200},
		{"one-hour cycles", 3000, time.Hour, 3000},
	} {
		if got := InheritedDBTimeMsPerHour(tc.budgetMs, tc.period); got != tc.want {
			t.Errorf("%s: InheritedDBTimeMsPerHour(%d, %v) = %v, want %v", tc.name,
				tc.budgetMs, tc.period, got, tc.want)
		}
	}
}

// Boundary and invalid input: an unknown period (0, the collector has not
// started) or a disabled budget gives no inherited budget, never a silent
// 60 s cycle; a negative period or budget is treated the same way.
func TestInheritedBudget_UnknownPeriodOrDisabledBudgetIsZero(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budgetMs int
		period   time.Duration
	}{
		{"zero period", 3000, 0},
		{"negative period", 3000, -time.Second},
		{"budget disabled", 0, 60 * time.Second},
		{"negative budget", -5, 60 * time.Second},
		{"both zero", 0, 0},
	} {
		if got := InheritedDBTimeMsPerHour(tc.budgetMs, tc.period); got != 0 {
			t.Errorf("%s: InheritedDBTimeMsPerHour(%d, %v) = %v, want 0", tc.name,
				tc.budgetMs, tc.period, got)
		}
	}
}

// The global value keeps today's meaning: the configured interval, and an
// explicit self_budget.db_time_ms_per_hour wins over any inheritance.
func TestInheritedBudget_GlobalValueUsesTheConfiguredInterval(t *testing.T) {
	c := DefaultConfig()
	c.Analyzer.SelfCostBudgetMs = 3000
	c.Collector.IntervalSeconds = 120
	got, explicit := c.EffectiveSelfDBTimeMsPerHour()
	if explicit || got != InheritedDBTimeMsPerHour(3000, 120*time.Second) || got != 90_000 {
		t.Fatalf("inherited at 120 s = %v (explicit %v), want 90000 inherited", got, explicit)
	}
	c.SelfBudget.DBTimeMsPerHour = 50_000
	if got, explicit := c.EffectiveSelfDBTimeMsPerHour(); got != 50_000 || !explicit {
		t.Fatalf("explicit = %v (%v), want 50000 explicit", got, explicit)
	}
}

// No concurrent-access test: InheritedDBTimeMsPerHour is a pure function of
// its arguments and touches no shared state.
