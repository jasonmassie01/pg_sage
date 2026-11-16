package main

import (
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeOutcomesMetrics renders pg_sage_outcomes_measured_ratio (ux-02a):
// the share of the default window's changes with a measured effect, per
// database, from the last complete ledger read of that window.
func writeOutcomesMetrics(b *strings.Builder, ratios map[string]float64) {
	const name = "pg_sage_outcomes_measured_ratio"
	metricsreg.Header(b, name)
	names := make([]string, 0, len(ratios))
	for db := range ratios {
		names = append(names, db)
	}
	sort.Strings(names)
	for _, db := range names {
		writeSample(b, name, []labelPair{{"database", db}}, ratios[db])
	}
	b.WriteString("\n")
}
