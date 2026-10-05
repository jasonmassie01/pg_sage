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
// normalized to one collector cycle, keeps it for /metrics, and raises a
// finding above the budget. The first cycle after a start has no window
// and a failed reading proves nothing: both leave the category unknown,
// so an open finding stays open. A disabled budget (0) still measures and
// resolves the finding. The same reading feeds the declared self-budget
// (checkSelfBudget).
func (a *Analyzer) checkSelfCost(ctx context.Context) []Finding {
	if a.pool == nil {
		a.evalFail(categorySelfCost) // no database to measure: unknown
		a.evalFail(categorySelfBudget)
		return nil
	}
	reading, err := selfcost.Read(ctx, a.pool)
	if err != nil {
		a.evalFail(categorySelfCost)
		a.evalFail(categorySelfBudget)
		a.logFn("WARN", "analyzer: measure pg_sage's own cost: %v", err)
		return nil
	}
	cost := a.selfCost.Observe(reading, a.collectorInterval())
	return append(a.checkSelfCostBudget(cost), a.checkSelfBudget(cost)...)
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
	return ruleSelfCost(cost, budget)
}

// SelfCost is the latest measured cost of pg_sage on this database.
func (a *Analyzer) SelfCost() selfcost.Cost {
	return a.selfCost.Last()
}

// collectorInterval is the cycle the cost is normalized to.
func (a *Analyzer) collectorInterval() time.Duration {
	secs := a.cfg.Collector.IntervalSeconds
	if secs <= 0 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

// ruleSelfCost raises a warning when the known DB time per collector
// cycle exceeds budgetMs. The finding is about the database's bill, not a
// sage object, so it names neither pg_sage nor a sage table (the
// self-monitoring filter would drop it).
func ruleSelfCost(c selfcost.Cost, budgetMs int) []Finding {
	if !selfcost.OverBudget(c, budgetMs) {
		return nil
	}
	return []Finding{{
		Category:         categorySelfCost,
		Severity:         "warning",
		ObjectType:       "database",
		ObjectIdentifier: c.Database,
		Title: fmt.Sprintf("Sage monitoring used %.0f ms of database time per collector "+
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
		Recommendation: "Its statements are the pg_stat_statements entries that contain " +
			"the /* pg_sage */ tag; sort them by total_exec_time to see which " +
			"component costs most. Lengthen collector.interval_seconds or " +
			"analyzer.interval_seconds to spend less, or raise " +
			"analyzer.self_cost_budget_ms if this cost is expected.",
	}}
}
