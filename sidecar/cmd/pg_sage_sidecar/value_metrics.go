package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/value"
)

// writeValueMetrics emits the DBA-hours-saved value series for every
// monitored database (D3). It reads the same ledgers as /api/v1/value, so
// only verified-successful actions are counted and reverted actions
// contribute nothing. Every series carries the fleet instance name as its
// database label.
//
// Net verified toil can decrease when a rollback zeroes an action's
// credit, so it is a gauge (SURF-18); a counter would make rate()
// report a reset spike. pg_sage_value_metrics_up{database} reports
// whether that database's ledger was read on this scrape, so a partial
// scrape is visible instead of silently missing a database.
func writeValueMetrics(
	b *strings.Builder, ctx context.Context, sources []value.Source,
) {
	reader := value.NewFleetService(func() []value.Source { return sources })
	results, err := reader.Read(ctx, value.Filter{})
	if err != nil {
		logWarn("metrics", "value metrics read failed: %v", err)
	}
	b.WriteString("# HELP pg_sage_toil_minutes_saved " +
		"Net verified DBA toil minutes saved (decreases on rollback)\n" +
		"# TYPE pg_sage_toil_minutes_saved gauge\n")
	for _, result := range results {
		writeToilSeries(b, result)
	}
	b.WriteString("\n# HELP pg_sage_incidents_avoided_total " +
		"Incidents avoided by kind\n" +
		"# TYPE pg_sage_incidents_avoided_total counter\n")
	for _, result := range results {
		writeIncidentSeries(b, result)
	}
	b.WriteString("\n# HELP pg_sage_value_metrics_up " +
		"1 if the database's value ledger was read on this scrape\n" +
		"# TYPE pg_sage_value_metrics_up gauge\n")
	for _, result := range results {
		up := 1
		if result.Err != nil {
			up = 0
			logWarn("metrics", "value metrics for database %q: %v",
				result.Name, result.Err)
		}
		fmt.Fprintf(b, "pg_sage_value_metrics_up{database=%q} %d\n", result.Name, up)
	}
	b.WriteString("\n")
}

func writeToilSeries(b *strings.Builder, result value.SourceResult) {
	if result.Err != nil {
		return
	}
	features := make([]string, 0, len(result.Snapshot.ByFeatureMinutes))
	for feature := range result.Snapshot.ByFeatureMinutes {
		features = append(features, feature)
	}
	sort.Strings(features)
	for _, feature := range features {
		fmt.Fprintf(b, "pg_sage_toil_minutes_saved{database=%q,feature=%q} %g\n",
			result.Name, feature, result.Snapshot.ByFeatureMinutes[feature])
	}
}

func writeIncidentSeries(b *strings.Builder, result value.SourceResult) {
	if result.Err != nil {
		return
	}
	counts := map[string]int{}
	for _, incident := range result.Snapshot.Incidents {
		counts[incident.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(b, "pg_sage_incidents_avoided_total{database=%q,kind=%q} %d\n",
			result.Name, kind, counts[kind])
	}
}
