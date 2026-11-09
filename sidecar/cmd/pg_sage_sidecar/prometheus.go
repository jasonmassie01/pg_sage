package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/shadow"
)

// handleMetrics renders in-memory state and the refreshers' snapshots;
// it performs no database I/O (observability-03). Each database's
// snapshot is loaded once, so a scrape sees one consistent refresh.
func handleMetrics(w http.ResponseWriter, _ *http.Request) {
	views := processMetrics.views()
	var b strings.Builder

	// Info metric.
	b.WriteString("# HELP pg_sage_info pg_sage version\n" +
		"# TYPE pg_sage_info gauge\n")
	writeSample(&b, "pg_sage_info", []labelPair{{"version", version}, {"mode", cfg.Mode}}, 1)
	b.WriteString("\n")
	writeBuildMetrics(&b)

	writeModeMetric(&b, cfg.Mode)
	writeMetricsListenerMetric(&b)
	writeLoginLimiterMetric(&b, api.LoginLimiterSaturatedTotal())
	writeFirstLookMetrics(&b, firstLookTracker.Snapshot())

	writeConnectionMetric(&b, views)

	// Every family renders whenever its data exists, in every mode
	// (observability-02); per-database health comes from the manager.
	writeProcessMetrics(&b, views)
	writeDatabaseHealthMetrics(&b, fleetMgr)
	if fleetMgr != nil {
		writeFleetMetrics(&b)
	}
	writeFleetBudgetMetrics(&b, fleetLLMBudget)

	// pg_stat metrics of the process database (only when it exists).
	if pool != nil {
		writeDatabaseMetrics(&b, views)
	}
	// Value lives in each monitored database (D3), so every database
	// runtime refreshes its own ledger, never the meta pool's.
	// A database that failed to connect has no runtime; it still shows.
	monitored := withUnrefreshedInstances(views, fleetMgr)
	writeValueMetrics(&b, monitored)
	writeSLOMetrics(&b, views)
	writeMetricsRefreshSeries(&b, monitored)
	writeSelfCostFromFleet(&b, fleetMgr)
	writeSelfBudgetFromFleet(&b, fleetMgr)
	writeOptimizerMemoryFromFleet(&b, fleetMgr)
	writeShadowMetrics(&b, shadow.DecisionCounts(), shadow.ScoreCounts())
	writeParkMetrics(&b, executor.ParkCounts(), executor.WaitReleaseCounts())
	writeRecordFailureMetrics(&b, executor.RecordFailureCounts())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := fmt.Fprint(w, b.String()); err != nil {
		logWarn("prometheus", "write /metrics response: %v", err)
	}
}

// Help text of the families observability-09 removes in v2.7.0; each
// names the family that replaces it.
const (
	helpFindingsTotal = "# HELP pg_sage_findings_total Open findings by severity " +
		"(deprecated, removed in v2.7.0: use pg_sage_findings_open)\n"
	helpCollectorLastRun = "# HELP pg_sage_collector_last_run_timestamp Last " +
		"collector run (deprecated, removed in v2.7.0: use " +
		"pg_sage_collector_last_success_timestamp_seconds)\n"
	helpConnectionsTotal = "# HELP pg_sage_connections_total Connections by state on " +
		"the control database (deprecated, removed in v2.7.0: use postgres_exporter)\n"
	helpCacheHitRatio = "# HELP pg_sage_cache_hit_ratio Buffer cache hit ratio of " +
		"the control database (deprecated, removed in v2.7.0: use postgres_exporter)\n"
	helpFleetInstanceFindings = "# HELP pg_sage_fleet_instance_findings Per-instance " +
		"open findings (deprecated, removed in v2.7.0: use pg_sage_findings_open)\n"
	helpFleetInstanceHealth = "# HELP pg_sage_fleet_instance_health Per-instance " +
		"health score (deprecated, removed in v2.7.0: use pg_sage_database_health_score)\n"
)

// writeProcessMetrics renders the process-wide families: the standalone
// analyzer and collector (no samples in other modes), the shared LLM
// client and the optimizer.
func writeProcessMetrics(b *strings.Builder, views []metricsView) {
	// Findings from sage.findings table.
	b.WriteString(helpFindingsTotal + "# TYPE pg_sage_findings_total gauge\n")
	if anal != nil {
		counts := anal.OpenFindingsCount()
		for _, sev := range []string{"critical", "warning", "info"} {
			writeSample(b, "pg_sage_findings_total", []labelPair{{"severity", sev}},
				counts[sev])
		}
	}
	b.WriteString("\n")

	// Collector metrics.
	if coll != nil {
		snap := coll.LatestSnapshot()
		if snap != nil {
			b.WriteString(helpCollectorLastRun +
				"# TYPE pg_sage_collector_last_run_timestamp gauge\n")
			fmt.Fprintf(b, "pg_sage_collector_last_run_timestamp %d\n\n", snap.CollectedAt.Unix())
		}
	}

	// LLM metrics.
	if llmClient != nil {
		writeLLMMetrics(b)
	}

	// Optimizer metrics from sage.findings, as last refreshed.
	writeOptimizerMetrics(b, views)
}

