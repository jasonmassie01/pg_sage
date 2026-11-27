package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// footprint-02: the database-time budget inherited from
// analyzer.self_cost_budget_ms follows each database's collector period.
// Two databases using the same 120000 ms of database time per hour: at
// 60 s cycles the inherited budget is 180000 ms/h (not exceeded), at
// 120 s cycles it is 90000 ms/h (exceeded). The global gauge keeps the
// configured interval's value.
func TestInheritedBudget_ExceededFollowsEachDatabasesCycleSeconds(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"fast": {DBKnown: true, DBTimeMsPerHour: 120_000, CycleSeconds: 60},
		"slow": {DBKnown: true, DBTimeMsPerHour: 120_000, CycleSeconds: 120},
	}, selfbudget.CPUReading{}, selfbudget.Budget{DBTimeMsPerHour: 180_000}, 3000, nil)
	out := b.String()
	for _, want := range []string{
		`pg_sage_self_budget{resource="db_time"} 180000`,
		"# TYPE pg_sage_self_budget_database gauge",
		`pg_sage_self_budget_database{database="fast",resource="db_time"} 180000`,
		`pg_sage_self_budget_database{database="slow",resource="db_time"} 90000`,
		`pg_sage_self_budget_exceeded{database="fast",resource="db_time"} 0`,
		`pg_sage_self_budget_exceeded{database="slow",resource="db_time"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// An explicit self_budget.db_time_ms_per_hour wins: no per-database series
// and every database is compared with the explicit value.
func TestInheritedBudget_ExplicitValueHasNoPerDatabaseSeries(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"fast": {DBKnown: true, DBTimeMsPerHour: 120_000, CycleSeconds: 60},
		"slow": {DBKnown: true, DBTimeMsPerHour: 120_000, CycleSeconds: 120},
	}, selfbudget.CPUReading{}, selfbudget.Budget{DBTimeMsPerHour: 100_000}, 0, nil)
	out := b.String()
	if strings.Contains(out, "pg_sage_self_budget_database") {
		t.Fatalf("explicit budget exported a per-database budget:\n%s", out)
	}
	for _, want := range []string{
		`pg_sage_self_budget_exceeded{database="fast",resource="db_time"} 1`,
		`pg_sage_self_budget_exceeded{database="slow",resource="db_time"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// Boundary: usage exactly at the inherited budget is not exceeded; a
// database whose period is unknown (first cycle, no collector) has no
// per-database budget and no db_time flag, only what it measured.
func TestInheritedBudget_BoundaryAndUnknownPeriod(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"edge":    {DBKnown: true, DBTimeMsPerHour: 90_000, CycleSeconds: 120},
		"unknown": {StorageKnown: true, StorageBytes: 4096},
	}, selfbudget.CPUReading{},
		selfbudget.Budget{DBTimeMsPerHour: 180_000, StorageBytes: 1 << 30}, 3000, nil)
	out := b.String()
	for _, want := range []string{
		`pg_sage_self_budget_database{database="edge",resource="db_time"} 90000`,
		`pg_sage_self_budget_exceeded{database="edge",resource="db_time"} 0`,
		`pg_sage_self_budget_exceeded{database="unknown",resource="storage"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		`pg_sage_self_budget_database{database="unknown"`,
		`pg_sage_self_budget_exceeded{database="unknown",resource="db_time"}`,
	} {
		if strings.Contains(out, absent) {
			t.Errorf("unknown period exported %q:\n%s", absent, out)
		}
	}
}

// selfBudgetFromConfig is what /metrics compares with: the inherited
// per-cycle budget only when self_budget does not set database time.
func TestInheritedBudget_FromConfig(t *testing.T) {
	c := config.DefaultConfig()
	c.Collector.IntervalSeconds = 3600
	c.Analyzer.SelfCostBudgetMs = 3000
	c.SelfBudget.DBTimeMsPerHour = 0
	budget, inherited := selfBudgetFromConfig(c)
	if inherited != 3000 || budget.DBTimeMsPerHour != 3000 ||
		budget.CPUMsPerCycle != float64(c.SelfBudget.CPUMsPerCycle) ||
		budget.StorageBytes != int64(c.SelfBudget.StorageMB)<<20 {
		t.Fatalf("inherited: budget %+v inherited %d, want 3000 ms/cycle at 3600 s",
			budget, inherited)
	}
	c.SelfBudget.DBTimeMsPerHour = 50_000
	if budget, inherited = selfBudgetFromConfig(c); inherited != 0 ||
		budget.DBTimeMsPerHour != 50_000 {
		t.Fatalf("explicit: budget %+v inherited %d, want 50000 and no inheritance",
			budget, inherited)
	}
	c.SelfBudget.DBTimeMsPerHour, c.Analyzer.SelfCostBudgetMs = 0, 0
	if budget, inherited = selfBudgetFromConfig(c); inherited != 0 ||
		budget.DBTimeMsPerHour != 0 {
		t.Fatalf("disabled: budget %+v inherited %d, want none", budget, inherited)
	}
}
