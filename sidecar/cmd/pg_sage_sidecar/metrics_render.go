package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// sourceView is the first database that refreshes source (standalone has
// one database with the optimizer and pg_stat sources).
func sourceView(views []metricsView, source string) (metricsView, bool) {
	for _, v := range views {
		if v.has(source) {
			return v, true
		}
	}
	return metricsView{}, false
}

// writeConnectionMetric reports whether the sidecar reaches its control
// database: the process pool's last health check when there is one (the
// monitored database in standalone, the metadata database in meta mode),
// otherwise whether any database's last metrics refresh read it. Nothing
// is sampled while neither has a result yet, so startup never reports a
// false 0. Per-database reachability is pg_sage_database_up.
func writeConnectionMetric(b *strings.Builder, views []metricsView) {
	metricsreg.Header(b, "pg_sage_connection_up")
	if up, known := connectionState(views); known {
		fmt.Fprintf(b, "pg_sage_connection_up %d\n", up)
	}
	b.WriteString("\n")
}

func connectionState(views []metricsView) (int, bool) {
	if pool != nil {
		return poolHealth.state()
	}
	refreshed := false
	for _, v := range views {
		if v.snap == nil {
			continue
		}
		refreshed = true
		if v.snap.reachable() {
			return 1, true
		}
	}
	if !refreshed && len(views) > 0 {
		return 0, false
	}
	return 0, true
}

// writeMetricsRefreshSeries reports, per database and source, whether the
// last refresh succeeded and when the source last refreshed successfully.
// Values are as old as time() - pg_sage_metrics_refresh_timestamp_seconds.
func writeMetricsRefreshSeries(b *strings.Builder, views []metricsView) {
	metricsreg.Header(b, "pg_sage_metrics_refresh_success")
	for _, v := range views {
		if v.snap == nil {
			continue
		}
		for _, source := range v.sources {
			ok := 0
			if v.snap.ok[source] {
				ok = 1
			}
			writeSample(b, "pg_sage_metrics_refresh_success",
				[]labelPair{{"database", v.name}, {"source", source}}, ok)
		}
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_metrics_refresh_timestamp_seconds")
	for _, v := range views {
		if v.snap == nil {
			continue
		}
		for _, source := range v.sources {
			at := v.snap.lastSuccess[source]
			if at.IsZero() {
				continue
			}
			writeSample(b, "pg_sage_metrics_refresh_timestamp_seconds",
				[]labelPair{{"database", v.name}, {"source", source}}, at.Unix())
		}
	}
	b.WriteString("\n")
}
