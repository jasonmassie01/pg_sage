package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// writeSelfBudgetFromFleet exports pg_sage's declared self-budget and its
// usage (roadmap phase 3): each database's as its analyzer measured it,
// the process CPU as the process watch did (footprint-07).
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
	budget, inheritedMs := selfBudgetFromConfig(cfg)
	writeSelfBudgetMetrics(b, usages, selfbudget.ProcessWatch().Reading(), budget,
		inheritedMs, selfbudget.Process().Snapshot())
}

// selfBudgetFromConfig is the declared budget at the configured collector
// interval and, when self_budget does not set database time, the per-cycle
// analyzer.self_cost_budget_ms each database inherits (0 otherwise): a
// database then has that budget at its own collector's period.
func selfBudgetFromConfig(c *config.Config) (selfbudget.Budget, int) {
	dbPerHour, explicit := c.EffectiveSelfDBTimeMsPerHour()
	budget := selfbudget.Budget{CPUMsPerCycle: float64(c.SelfBudget.CPUMsPerCycle),
		DBTimeMsPerHour: dbPerHour, BlocksPerHour: float64(c.SelfBudget.BlocksPerHour),
		StorageBytes: int64(c.SelfBudget.StorageMB) << 20}
	inheritedMs := 0
	if !explicit && c.Analyzer.SelfCostBudgetMs > 0 {
		inheritedMs = c.Analyzer.SelfCostBudgetMs
	}
	return budget, inheritedMs
}

