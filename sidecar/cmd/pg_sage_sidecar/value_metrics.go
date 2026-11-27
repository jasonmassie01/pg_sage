package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeValueMetrics emits the DBA-hours-saved value series for every
// monitored database (D3) from its last metrics refresh. The refresh reads
// the same ledgers as /api/v1/value, so only verified-successful actions
// are counted and reverted actions contribute nothing. Every series
// carries the fleet instance name as its database label.
//
// Net verified toil can decrease when a rollback zeroes an action's
// credit, so it is a gauge (SURF-18); a counter would make rate()
// report a reset spike. Incidents avoided decrease the same way when a
// rollback voids an incident credit (earned-09), so they are a gauge too;
// the name keeps its _total suffix so existing queries keep working.
// pg_sage_value_metrics_up{database} reports whether that database's
// last refresh read its ledger, so a partial scrape is visible instead
// of silently missing a database. A database not yet refreshed has no
// sample at all.
func writeValueMetrics(b *strings.Builder, views []metricsView) {
	rows := valueRows(views)
	metricsreg.Header(b, "pg_sage_toil_minutes_saved")
	for _, v := range rows {
		writeToilSeries(b, v)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_incidents_avoided_total")
	for _, v := range rows {
		writeIncidentSeries(b, v)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_value_metrics_up")
	for _, v := range rows {
		up := 0
		if v.ok(metricsSourceValue) {
			up = 1
		}
		writeSample(b, "pg_sage_value_metrics_up", dbLabel(v.name), up)
	}
	b.WriteString("\n")
}

// valueRows are the refreshed databases with a value ledger, in name
// order. Two names for one physical database keep only the first, as
// /api/v1/value counts it once.
func valueRows(views []metricsView) []metricsView {
	seen := map[string]bool{}
	rows := make([]metricsView, 0, len(views))
	for _, v := range views {
		if !v.has(metricsSourceValue) || v.snap == nil {
			continue
		}
		if v.ok(metricsSourceValue) {
			if seen[v.snap.value.Identity] {
				continue
			}
			seen[v.snap.value.Identity] = true
		}
		rows = append(rows, v)
	}
	return rows
}

func writeToilSeries(b *strings.Builder, v metricsView) {
	if !v.ok(metricsSourceValue) {
		return
	}
	toil := v.snap.value.ToilMinutesByFeature
	for _, feature := range sortedKeys(toil) {
		writeSample(b, "pg_sage_toil_minutes_saved",
			[]labelPair{{"database", v.name}, {"feature", feature}}, toil[feature])
	}
}

func writeIncidentSeries(b *strings.Builder, v metricsView) {
	if !v.ok(metricsSourceValue) {
		return
	}
	counts := v.snap.value.IncidentsByKind
	for _, kind := range sortedKeys(counts) {
		writeSample(b, "pg_sage_incidents_avoided_total",
			[]labelPair{{"database", v.name}, {"kind", kind}}, counts[kind])
	}
}
