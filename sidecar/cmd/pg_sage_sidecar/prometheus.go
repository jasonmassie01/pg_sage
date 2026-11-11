package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/pgaudit"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/sre/probes"
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
	writeTLSMetrics(&b, apiTLS)
	writeSSODiscoveryMetrics(&b, api.CurrentSSODiscoveryStatus())
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
	writeCollectorMetrics(&b, collectorStatuses(fleetMgr, coll, resolveDBName()))
	writeLeaderMetrics(&b, fleetLeader, governanceElector.Load())

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
	writeExecutorMetrics(&b, views)
	writeLogwatchMetrics(&b, runtimeLogFanouts.droppedLines())
	writeSREProbeMetrics(&b, probes.QueueStats())
	writeSREInvestigationMetrics(&b, views)
	writeMigrationEnrichMetrics(&b, migration.EnrichDroppedCounts())
	writePGAuditMetrics(&b, pgaudit.RecordCounts())
	writeAskMetrics(&b, ask.AnswerCounts())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := fmt.Fprint(w, b.String()); err != nil {
		logWarn("prometheus", "write /metrics response: %v", err)
	}
}

// writeProcessMetrics renders the process-wide families: the shared LLM
// client and the optimizer (standalone has its counts).
func writeProcessMetrics(b *strings.Builder, views []metricsView) {
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
	metricsreg.Header(b, "pg_sage_optimizer_recommendations")
	if v, found := sourceView(views, metricsSourceOptimizer); found &&
		v.ok(metricsSourceOptimizer) {
		for _, cat := range sortedKeys(v.snap.optimizer) {
			writeSample(b, "pg_sage_optimizer_recommendations",
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
	// Fleet totals only; per-database series are the health families.
	status := fleetMgr.FleetStatus()
	b.WriteString("# HELP pg_sage_fleet_databases Total fleet databases\n" +
		"# TYPE pg_sage_fleet_databases gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_databases %d\n\n",
		status.Summary.TotalDatabases)

	b.WriteString("# HELP pg_sage_fleet_healthy Healthy databases\n" +
		"# TYPE pg_sage_fleet_healthy gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_healthy %d\n\n",
		status.Summary.Healthy)

	metricsreg.Header(b, "pg_sage_fleet_findings_open")
	fmt.Fprintf(b, "pg_sage_fleet_findings_open %d\n\n",
		status.Summary.TotalFindings)

	b.WriteString("# HELP pg_sage_fleet_findings_critical Total critical findings\n" +
		"# TYPE pg_sage_fleet_findings_critical gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_findings_critical %d\n\n",
		status.Summary.TotalCritical)
}
