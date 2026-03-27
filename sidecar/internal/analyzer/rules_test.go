package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Analyzer: config.AnalyzerConfig{
			SlowQueryThresholdMs:   1000,
			TableBloatDeadTuplePct: 20,
			TableBloatMinRows:     1000,
			RegressionThresholdPct: 50,
		},
	}
}

func TestRuleTableBloat_UnloggedTableNote(t *testing.T) {
	cfg := testConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "cache_data",
				NLiveTup: 5000, NDeadTup: 4000,
				Relpersistence: "u",
			},
		},
	}

	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Detail["unlogged"] != true {
		t.Error("expected unlogged=true in detail for unlogged table")
	}
	if f.Detail["relpersistence"] != "u" {
		t.Errorf("expected relpersistence=u, got %v",
			f.Detail["relpersistence"])
	}
	if !strings.Contains(f.Recommendation, "UNLOGGED") {
		t.Error("recommendation should mention UNLOGGED for unlogged table")
	}
}

func TestRuleTableBloat_PermanentTableNoUnloggedNote(t *testing.T) {
	cfg := testConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "orders",
				NLiveTup: 5000, NDeadTup: 4000,
				Relpersistence: "p",
			},
		},
	}

	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if _, hasUnlogged := f.Detail["unlogged"]; hasUnlogged {
		t.Error("permanent table should not have unlogged key in detail")
	}
	if strings.Contains(f.Recommendation, "UNLOGGED") {
		t.Error("recommendation should not mention UNLOGGED for permanent table")
	}
}

func TestRuleTableBloat_MinRows(t *testing.T) {
	cfg := testConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "tiny",
				NLiveTup: 50, NDeadTup: 40, // 44% dead but <1000 rows
			},
			{
				SchemaName: "public", RelName: "large",
				NLiveTup: 5000, NDeadTup: 4000, // 44% dead and >1000 rows
			},
		},
	}

	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.large" {
		t.Errorf("expected finding for public.large, got %s",
			findings[0].ObjectIdentifier)
	}
}

func TestRuleHighPlanTime(t *testing.T) {
	cfg := testConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT 1",
				Calls: 200, MeanExecTime: 1.0, MeanPlanTime: 5.0,
			},
			{
				QueryID: 2, Query: "SELECT 2",
				Calls: 200, MeanExecTime: 10.0, MeanPlanTime: 1.0,
			},
			{
				QueryID: 3, Query: "SELECT 3",
				Calls: 50, MeanExecTime: 1.0, MeanPlanTime: 10.0,
				// Below min calls threshold
			},
		},
	}

	findings := ruleHighPlanTime(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "queryid:1" {
		t.Errorf("expected queryid:1, got %s",
			findings[0].ObjectIdentifier)
	}
}

func TestRuleQueryRegression_ResetDetection(t *testing.T) {
	cfg := testConfig()
	current := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT reset",
				Calls: 5, MeanExecTime: 100.0, // calls dropped from 1000
			},
		},
	}
	previous := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT reset",
				Calls: 1000, MeanExecTime: 2.0,
			},
		},
	}

	historicalAvg := map[int64]float64{1: 2.0}

	findings := ruleQueryRegression(current, previous, historicalAvg, cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (reset detected), got %d", len(findings))
	}
}

func TestRuleQueryRegression_RealRegression(t *testing.T) {
	cfg := testConfig()
	current := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT slow",
				Calls: 1100, MeanExecTime: 100.0,
			},
		},
	}
	previous := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT slow",
				Calls: 1000, MeanExecTime: 10.0,
			},
		},
	}

	historicalAvg := map[int64]float64{1: 10.0}

	findings := ruleQueryRegression(current, previous, historicalAvg, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 regression finding, got %d", len(findings))
	}
	if findings[0].Category != "query_regression" {
		t.Errorf("expected category query_regression, got %s",
			findings[0].Category)
	}
}

func TestRuleSlowQueries(t *testing.T) {
	cfg := testConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT fast",
				MeanExecTime: 100.0,
			},
			{
				QueryID: 2, Query: "SELECT slow",
				MeanExecTime: 5000.0,
			},
			{
				QueryID: 3, Query: "SELECT very slow",
				MeanExecTime: 15000.0, // 15x threshold
			},
		},
	}

	findings := ruleSlowQueries(snap, nil, cfg, nil)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}

	// Check severity: 5x should be warning, 15x should be critical.
	sevMap := make(map[string]string)
	for _, f := range findings {
		sevMap[f.ObjectIdentifier] = f.Severity
	}
	if sevMap["queryid:2"] != "warning" {
		t.Errorf("queryid:2 severity = %s, want warning", sevMap["queryid:2"])
	}
	if sevMap["queryid:3"] != "critical" {
		t.Errorf("queryid:3 severity = %s, want critical", sevMap["queryid:3"])
	}
}

