package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// SelfFootprint is what pg_sage measured about its own cost on this
// database at its latest analyzer cycle (footprint-08): the cost window,
// the self-budget usage, the costliest statements, the cost per
// component and the busiest loops. Every figure keeps its Known flags;
// the slices are nil before a usable window.
type SelfFootprint struct {
	Cost        selfcost.Cost
	Usage       selfbudget.Usage
	Top         []selfcost.StatementCost
	ByComponent []selfcost.ComponentCost
	Loops       []selfbudget.LoopCost
}

// SelfFootprint is a copy of the latest footprint; safe to call while
// the analyzer runs (the API reads it). A nil analyzer has none.
func (a *Analyzer) SelfFootprint() SelfFootprint {
	if a == nil {
		return SelfFootprint{}
	}
	usage, loops := a.selfBudget.lastWithLoops()
	cost, top, comps := a.selfCost.Window()
	return SelfFootprint{Cost: cost, Usage: usage, Top: top, ByComponent: comps, Loops: loops}
}

// WithSelfCostMeter makes the analyzer meter its own cost into m (shared
// with whoever reads it); nil keeps the analyzer's own meter. Call it
// before the analyzer starts.
func (a *Analyzer) WithSelfCostMeter(m *selfcost.Meter) {
	if m != nil {
		a.selfCost = m
	}
}

// selfCostAdvice is the sage_self_cost recommendation: the component
// that cost most this window (when the breakdown names one) and where the
// full breakdown is.
func selfCostAdvice(database string, comps []selfcost.ComponentCost) string {
	where := fmt.Sprintf("Settings > General > Footprint and GET /api/v1/self-cost?database=%s "+
		"break the cost down by component. ", database)
	return topComponentLine(comps) + where + "Lengthen collector.interval_seconds or " +
		"analyzer.interval_seconds to spend less, or raise " +
		"analyzer.self_cost_budget_ms if this cost is expected."
}

// topComponentLine names the costliest component and its share of the
// window's database time ("" without a breakdown). Unattributed time
// comes from statements whose pg_stat_statements text predates the
// component tags; it is named as such.
func topComponentLine(comps []selfcost.ComponentCost) string {
	top, total := topComponent(comps)
	if top == nil {
		return ""
	}
	share := 100 * top.DBTimeMsPerHour / total
	if top.Component == selfcost.Unattributed {
		return fmt.Sprintf("%.0f%% of the database time is unattributed (statements "+
			"pg_stat_statements first saw before their tag named a component). ", share)
	}
	line := fmt.Sprintf("The costliest component was %s (%.0f ms per hour, %.0f%% of "+
		"pg_sage's database time). ", top.Component, top.DBTimeMsPerHour, share)
	for _, c := range comps {
		if c.Component == selfcost.Unattributed && c.DBTimeMsPerHour > 0 {
			line += fmt.Sprintf("%.0f%% is unattributed. ", 100*c.DBTimeMsPerHour/total)
		}
	}
	return line
}

// topComponent is the component with the most database time and the
// total (nil when no component used any).
func topComponent(comps []selfcost.ComponentCost) (*selfcost.ComponentCost, float64) {
	var top *selfcost.ComponentCost
	total := 0.0
	for i := range comps {
		total += comps[i].DBTimeMsPerHour
		if comps[i].DBTimeMsPerHour > 0 &&
			(top == nil || comps[i].DBTimeMsPerHour > top.DBTimeMsPerHour) {
			top = &comps[i]
		}
	}
	if total <= 0 {
		return nil, 0
	}
	return top, total
}