// writeLLMMetrics reports the general LLM client's state and token use.
func writeLLMMetrics(b *strings.Builder) {
	b.WriteString("# HELP pg_sage_llm_enabled LLM integration enabled\n" +
		"# TYPE pg_sage_llm_enabled gauge\n")
	enabled := 0
	if llmClient.IsEnabled() {
		enabled = 1
	}
	fmt.Fprintf(b, "pg_sage_llm_enabled %d\n\n", enabled)

	b.WriteString("# HELP pg_sage_llm_circuit_open LLM circuit breaker (0=closed, 1=open)\n" +
		"# TYPE pg_sage_llm_circuit_open gauge\n")
	circuitVal := 0
	if llmClient.IsCircuitOpen() {
		circuitVal = 1
	}
	fmt.Fprintf(b, "pg_sage_llm_circuit_open %d\n\n", circuitVal)

	b.WriteString("# HELP pg_sage_llm_tokens_used_today Tokens consumed today\n" +
		"# TYPE pg_sage_llm_tokens_used_today gauge\n")
	fmt.Fprintf(b, "pg_sage_llm_tokens_used_today %d\n\n", llmClient.TokensUsedToday())

	b.WriteString("# HELP pg_sage_llm_tokens_budget_daily Daily token budget\n" +
		"# TYPE pg_sage_llm_tokens_budget_daily gauge\n")
	fmt.Fprintf(b, "pg_sage_llm_tokens_budget_daily %d\n\n", cfg.LLM.TokenBudgetDaily)
}

func writeOptimizerMetrics(b *strings.Builder, views []metricsView) {
	b.WriteString("# HELP pg_sage_optimizer_recommendations_total Index recommendations by category\n")
	b.WriteString("# TYPE pg_sage_optimizer_recommendations_total gauge\n")
	if v, found := sourceView(views, metricsSourceOptimizer); found &&
		v.ok(metricsSourceOptimizer) {
		for _, cat := range sortedKeys(v.snap.optimizer) {
			writeSample(b, "pg_sage_optimizer_recommendations_total",
				[]labelPair{{"category", cat}}, v.snap.optimizer[cat])
		}
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_optimizer_enabled Optimizer v2 enabled " +
		"with a configured LLM\n")
	b.WriteString("# TYPE pg_sage_optimizer_enabled gauge\n")
	fmt.Fprintf(b, "pg_sage_optimizer_enabled %d\n\n", optimizerGaugeValue())
}

func writeFleetMetrics(b *strings.Builder) {
	status := fleetMgr.FleetStatus()
	// FleetStatus lists databases in map order; the exposition must not
	// reorder series between scrapes (observability-07).
	sort.Slice(status.Databases, func(i, j int) bool {
		return status.Databases[i].Name < status.Databases[j].Name
	})
	b.WriteString("# HELP pg_sage_fleet_databases Total fleet databases\n" +
		"# TYPE pg_sage_fleet_databases gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_databases %d\n\n",
		status.Summary.TotalDatabases)

	b.WriteString("# HELP pg_sage_fleet_healthy Healthy databases\n" +
		"# TYPE pg_sage_fleet_healthy gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_healthy %d\n\n",
		status.Summary.Healthy)

	b.WriteString("# HELP pg_sage_fleet_findings_total Total open findings\n" +
		"# TYPE pg_sage_fleet_findings_total gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_findings_total %d\n\n",
		status.Summary.TotalFindings)

	b.WriteString("# HELP pg_sage_fleet_findings_critical Total critical findings\n" +
		"# TYPE pg_sage_fleet_findings_critical gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_findings_critical %d\n\n",
		status.Summary.TotalCritical)

	b.WriteString(helpFleetInstanceFindings +
		"# TYPE pg_sage_fleet_instance_findings gauge\n")
	for _, db := range status.Databases {
		writeSample(b, "pg_sage_fleet_instance_findings",
			[]labelPair{{"database", db.Name}}, db.Status.FindingsOpen)
	}
	b.WriteString("\n")

	b.WriteString(helpFleetInstanceHealth +
		"# TYPE pg_sage_fleet_instance_health gauge\n")
	for _, db := range status.Databases {
		writeSample(b, "pg_sage_fleet_instance_health",
			[]labelPair{{"database", db.Name}}, db.Status.HealthScore)
	}
	b.WriteString("\n")
}

// writeDatabaseMetrics renders the process database's last pg_stat
// refresh (the monitored database, or the metadata database in meta mode);
// a failed refresh omits them. The size is per database, with the health
// families (database_health_metrics.go).
func writeDatabaseMetrics(b *strings.Builder, views []metricsView) {
	v, found := pgStatView(views)
	if !found || !v.ok(metricsSourcePGStat) {
		return
	}
	stat := v.snap.pgstat
	b.WriteString(helpConnectionsTotal + "# TYPE pg_sage_connections_total gauge\n")
	for _, state := range sortedKeys(stat.connectionsByState) {
		writeSample(b, "pg_sage_connections_total", []labelPair{{"state", state}},
			stat.connectionsByState[state])
	}
	b.WriteString("\n")

	if stat.blksHit+stat.blksRead > 0 {
		ratio := float64(stat.blksHit) / float64(stat.blksHit+stat.blksRead)
		b.WriteString(helpCacheHitRatio + "# TYPE pg_sage_cache_hit_ratio gauge\n")
		fmt.Fprintf(b, "pg_sage_cache_hit_ratio %g\n\n", ratio)
	}
}
