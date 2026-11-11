package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// categorySelfCost is the finding raised when pg_sage's own statements
// use more database time per collector cycle than
// analyzer.self_cost_budget_ms (perf v1.8.3: pg_sage reports its own bill
// instead of hiding from pg_stat_statements).
const categorySelfCost = "sage_self_cost"

// checkSelfCost measures pg_sage's own cost once per analyzer cycle,
// normalized to one cycle of this database's collector (collectorPeriod),
// keeps it for /metrics, and raises a finding above the budget. Every
// read is bounded (the catalog reader) and the sage schema's reading is
// the cycle's one (sageSchemaReading). The first cycle after a start has
// no window, an unknown collector period has no cycle and a failed
// reading proves nothing: each leaves the category unknown, so an open
// finding stays open; failed statements alone leave the schema counters
// measured. A disabled budget (0) still measures and resolves the
// finding. The same reading feeds the declared self-budget
// (checkSelfBudget).
func (a *Analyzer) checkSelfCost(ctx context.Context) []Finding {
	if a.pool == nil {
		a.evalFail(categorySelfCost) // no database to measure: unknown
		a.evalFail(categorySelfBudget)
		return nil
	}
	sc, err := a.sageSchemaReading(ctx)
	if err != nil {
		a.evalFail(categorySelfCost)
		a.evalFail(categorySelfBudget)
		a.logFn("WARN", "analyzer: measure pg_sage's own cost: %v", err)
		return nil
	}
	reading := a.selfStatements.Sample(ctx, a.catalog(), sc)
	if reading.StatementsErr != nil {
		a.logFn("WARN", "analyzer: measure pg_sage's own statements: %v",
			reading.StatementsErr)
	}
	cost := a.selfCost.Observe(reading, a.collectorPeriod())
	return append(a.checkSelfCostBudget(cost), a.checkSelfBudget(ctx, cost)...)
}

// checkSelfCostBudget is analyzer.self_cost_budget_ms against the cost.
func (a *Analyzer) checkSelfCostBudget(cost selfcost.Cost) []Finding {
	budget := a.cfg.Analyzer.SelfCostBudgetMs
	if budget <= 0 {
		a.eval.evaluated(categorySelfCost)
		return nil
	}
	if !cost.Known || !cost.DBTimeKnown {
		a.evalFail(categorySelfCost)
		return nil
	}
	a.eval.evaluated(categorySelfCost)
	return ruleSelfCost(cost, budget, a.selfCost.ByComponent()...)
}

// SelfCost is the latest measured cost of pg_sage on this database.
func (a *Analyzer) SelfCost() selfcost.Cost {
	return a.selfCost.Last()
}

// collectorPeriod is the cycle the cost is normalized to: the period this
// database's collector runs at (in fleet mode its own, possibly derived,
// interval, not the global collector.interval_seconds). It is 0 (unknown)
// without a collector or before it starts; the cost is then unknown.
func (a *Analyzer) collectorPeriod() time.Duration {
	if a.collector == nil {
		return 0
	}
	return a.collector.Interval()
}

// ruleSelfCost raises a warning when the known DB time per collector
// cycle exceeds budgetMs; comps (the window's cost per component, may be
// empty) name the costliest component in the advice. The finding is
// about pg_sage's bill on the database, keyed by the database name; the
// self-monitoring filter exempts its category, so it may name pg_sage
// (and a database named sage).
func ruleSelfCost(c selfcost.Cost, budgetMs int, comps ...selfcost.ComponentCost) []Finding {
	if !selfcost.OverBudget(c, budgetMs) {
		return nil
	}
	f := Finding{
		Category:         categorySelfCost,
		Severity:         "warning",
		ObjectType:       "database",
		ObjectIdentifier: c.Database,
		Title: fmt.Sprintf("pg_sage monitoring used %.0f ms of database time per collector "+
			"cycle on %s (budget %d ms)", c.DBTimeMsPerCycle, c.Database, budgetMs),
		Detail: map[string]any{
			"db_time_ms_per_cycle":   c.DBTimeMsPerCycle,
			"budget_ms":              budgetMs,
			"cycle_seconds":          c.CycleSeconds,
			"window_seconds":         c.WindowSeconds,
			"calls_per_cycle":        c.CallsPerCycle,
			"blocks_per_cycle":       c.BlocksPerCycle,
			"rows_read_per_cycle":    c.RowsReadPerCycle,
			"rows_written_per_cycle": c.RowsWrittenPerCycle,
			"schema_bytes":           c.SchemaBytes,
		},
		Recommendation: selfCostAdvice(c.Database, comps),
	}
	if top, _ := topComponent(comps); top != nil {
		f.Detail["top_component"] = top.Component
		f.Detail["top_component_db_time_ms_per_hour"] = top.DBTimeMsPerHour
	}
	return []Finding{f}
}
