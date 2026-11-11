package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// selfComponentMetric is pg_sage's database time per hour by component
// (footprint-08). It is a family of its own: the per-database total
// pg_sage_self_db_time_ms_per_hour{database} keeps its meaning, so a
// sum by (database) over it never counts a component twice.
const selfComponentMetric = "pg_sage_self_component_db_time_ms_per_hour"

// writeSelfComponentMetrics writes each database's latest per-component
// database time (sorted databases, MetricComponents order), components
// folded into the fixed set; a database with no window yet is left out.
func writeSelfComponentMetrics(b *strings.Builder, comps map[string][]selfcost.ComponentCost) {
	var body strings.Builder
	for _, db := range sortedKeys(comps) {
		if len(comps[db]) == 0 {
			continue
		}
		sums := map[string]float64{}
		for _, c := range comps[db] {
			sums[selfcost.MetricComponent(c.Component)] += c.DBTimeMsPerHour
		}
		for _, name := range selfcost.MetricComponents {
			if v, ok := sums[name]; ok {
				writeSample(&body, selfComponentMetric,
					[]labelPair{{"database", db}, {"component", name}}, formatGauge(v))
			}
		}
	}
	if body.Len() == 0 {
		return
	}
	metricsreg.Header(b, selfComponentMetric)
	b.WriteString(body.String())
	b.WriteString("\n")
}
