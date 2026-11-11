package analyzer

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// footprint-02: pg_sage's own cost is normalized to the period this
// database's collector actually runs at, not to the global
// collector.interval_seconds. In fleet mode a database's collector runs on
// its own (self-configured) interval while the analyzer holds the global
// config, so the per-cycle figures follow the collector. Without a running
// collector the period is unknown: the cost is unknown and neither
// category is evaluated (no silent 60 s).

// startedCollector is a collector whose ticker period is cfg's interval:
// Run records the period and, with a cancelled context, returns without
// collecting.
func startedCollector(cfg *config.Config) *collector.Collector {
	c := collector.New(nil, cfg, 160000, noopLog)
	runStopped(c)
	return c
}

// runStopped runs c once with a cancelled context, so it (re)reads its
// configured interval as the period in use.
func runStopped(c *collector.Collector) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
}

// collectorConfig is a copy of cfg whose collector runs every secs.
func collectorConfig(cfg *config.Config, secs int) *config.Config {
	c := *cfg
	c.Collector.IntervalSeconds = secs
	return &c
}

func TestCycleSeconds_PeriodIsTheRunningCollectors(t *testing.T) {
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	if got := New(nil, cfg, nil, nil, nil, nil, nil, noopLog).collectorPeriod(); got != 0 {
		t.Fatalf("nil collector period = %v, want 0 (unknown)", got)
	}
	idle := collector.New(nil, collectorConfig(cfg, 120), 160000, noopLog)
	a := New(nil, cfg, idle, nil, nil, nil, nil, noopLog)
	if got := a.collectorPeriod(); got != 0 {
		t.Fatalf("collector not started: period = %v, want 0 (unknown)", got)
	}
	runStopped(idle)
	if got := a.collectorPeriod(); got != 120*time.Second {
		t.Fatalf("period = %v, want the collector's 120 s while cfg says 60 s", got)
	}
}

// selfCostPeriodAnalyzer is an analyzer on pool whose config says 60 s;
// database time per hour is the only enabled self-budget resource (set
// far above any test's use, so it never breaches but needs a known
// period) and the per-cycle DB-time budget is 3000 ms. The process CPU is
// not the analyzer's (footprint-07).
func selfCostPeriodAnalyzer(t *testing.T, coll *collector.Collector) *Analyzer {
	t.Helper()
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	cfg.Analyzer.SelfCostBudgetMs = 3000
	cfg.SelfBudget = config.SelfBudgetConfig{DBTimeMsPerHour: 3_600_000}
	return New(phase2Pool(t), cfg, coll, nil, nil, nil, nil, noopLog)
}

// Happy path: the collector runs at 120 s while cfg says 60 s. The cost
// and the usage record 120 s cycles.
func TestCheckSelfCost_CycleSecondsFollowsTheCollector(t *testing.T) {
	cfg := phase2Config()
	a := selfCostPeriodAnalyzer(t, startedCollector(collectorConfig(cfg, 120)))
	ctx := context.Background()
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	c := a.SelfCost()
	if !c.Known || c.CycleSeconds != 120 {
		t.Fatalf("SelfCost() = %+v, want known with CycleSeconds 120", c)
	}
	u := a.SelfBudgetUsage()
	if u.CycleSeconds != 120 || u.CPUKnown {
		t.Fatalf("SelfBudgetUsage() = %+v, want 120 s cycles and no CPU", u)
	}
	if !a.eval.ok[categorySelfBudget] {
		t.Fatalf("budget category not evaluated with a known period: %+v", a.eval)
	}
}

// State transition: a period promoted between two analyzer cycles (60 s
// to 120 s) is the period of the next cost.
func TestCheckSelfCost_CycleSecondsFollowsAPromotedPeriod(t *testing.T) {
	collCfg := collectorConfig(phase2Config(), 60)
	coll := startedCollector(collCfg)
	a := selfCostPeriodAnalyzer(t, coll)
	ctx := context.Background()
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	if c, u := a.SelfCost(), a.SelfBudgetUsage(); c.CycleSeconds != 60 ||
		u.CycleSeconds != 60 {
		t.Fatalf("at 60 s: cost %+v usage %+v, want 60 s cycles", c, u)
	}
	collCfg.Collector.IntervalSeconds = 120
	runStopped(coll)
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	if c, u := a.SelfCost(), a.SelfBudgetUsage(); c.CycleSeconds != 120 ||
		u.CycleSeconds != 120 {
		t.Fatalf("after promotion: cost %+v usage %+v, want 120 s cycles", c, u)
	}
}

// Boundary: without a collector, or before it starts (period 0), the cost
// is unknown and neither category is evaluated, so an open finding stays
// open. The sage schema's size is still measured.
func TestCheckSelfCost_CycleSecondsUnknownWithoutAPeriod(t *testing.T) {
	idle := collector.New(nil, collectorConfig(phase2Config(), 60), 160000, noopLog)
	for name, coll := range map[string]*collector.Collector{
		"nil collector": nil, "collector not started": idle,
	} {
		t.Run(name, func(t *testing.T) {
			a := selfCostPeriodAnalyzer(t, coll)
			ctx := context.Background()
			a.eval = newCycleEval()
			a.checkSelfCost(ctx)
			a.eval = newCycleEval()
			if got := a.checkSelfCost(ctx); len(got) != 0 {
				t.Fatalf("unknown period raised %+v", got)
			}
			c, u := a.SelfCost(), a.SelfBudgetUsage()
			if c.Known || c.DBTimeKnown || c.CycleSeconds != 0 || c.SchemaBytes <= 0 {
				t.Fatalf("SelfCost() = %+v, want unknown, no cycle, schema measured", c)
			}
			if u.CPUKnown || u.DBKnown || u.CycleSeconds != 0 || !u.StorageKnown {
				t.Fatalf("SelfBudgetUsage() = %+v, want CPU and DB unknown", u)
			}
			if !a.eval.failed[categorySelfCost] || !a.eval.failed[categorySelfBudget] {
				t.Fatalf("eval = %+v, want both categories unknown", a.eval)
			}
		})
	}
}

// Error propagation: nothing new. With a running collector a failed
// reading still fails both categories and records no cost.
func TestCheckSelfCost_CycleSecondsReadErrorFailsBoth(t *testing.T) {
	a := selfCostPeriodAnalyzer(t, startedCollector(collectorConfig(phase2Config(), 120)))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	a.eval = newCycleEval()
	if got := a.checkSelfCost(canceled); len(got) != 0 {
		t.Fatalf("failed read raised %+v", got)
	}
	if !a.eval.failed[categorySelfCost] || !a.eval.failed[categorySelfBudget] {
		t.Fatalf("eval = %+v, want both categories unknown", a.eval)
	}
	if c := a.SelfCost(); !c.At.IsZero() || c.CycleSeconds != 0 {
		t.Fatalf("failed read recorded a cost: %+v", c)
	}
}

// No concurrent-access test of its own: the period is an atomic read of
// the collector (collector_interval_test covers it under a running
// ticker) and the meters it feeds are already guarded and tested.
