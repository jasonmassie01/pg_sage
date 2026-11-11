package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// writeSelfCostFromFleet exports what pg_sage itself costs each database
// (perf v1.8.3) and its database time by component (footprint-08), as
// measured by each instance's analyzer.
func writeSelfCostFromFleet(b *strings.Builder, mgr *fleet.DatabaseManager) {
	if mgr == nil || cfg == nil {
		return
	}
	costs := map[string]selfcost.Cost{}
	comps := map[string][]selfcost.ComponentCost{}
	for name, inst := range mgr.Instances() {
		if inst != nil && inst.Analyzer != nil {
			fp := inst.Analyzer.SelfFootprint()
			costs[name], comps[name] = fp.Cost, fp.ByComponent
		}
	}
	writeSelfCostMetrics(b, costs, cfg.Analyzer.SelfCostBudgetMs)
	writeSelfComponentMetrics(b, comps)
}

// selfCostGauge is one per-database self-cost series.
type selfCostGauge struct {
	name  string
	value func(selfcost.Cost) (float64, bool)
}

var selfCostGauges = []selfCostGauge{
	{"pg_sage_self_db_time_ms_per_cycle", dbTimeRate(
		func(c selfcost.Cost) float64 { return c.DBTimeMsPerCycle })},
	{"pg_sage_self_statements_per_cycle",
		dbTimeRate(func(c selfcost.Cost) float64 { return c.CallsPerCycle })},
	{"pg_sage_self_blocks_per_cycle", dbTimeRate(
		func(c selfcost.Cost) float64 { return c.BlocksPerCycle })},
	{"pg_sage_self_rows_read_per_cycle",
		tableRate(func(c selfcost.Cost) float64 { return c.RowsReadPerCycle })},
	{"pg_sage_self_rows_written_per_cycle", tableRate(
		func(c selfcost.Cost) float64 { return c.RowsWrittenPerCycle })},
	{"pg_sage_self_schema_bytes",
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
		metricsreg.Header(b, g.name)
		for _, db := range dbs {
			if v, ok := g.value(costs[db]); ok {
				writeSample(b, g.name, dbLabel(db), formatGauge(v))
			}
		}
	}
	if budgetMs > 0 {
		metricsreg.Header(b, "pg_sage_self_db_time_budget_ms")
		fmt.Fprintf(b, "pg_sage_self_db_time_budget_ms %d\n", budgetMs)
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
