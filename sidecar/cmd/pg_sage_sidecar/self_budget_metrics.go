package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// writeSelfBudgetFromFleet exports pg_sage's declared self-budget and its
// usage (roadmap phase 3) as measured by each instance's analyzer.
func writeSelfBudgetFromFleet(b *strings.Builder, mgr *fleet.DatabaseManager) {
	if mgr == nil || cfg == nil {
		return
	}
	usages := map[string]selfbudget.Usage{}
	for name, inst := range mgr.Instances() {
		if inst != nil && inst.Analyzer != nil {
			usages[name] = inst.Analyzer.SelfBudgetUsage()
		}
	}
	dbPerHour, _ := cfg.EffectiveSelfDBTimeMsPerHour()
	budget := selfbudget.Budget{CPUMsPerCycle: float64(cfg.SelfBudget.CPUMsPerCycle),
		DBTimeMsPerHour: dbPerHour, BlocksPerHour: float64(cfg.SelfBudget.BlocksPerHour),
		StorageBytes: int64(cfg.SelfBudget.StorageMB) << 20}
	writeSelfBudgetMetrics(b, usages, budget, selfbudget.Process().Snapshot())
}

// writeSelfBudgetMetrics writes the usage (known values only), the
// budgets of enabled resources, an exceeded flag per database and known
// resource, and each loop's busy time and runs.
func writeSelfBudgetMetrics(b *strings.Builder, usages map[string]selfbudget.Usage,
	budget selfbudget.Budget, loops map[string]selfbudget.LoopStat) {
	dbs := sortedKeys(usages)
	var body strings.Builder
	writeSelfCPU(&body, dbs, usages)
	writeSelfDBRates(&body, dbs, usages)
	writeSelfBudgets(&body, dbs, usages, budget)
	writeSelfLoops(&body, loops)
	if body.Len() == 0 {
		return
	}
	b.WriteString(body.String())
	b.WriteString("\n")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeSelfCPU exports the sidecar process's CPU per cycle once: it is a
// process figure, the same for every database.
func writeSelfCPU(b *strings.Builder, dbs []string, usages map[string]selfbudget.Usage) {
	for _, db := range dbs {
		if u := usages[db]; u.CPUKnown {
			fmt.Fprintf(b, "# HELP pg_sage_self_cpu_ms_per_cycle CPU time of the pg_sage "+
				"process per collector cycle (ms)\n# TYPE pg_sage_self_cpu_ms_per_cycle gauge\n"+
				"pg_sage_self_cpu_ms_per_cycle %s\n", formatGauge(u.CPUMsPerCycle))
			return
		}
	}
}

func writeSelfDBRates(b *strings.Builder, dbs []string, usages map[string]selfbudget.Usage) {
	for _, g := range []struct {
		name, help string
		value      func(selfbudget.Usage) float64
	}{
		{"pg_sage_self_db_time_ms_per_hour", "Database time of pg_sage's own statements " +
			"per hour (ms)", func(u selfbudget.Usage) float64 { return u.DBTimeMsPerHour }},
		{"pg_sage_self_blocks_per_hour", "Shared blocks hit or read by pg_sage's own " +
			"statements per hour", func(u selfbudget.Usage) float64 { return u.BlocksPerHour }},
	} {
		header := false
		for _, db := range dbs {
			u := usages[db]
			if !u.DBKnown {
				continue
			}
			if !header {
				fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
				header = true
			}
			fmt.Fprintf(b, "%s{database=%q} %s\n", g.name, db, formatGauge(g.value(u)))
		}
	}
}

// selfResource is one budgeted resource as /metrics sees it.
type selfResource struct {
	name  selfbudget.Resource
	limit float64
	used  func(selfbudget.Usage) (float64, bool)
}

func selfResources(budget selfbudget.Budget) []selfResource {
	return []selfResource{
		{selfbudget.ResourceCPU, budget.CPUMsPerCycle,
			func(u selfbudget.Usage) (float64, bool) { return u.CPUMsPerCycle, u.CPUKnown }},
		{selfbudget.ResourceDBTime, budget.DBTimeMsPerHour,
			func(u selfbudget.Usage) (float64, bool) { return u.DBTimeMsPerHour, u.DBKnown }},
		{selfbudget.ResourceIO, budget.BlocksPerHour,
			func(u selfbudget.Usage) (float64, bool) { return u.BlocksPerHour, u.DBKnown }},
		{selfbudget.ResourceStorage, float64(budget.StorageBytes),
			func(u selfbudget.Usage) (float64, bool) {
				return float64(u.StorageBytes), u.StorageKnown
			}},
	}
}

func writeSelfBudgets(b *strings.Builder, dbs []string, usages map[string]selfbudget.Usage,
	budget selfbudget.Budget) {
	var limits, exceeded strings.Builder
	for _, r := range selfResources(budget) {
		if r.limit <= 0 {
			continue
		}
		fmt.Fprintf(&limits, "pg_sage_self_budget{resource=%q} %s\n", r.name,
			formatGauge(r.limit))
		for _, db := range dbs {
			if used, ok := r.used(usages[db]); ok {
				flag := 0
				if used > r.limit {
					flag = 1
				}
				fmt.Fprintf(&exceeded, "pg_sage_self_budget_exceeded{database=%q,"+
					"resource=%q} %d\n", db, r.name, flag)
			}
		}
	}
	if exceeded.Len() == 0 {
		return // nothing measured: no budget series without usage
	}
	b.WriteString("# HELP pg_sage_self_budget pg_sage's declared budget for itself " +
		"(self_budget)\n# TYPE pg_sage_self_budget gauge\n" + limits.String())
	b.WriteString("# HELP pg_sage_self_budget_exceeded 1 when pg_sage used more than its " +
		"budget of the resource\n# TYPE pg_sage_self_budget_exceeded gauge\n" +
		exceeded.String())
}

func writeSelfLoops(b *strings.Builder, loops map[string]selfbudget.LoopStat) {
	if len(loops) == 0 {
		return
	}
	names := sortedKeys(loops)
	b.WriteString("# HELP pg_sage_self_loop_busy_seconds_total Wall time pg_sage's " +
		"periodic loops spent working\n# TYPE pg_sage_self_loop_busy_seconds_total counter\n")
	for _, n := range names {
		fmt.Fprintf(b, "pg_sage_self_loop_busy_seconds_total{loop=%q} %s\n", n,
			formatGauge(loops[n].Busy.Seconds()))
	}
	b.WriteString("# HELP pg_sage_self_loop_runs_total Runs of pg_sage's periodic loops\n" +
		"# TYPE pg_sage_self_loop_runs_total counter\n")
	for _, n := range names {
		fmt.Fprintf(b, "pg_sage_self_loop_runs_total{loop=%q} %d\n", n, loops[n].Runs)
	}
}