// writeSelfBudgetMetrics writes the usage (known values only), the
// budgets of enabled resources, an exceeded flag per database and known
// resource (once for the process CPU), and each loop's busy time and
// runs. inheritedMs (0: none) is the per-cycle database-time budget each
// database inherits at its own collector period.
func writeSelfBudgetMetrics(b *strings.Builder, usages map[string]selfbudget.Usage,
	proc selfbudget.CPUReading, budget selfbudget.Budget, inheritedMs int,
	loops map[string]selfbudget.LoopStat) {
	dbs := sortedKeys(usages)
	var body strings.Builder
	writeSelfCPU(&body, proc)
	writeSelfDBRates(&body, dbs, usages)
	writeSelfBudgets(&body, dbs, usages, proc, budget, inheritedMs)
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

// writeSelfCPU exports the sidecar process's CPU per cycle once, as the
// process watch measured it: a process figure for every database.
func writeSelfCPU(b *strings.Builder, proc selfbudget.CPUReading) {
	if !proc.Known {
		return
	}
	metricsreg.Header(b, "pg_sage_self_cpu_ms_per_cycle")
	fmt.Fprintf(b, "pg_sage_self_cpu_ms_per_cycle %s\n", formatGauge(proc.MsPerCycle))
}

func writeSelfDBRates(b *strings.Builder, dbs []string, usages map[string]selfbudget.Usage) {
	for _, g := range []struct {
		name  string
		value func(selfbudget.Usage) float64
	}{
		{"pg_sage_self_db_time_ms_per_hour",
			func(u selfbudget.Usage) float64 { return u.DBTimeMsPerHour }},
		{"pg_sage_self_blocks_per_hour",
			func(u selfbudget.Usage) float64 { return u.BlocksPerHour }},
	} {
		header := false
		for _, db := range dbs {
			u := usages[db]
			if !u.DBKnown {
				continue
			}
			if !header {
				metricsreg.Header(b, g.name)
				header = true
			}
			writeSample(b, g.name, dbLabel(db), formatGauge(g.value(u)))
		}
	}
}

// selfResource is one budgeted resource as /metrics sees it.
type selfResource struct {
	name  selfbudget.Resource
	limit float64
	used  func(selfbudget.Usage) (float64, bool)
}

// selfResources are the per-database resources; the process CPU is
// flagged once (addProcessCPU).
func selfResources(budget selfbudget.Budget) []selfResource {
	return []selfResource{
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

// dbLimit is the resource's limit on one database and whether it is
// inherited: database time inherited from analyzer.self_cost_budget_ms
// follows that database's collector period (0 while it is unknown).
func (r selfResource) dbLimit(u selfbudget.Usage, inheritedMs int) (float64, bool) {
	if r.name != selfbudget.ResourceDBTime || inheritedMs <= 0 {
		return r.limit, false
	}
	period := time.Duration(u.CycleSeconds * float64(time.Second))
	return config.InheritedDBTimeMsPerHour(inheritedMs, period), true
}

// selfBudgetSeries collects the budget series of every resource.
type selfBudgetSeries struct {
	limits, perDB, exceeded strings.Builder
}

// addDatabases writes r's inherited budget and exceeded flag per database.
func (s *selfBudgetSeries) addDatabases(r selfResource, dbs []string,
	usages map[string]selfbudget.Usage, inheritedMs int) {
	for _, db := range dbs {
		u := usages[db]
		limit, inherited := r.dbLimit(u, inheritedMs)
		if limit <= 0 {
			continue // period unknown: no inherited budget to compare with
		}
		if inherited {
			writeSample(&s.perDB, "pg_sage_self_budget_database",
				[]labelPair{{"database", db}, {"resource", string(r.name)}},
				formatGauge(limit))
		}
		if used, ok := r.used(u); ok {
			flag := 0
			if used > limit {
				flag = 1
			}
			writeSample(&s.exceeded, "pg_sage_self_budget_exceeded",
				[]labelPair{{"database", db}, {"resource", string(r.name)}}, flag)
		}
	}
}

// addProcessCPU writes the CPU budget and, once measured, the process's
// exceeded flag (no database label: the budget is the process's).
func (s *selfBudgetSeries) addProcessCPU(proc selfbudget.CPUReading, limit float64) {
	if limit <= 0 {
		return
	}
	cpu := []labelPair{{"resource", string(selfbudget.ResourceCPU)}}
	writeSample(&s.limits, "pg_sage_self_budget", cpu, formatGauge(limit))
	if !proc.Known {
		return
	}
	flag := 0
	if proc.MsPerCycle > limit {
		flag = 1
	}
	writeSample(&s.exceeded, "pg_sage_self_budget_exceeded", cpu, flag)
}

func writeSelfBudgets(b *strings.Builder, dbs []string, usages map[string]selfbudget.Usage,
	proc selfbudget.CPUReading, budget selfbudget.Budget, inheritedMs int) {
	var s selfBudgetSeries
	s.addProcessCPU(proc, budget.CPUMsPerCycle)
	for _, r := range selfResources(budget) {
		if r.limit <= 0 {
			continue
		}
		writeSample(&s.limits, "pg_sage_self_budget",
			[]labelPair{{"resource", string(r.name)}}, formatGauge(r.limit))
		s.addDatabases(r, dbs, usages, inheritedMs)
	}
	if s.exceeded.Len() == 0 {
		return // nothing measured: no budget series without usage
	}
	metricsreg.Header(b, "pg_sage_self_budget")
	b.WriteString(s.limits.String())
	if s.perDB.Len() > 0 {
		metricsreg.Header(b, "pg_sage_self_budget_database")
		b.WriteString(s.perDB.String())
	}
	metricsreg.Header(b, "pg_sage_self_budget_exceeded")
	b.WriteString(s.exceeded.String())
}

func writeSelfLoops(b *strings.Builder, loops map[string]selfbudget.LoopStat) {
	if len(loops) == 0 {
		return
	}
	names := sortedKeys(loops)
	metricsreg.Header(b, "pg_sage_self_loop_busy_seconds_total")
	for _, n := range names {
		writeSample(b, "pg_sage_self_loop_busy_seconds_total", loopLabels(n),
			formatGauge(loops[n].Busy.Seconds()))
	}
	metricsreg.Header(b, "pg_sage_self_loop_runs_total")
	for _, n := range names {
		writeSample(b, "pg_sage_self_loop_runs_total", loopLabels(n), loops[n].Runs)
	}
	writeSelfLoopHealth(b, names, loops)
}

// writeSelfLoopHealth exports each loop's failed runs and when its last
// successful run ended, omitted until it has one (observability-06), with
// the loop and database labels of the busy time (footprint-07).
func writeSelfLoopHealth(b *strings.Builder, names []string,
	loops map[string]selfbudget.LoopStat) {
	metricsreg.Header(b, "pg_sage_self_loop_errors_total")
	for _, n := range names {
		writeSample(b, "pg_sage_self_loop_errors_total", loopLabels(n), loops[n].Errors)
	}
	header := false
	for _, n := range names {
		at := loops[n].LastSuccess
		if at.IsZero() {
			continue
		}
		if !header {
			metricsreg.Header(b, "pg_sage_self_loop_last_success_timestamp_seconds")
			header = true
		}
		writeSample(b, "pg_sage_self_loop_last_success_timestamp_seconds",
			loopLabels(n), at.Unix())
	}
}

// loopLabels are a loop's labels: loop is the component (so sum by (loop)
// adds up every database) and database the database it worked for,
// absent on a process loop (footprint-07).
func loopLabels(name string) []labelPair {
	component, database := selfbudget.SplitLoopName(name)
	if database == "" {
		return []labelPair{{"loop", component}}
	}
	return []labelPair{{"loop", component}, {"database", database}}
}
