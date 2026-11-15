package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// collectorStatuses is every collector's health by database: each fleet
// instance that has a collector (standalone registers its one runtime
// there too), plus the standalone collector when no instance holds it.
func collectorStatuses(
	mgr *fleet.DatabaseManager, standalone *collector.Collector, standaloneName string,
) map[string]collector.Status {
	out := map[string]collector.Status{}
	seen := false
	if mgr != nil {
		for name, inst := range mgr.Instances() {
			if inst == nil || inst.Collector == nil {
				continue
			}
			seen = seen || inst.Collector == standalone
			out[name] = inst.Collector.Status()
		}
	}
	if standalone != nil && !seen {
		out[standaloneName] = standalone.Status()
	}
	return out
}

// writeCollectorMetrics writes each database's collector health. Every
// series carries the database label, in every mode.
func writeCollectorMetrics(b *strings.Builder, statuses map[string]collector.Status) {
	dbs := sortedKeys(statuses)
	metricsreg.Header(b, "pg_sage_collector_breaker_state")
	for _, db := range dbs {
		fmt.Fprintf(b, "pg_sage_collector_breaker_state{database=%q} %d\n", db,
			statuses[db].BreakerState.Code())
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_collector_breaker_skips_total")
	for _, db := range dbs {
		for _, reason := range []string{collector.SkipReasonLoad,
			collector.SkipReasonLoadQueryError} {
			fmt.Fprintf(b, "pg_sage_collector_breaker_skips_total{database=%q,reason=%q} %d\n",
				db, reason, statuses[db].SkipsTotal[reason])
		}
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_collector_last_success_timestamp")
	for _, db := range dbs {
		if at := statuses[db].LastSuccess; !at.IsZero() {
			fmt.Fprintf(b, "pg_sage_collector_last_success_timestamp{database=%q} %d\n",
				db, at.Unix())
		}
	}
	writeCollectorUnavailable(b, dbs, statuses)
	writeCollectorFailureCounter(b, dbs, "pg_sage_collector_persist_failures_total",
		statuses,
		func(s collector.Status) int64 { return s.PersistFailures })
	writeCollectorFailureCounter(b, dbs, "pg_sage_collector_query_store_failures_total",
		statuses,
		func(s collector.Status) int64 { return s.QueryStoreFailures })
	writeQueryRollupMetrics(b, querystore.RollupHourCounts(), querystore.RollupLags())
	writeWorkloadPackMetrics(b, workpack.BuildCounts(), workpack.PackBytes())
}

// writeCollectorUnavailable writes 1 per category the latest snapshot
// could not read (unknown, not empty).
func writeCollectorUnavailable(b *strings.Builder, dbs []string,
	statuses map[string]collector.Status) {
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_collector_category_unavailable")
	for _, db := range dbs {
		for _, category := range sortedKeys(statuses[db].Unavailable) {
			fmt.Fprintf(b,
				"pg_sage_collector_category_unavailable{database=%q,category=%q} 1\n",
				db, category)
		}
	}
}

func writeCollectorFailureCounter(b *strings.Builder, dbs []string, name string,
	statuses map[string]collector.Status, value func(collector.Status) int64) {
	b.WriteString("\n")
	metricsreg.Header(b, name)
	for _, db := range dbs {
		fmt.Fprintf(b, "%s{database=%q} %d\n", name, db, value(statuses[db]))
	}
	b.WriteString("\n")
}
