package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/startup"
)

// footprint-02, fleet integration on real PostgreSQL: two runtimes whose
// collectors run at 60 s and 120 s while the global config says 3600 s.
// Each analyzer's first cycle records its own collector's period, /metrics
// exports each database's inherited budget (3000 ms x 3600 / period), and
// the exceeded flags compare with those values, not the global one.

// selfCostRuntime starts a fleet runtime's collector and analyzer the way
// startMonitoring wires them: the collector on the runtime's own config
// (a clone whose interval self-config may have derived), the analyzer on
// the global config.
func selfCostRuntime(t *testing.T, pool *pgxpool.Pool, version int, name string,
	secs int) *databaseRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{
		spec: databaseRuntimeSpec{Scope: "fleet", Name: name, Pool: pool},
		ctx:  ctx, cancel: cancel, workers: &sync.WaitGroup{},
		checks: &startup.CheckResult{PGVersionNum: version},
	}
	rt.cfg = rt.runtimeConfig()
	rt.cfg.Collector.IntervalSeconds = secs
	rt.collector = rt.newCollector()
	rt.analyzer = analyzer.New(pool, cfg, rt.collector, nil, nil, nil, nil,
		logStructuredWrapper)
	rt.analyzer.WithDatabaseName(name)
	rt.start(func() { rt.collector.Run(rt.ctx) })
	rt.start(func() { rt.analyzer.Run(rt.ctx) })
	t.Cleanup(func() { cancel(); rt.workers.Wait() })
	return rt
}

// waitFirstSelfCost waits for each analyzer's first self-cost reading.
func waitFirstSelfCost(t *testing.T, rts ...*databaseRuntime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for _, rt := range rts {
		for rt.analyzer.SelfCost().At.IsZero() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: no self-cost reading within 2 minutes", rt.spec.Name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func selfCostFleetGlobals(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.Trust.Level = "fleet", "observation"
	cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds = 3600, 3600
	cfg.Analyzer.SelfCostBudgetMs = 3000
	cfg.SelfBudget.DBTimeMsPerHour = 0
	cfg.Safety.CPUCeilingPct = 100
	cfg.LLM.Enabled, cfg.Advisor.Enabled, cfg.Tuning.Enabled = false, false, false
	cfg.AutoExplain.Enabled = false
}

func TestFleetSelfCost_CycleSecondsAndInheritedBudgetPerRuntime(t *testing.T) {
	pool := autonomyPool(t)
	selfCostFleetGlobals(t)
	version := detectPGVersion(pool)
	fast := selfCostRuntime(t, pool, version, "fast", 60)
	slow := selfCostRuntime(t, pool, version, "slow", 120)
	waitFirstSelfCost(t, fast, slow)
	for _, tc := range []struct {
		rt   *databaseRuntime
		secs float64
	}{{fast, 60}, {slow, 120}} {
		c, u := tc.rt.analyzer.SelfCost(), tc.rt.analyzer.SelfBudgetUsage()
		if c.CycleSeconds != tc.secs || u.CycleSeconds != tc.secs {
			t.Errorf("%s: cost %+v usage %+v, want CycleSeconds %v (its collector), "+
				"not the global 3600", tc.rt.spec.Name, c, u, tc.secs)
		}
	}
	mgr := fleet.NewManager(cfg)
	for _, rt := range []*databaseRuntime{fast, slow} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: rt.spec.Name, Pool: pool,
			Collector: rt.collector, Analyzer: rt.analyzer, Status: &fleet.InstanceStatus{}})
	}
	var b strings.Builder
	writeSelfBudgetFromFleet(&b, mgr)
	out := b.String()
	for _, want := range []string{
		`pg_sage_self_budget{resource="db_time"} 3000`,
		`pg_sage_self_budget_database{database="fast",resource="db_time"} 180000`,
		`pg_sage_self_budget_database{database="slow",resource="db_time"} 90000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	assertExceededUsesEachRuntimesBudget(t, fast, slow)
}

// assertExceededUsesEachRuntimesBudget takes each runtime's measured usage
// (its real period and storage) and, as an hour of measured database time,
// 120000 ms on both: over the slow runtime's inherited budget only.
func assertExceededUsesEachRuntimesBudget(t *testing.T, fast, slow *databaseRuntime) {
	t.Helper()
	usages := map[string]selfbudget.Usage{}
	for _, rt := range []*databaseRuntime{fast, slow} {
		u := rt.analyzer.SelfBudgetUsage()
		u.DBKnown, u.DBTimeMsPerHour = true, 120_000
		usages[rt.spec.Name] = u
	}
	budget, inherited := selfBudgetFromConfig(cfg)
	var b strings.Builder
	writeSelfBudgetMetrics(&b, usages, selfbudget.CPUReading{}, budget, inherited, nil)
	out := b.String()
	for name, flag := range map[string]int{"fast": 0, "slow": 1} {
		want := fmt.Sprintf("pg_sage_self_budget_exceeded{database=%q,resource=\"db_time\"} %d",
			name, flag)
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q (the global 3000 ms/h would flag both):\n%s",
				want, out)
		}
	}
}
