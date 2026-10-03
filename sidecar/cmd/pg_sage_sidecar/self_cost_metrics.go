package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// writeSelfCostFromFleet exports what pg_sage itself costs each database
// (perf v1.8.3), as measured by each instance's analyzer.
func writeSelfCostFromFleet(b *strings.Builder, mgr *fleet.DatabaseManager) {
	if mgr == nil || cfg == nil {
		return
	}
	costs := map[string]selfcost.Cost{}
	for name, inst := range mgr.Instances() {
		if inst != nil && inst.Analyzer != nil {
			costs[name] = inst.Analyzer.SelfCost()
		}
	}
	writeSelfCostMetrics(b, costs, cfg.Analyzer.SelfCostBudgetMs)
}

// selfCostGauge is one per-database self-cost series.
type selfCostGauge struct {
	name, help string
	value      func(selfcost.Cost) (float64, bool)
}

var selfCostGauges = []selfCostGauge{
	{"pg_sage_self_db_time_ms_per_cycle", "Database time of pg_sage's own statements " +
		"per collector cycle (ms, from pg_stat_statements)", dbTimeRate(
		func(c selfcost.Cost) float64 { return c.DBTimeMsPerCycle })},
	{"pg_sage_self_statements_per_cycle", "pg_sage statement executions per collector " +
		"cycle", dbTimeRate(func(c selfcost.Cost) float64 { return c.CallsPerCycle })},
	{"pg_sage_self_blocks_per_cycle", "Shared blocks hit or read by pg_sage's own " +
		"statements per collector cycle", dbTimeRate(
		func(c selfcost.Cost) float64 { return c.BlocksPerCycle })},
	{"pg_sage_self_rows_read_per_cycle", "Rows pg_sage read from its sage tables per " +
		"collector cycle", tableRate(func(c selfcost.Cost) float64 { return c.RowsReadPerCycle })},
	{"pg_sage_self_rows_written_per_cycle", "Rows pg_sage inserted, updated or deleted " +
		"in its sage tables per collector cycle", tableRate(
		func(c selfcost.Cost) float64 { return c.RowsWrittenPerCycle })},
	{"pg_sage_self_schema_bytes", "Size of the sage schema (tables, TOAST, indexes)",
		func(c selfcost.Cost) (float64, bool) {
			return float64(c.SchemaBytes), c.SchemaBytes > 0
		}},
}

func dbTimeRate(f func(selfcost.Cost) float64) func(selfcost.Cost) (float64, bool) {
	return func(c selfcost.Cost) (float64, bool) { return f(c), c.Known && c.DBTimeKnown }
}

func tableRate(f func(selfcost.Cost) float64) func(selfcost.Cost) (float64, bool) {
	return func(c selfcost.Cost) (float64, bool) { return f(c), c.Known }
}

// writeSelfCostMetrics writes the self-cost gauges per database (sorted)
// and the budget; values not measured yet are left out, not exported as 0.
func writeSelfCostMetrics(b *strings.Builder, costs map[string]selfcost.Cost, budgetMs int) {
	if len(costs) == 0 {
		return
	}
	dbs := make([]string, 0, len(costs))
	for db := range costs {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, g := range selfCostGauges {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, db := range dbs {
			if v, ok := g.value(costs[db]); ok {
				fmt.Fprintf(b, "%s{database=%q} %s\n", g.name, db, formatGauge(v))
			}
		}
	}
	if budgetMs > 0 {
		fmt.Fprintf(b, "# HELP pg_sage_self_db_time_budget_ms analyzer.self_cost_budget_ms\n"+
			"# TYPE pg_sage_self_db_time_budget_ms gauge\npg_sage_self_db_time_budget_ms %d\n",
			budgetMs)
	}
	b.WriteString("\n")
}

// formatGauge prints integral values without an exponent.
func formatGauge(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
