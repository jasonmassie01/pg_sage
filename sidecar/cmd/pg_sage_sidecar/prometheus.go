package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// handleMetrics renders in-memory state and the refreshers' snapshots;
// it performs no database I/O (observability-03). Each database's
// snapshot is loaded once, so a scrape sees one consistent refresh.
func handleMetrics(w http.ResponseWriter, _ *http.Request) {
	views := processMetrics.views()
	var b strings.Builder

	writeProcessStateMetrics(&b)
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
	writeAuditHealthFamilies(&b)
	writeAskAndInboxMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := fmt.Fprint(w, b.String()); err != nil {
		logWarn("prometheus", "write /metrics response: %v", err)
	}
}

// writeProcessStateMetrics renders the process's own state: build, mode,
// listeners, login limiter, TLS, SSO discovery, deferred indexes, the first
// look, setup within the hour (ux-04b) and the first review grade (review-01a).
func writeProcessStateMetrics(b *strings.Builder) {
	writeBuildMetrics(b, cfg.Mode)
	writeModeMetric(b, cfg.Mode)
	writeMetricsListenerMetric(b)
	writeLoginLimiterMetric(b, api.LoginLimiterSaturatedTotal())
	writeTLSMetrics(b, apiTLS)
	writeSSODiscoveryMetrics(b, api.CurrentSSODiscoveryStatus())
	writeDeferredIndexMetrics(b, deferredIndexGauges)
	writeFirstLookMetrics(b, firstLookTracker.Snapshot())
	writeSetupMetrics(b, onboarding.SetupRatios())
	writeReviewMetrics(b, reviewFirstGrades)
}

// writeProcessMetrics renders the process-wide families: every
// registered LLM client (llm_metrics.go) and the optimizer.
func writeProcessMetrics(b *strings.Builder, views []metricsView) {
	writeLLMHealthFromRegistry(b)
	// Optimizer metrics from sage.findings, as last refreshed.
	writeOptimizerMetrics(b, views)
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

	metricsreg.Header(b, "pg_sage_optimizer_enabled")
	fmt.Fprintf(b, "pg_sage_optimizer_enabled %d\n\n", optimizerGaugeValue())
}

func writeFleetMetrics(b *strings.Builder) {
	// Fleet totals only; per-database series are the health families.
	defer writeFleetEnrollMetrics(b) // fleetpol-01b, meta-db discovery
	status := fleetMgr.FleetStatus()
	metricsreg.Header(b, "pg_sage_fleet_databases")
	fmt.Fprintf(b, "pg_sage_fleet_databases %d\n\n",
		status.Summary.TotalDatabases)

	metricsreg.Header(b, "pg_sage_fleet_healthy")
	fmt.Fprintf(b, "pg_sage_fleet_healthy %d\n\n",
		status.Summary.Healthy)

	metricsreg.Header(b, "pg_sage_fleet_findings_open")
	fmt.Fprintf(b, "pg_sage_fleet_findings_open %d\n\n",
		status.Summary.TotalFindings)

	metricsreg.Header(b, "pg_sage_fleet_findings_critical")
	fmt.Fprintf(b, "pg_sage_fleet_findings_critical %d\n\n",
		status.Summary.TotalCritical)
}

// writeAskAndInboxMetrics renders Ask Sage's answers and runs, the inbox,
// the outcome ledger and plan approvals, the in-process families of the
// people-facing surfaces.
func writeAskAndInboxMetrics(b *strings.Builder) {
	writeAskMetrics(b, ask.AnswerCounts())
	writeAskRunMetrics(b, ask.ActiveRuns(), ask.RunCounts())
	writeInboxMetrics(b)
	writeOutcomesMetrics(b, outcomes.MeasuredRatios())
	writePlanApprovalMetrics(b, changeplan.ApprovalCounts())
}