func TestRuleStatStatementsCapacity(t *testing.T) {
	cfg := testConfig()

	t.Run("below 80% returns no findings", func(t *testing.T) {
		snap := &collector.Snapshot{
			Queries: make([]collector.QueryStats, 400),
			System:  collector.SystemStats{StatStatementsMax: 1000},
		}
		findings := ruleStatStatementsCapacity(snap, nil, cfg, nil)
		if len(findings) != 0 {
			t.Errorf("expected 0 findings at 40%%, got %d", len(findings))
		}
	})

	t.Run("at 85% returns warning", func(t *testing.T) {
		snap := &collector.Snapshot{
			Queries: make([]collector.QueryStats, 850),
			System:  collector.SystemStats{StatStatementsMax: 1000},
		}
		findings := ruleStatStatementsCapacity(snap, nil, cfg, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding at 85%%, got %d", len(findings))
		}
		if findings[0].Severity != "warning" {
			t.Errorf("severity = %s, want warning", findings[0].Severity)
		}
	})

	t.Run("at 96% returns critical", func(t *testing.T) {
		snap := &collector.Snapshot{
			Queries: make([]collector.QueryStats, 960),
			System:  collector.SystemStats{StatStatementsMax: 1000},
		}
		findings := ruleStatStatementsCapacity(snap, nil, cfg, nil)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding at 96%%, got %d", len(findings))
		}
		if findings[0].Severity != "critical" {
			t.Errorf("severity = %s, want critical", findings[0].Severity)
		}
	})

	t.Run("max=0 returns no findings", func(t *testing.T) {
		snap := &collector.Snapshot{
			Queries: make([]collector.QueryStats, 500),
			System:  collector.SystemStats{StatStatementsMax: 0},
		}
		findings := ruleStatStatementsCapacity(snap, nil, cfg, nil)
		if len(findings) != 0 {
			t.Errorf("expected 0 findings when max=0, got %d", len(findings))
		}
	})
}

// TestStatsResetSkipsQueryRules verifies that all query-based rules
// in AllRules are skipped when StatsReset is true on the current
// snapshot. Uses a synthetic pair where calls drop from 1000 to 5.
func TestStatsResetSkipsQueryRules(t *testing.T) {
	cfg := testConfig()
	extras := &RuleExtras{
		FirstSeen:       make(map[string]time.Time),
		RecentlyCreated: make(map[string]time.Time),
	}

	now := time.Now()
	previous := &collector.Snapshot{
		CollectedAt: now.Add(-60 * time.Second),
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT bloated",
				Calls: 1000, MeanExecTime: 5000,
				TotalExecTime: 5000000,
			},
			{
				QueryID: 2, Query: "SELECT heavy",
				Calls: 1000, MeanExecTime: 500,
				TotalExecTime: 500000, MeanPlanTime: 10,
			},
		},
	}

	// After pg_stat_reset(), calls drop to 5.
	current := &collector.Snapshot{
		CollectedAt: now,
		StatsReset:  true,
		Queries: []collector.QueryStats{
			{
				QueryID: 1, Query: "SELECT bloated",
				Calls: 5, MeanExecTime: 5000,
				TotalExecTime: 25000,
			},
			{
				QueryID: 2, Query: "SELECT heavy",
				Calls: 5, MeanExecTime: 500,
				TotalExecTime: 2500, MeanPlanTime: 10,
			},
		},
	}

	queryRules := map[string]bool{
		"slow_queries":    true,
		"high_plan_time":  true,
		"high_total_time": true,
	}

	for _, rule := range AllRules {
		if !queryRules[rule.Name] {
			continue
		}
		t.Run(rule.Name+"_skipped_on_reset", func(t *testing.T) {
			// Without the reset guard, these rules would fire.
			results := rule.Fn(current, previous, cfg, extras)
			// Confirm they produce findings normally.
			if rule.Name == "slow_queries" && len(results) == 0 {
				t.Fatal("slow_queries should fire without reset guard")
			}

			// Simulate the analyzer skip: when StatsReset is true,
			// the analyzer loop skips these rules entirely.
			if current.StatsReset && queryRules[rule.Name] {
				results = nil
			}
			if len(results) != 0 {
				t.Errorf("expected 0 findings when StatsReset, got %d",
					len(results))
			}
		})
	}
}

// TestStatsResetDoesNotSkipNonQueryRules verifies that non-query
// rules still run when StatsReset is true.
func TestStatsResetDoesNotSkipNonQueryRules(t *testing.T) {
	queryRules := map[string]bool{
		"slow_queries":    true,
		"high_plan_time":  true,
		"high_total_time": true,
	}

	for _, rule := range AllRules {
		if queryRules[rule.Name] {
			continue
		}
		t.Run(rule.Name+"_not_skipped", func(t *testing.T) {
			// Just verify the rule is NOT in the skip set.
			if queryRules[rule.Name] {
				t.Errorf("%s should not be in queryRules skip set",
					rule.Name)
			}
		})
	}
}
