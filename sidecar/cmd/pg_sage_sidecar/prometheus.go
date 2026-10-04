package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/shadow"
)

func startPrometheusServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		logInfo("prometheus", "listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("prometheus", "server error: %v", err)
		}
	}()
	return srv
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder

	// Info metric.
	b.WriteString("# HELP pg_sage_info pg_sage version\n" +
		"# TYPE pg_sage_info gauge\n")
	fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=%q} 1\n\n", version, cfg.Mode)

	writeModeMetric(&b, cfg.Mode)

	writeConnectionMetric(&b, ctx)

	// Standalone metrics.
	if cfg.IsStandalone() {
		writeStandaloneMetrics(&b, ctx)
	}

	// Fleet metrics.
	if cfg.Mode == "fleet" && fleetMgr != nil {
		writeFleetMetrics(&b)
	}
	writeFleetBudgetMetrics(&b, fleetLLMBudget)

	// Database metrics (only when global pool exists).
	if pool != nil {
		writeDatabaseMetrics(&b, ctx)
	}
	// Value lives in each monitored database (D3), so it is read from
	// every fleet instance in all modes, never from the meta pool.
	writeValueMetrics(&b, ctx, fleet.ValueSources(fleetMgr))
	writeSLOMetrics(&b, ctx, fleetMgr)
	writeSelfCostFromFleet(&b, fleetMgr)
	writeShadowMetrics(&b, shadow.DecisionCounts(), shadow.ScoreCounts())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := fmt.Fprint(w, b.String()); err != nil {
		logWarn("prometheus", "write /metrics response: %v", err)
	}
}

// writeConnectionMetric reports whether PostgreSQL is reachable: the global
// pool when there is one, otherwise any connected fleet instance.
func writeConnectionMetric(b *strings.Builder, ctx context.Context) {
	// Connection metric.
	b.WriteString("# HELP pg_sage_connection_up PostgreSQL connection status\n" +
		"# TYPE pg_sage_connection_up gauge\n")
	connUp := 0
	if pool != nil {
		if err := pool.Ping(ctx); err == nil {
			connUp = 1
		}
	} else if fleetMgr != nil {
		// Fleet mode: report up if any instance is connected.
		for _, inst := range fleetMgr.Instances() {
			if inst.Pool != nil {
				if err := inst.Pool.Ping(ctx); err == nil {
					connUp = 1
					break
				}
			}
		}
	}
	fmt.Fprintf(b, "pg_sage_connection_up %d\n\n", connUp)
}

func writeStandaloneMetrics(b *strings.Builder, ctx context.Context) {
	// Findings from sage.findings table.
	b.WriteString("# HELP pg_sage_findings_total Open findings by severity\n" +
		"# TYPE pg_sage_findings_total gauge\n")
	if anal != nil {
		counts := anal.OpenFindingsCount()
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, counts[sev])
		}
	}
	b.WriteString("\n")

	// Collector metrics.
	if coll != nil {
		snap := coll.LatestSnapshot()
		if snap != nil {
			b.WriteString("# HELP pg_sage_collector_last_run_timestamp Last collector run\n" +
				"# TYPE pg_sage_collector_last_run_timestamp gauge\n")
			fmt.Fprintf(b, "pg_sage_collector_last_run_timestamp %d\n\n", snap.CollectedAt.Unix())
		}
	}

	// LLM metrics.
	if llmClient != nil {
		writeLLMMetrics(b)
	}

	// Optimizer metrics from sage.findings.
	writeOptimizerMetrics(b, ctx)
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

func writeOptimizerMetrics(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_optimizer_recommendations_total Index recommendations by category\n")
	b.WriteString("# TYPE pg_sage_optimizer_recommendations_total gauge\n")

	rows, err := pool.Query(ctx,
		`SELECT category, count(*)
		 FROM sage.findings
		 WHERE status = 'open'
		   AND category IN ('missing_index','covering_index','partial_index','composite_index')
		 GROUP BY category`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cat string
			var cnt int64
			if rows.Scan(&cat, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_optimizer_recommendations_total{category=%q} %d\n", cat, cnt)
			}
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

	b.WriteString("# HELP pg_sage_fleet_instance_findings Per-instance open findings\n" +
		"# TYPE pg_sage_fleet_instance_findings gauge\n")
	for _, db := range status.Databases {
		fmt.Fprintf(b,
			"pg_sage_fleet_instance_findings{database=%q} %d\n",
			db.Name, db.Status.FindingsOpen)
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_fleet_instance_health Per-instance health score\n" +
		"# TYPE pg_sage_fleet_instance_health gauge\n")
	for _, db := range status.Databases {
		fmt.Fprintf(b,
			"pg_sage_fleet_instance_health{database=%q} %d\n",
			db.Name, db.Status.HealthScore)
	}
	b.WriteString("\n")
}

func writeDatabaseMetrics(b *strings.Builder, ctx context.Context) {
	// Connections.
	b.WriteString("# HELP pg_sage_connections_total Connections by state\n" +
		"# TYPE pg_sage_connections_total gauge\n")
	rows, err := pool.Query(ctx, "SELECT coalesce(state, 'unknown'), count(*) "+
		"FROM pg_stat_activity GROUP BY state")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var state string
			var cnt int64
			if rows.Scan(&state, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_connections_total{state=%q} %d\n", state, cnt)
			}
		}
		b.WriteString("\n")
	}

	// Database size: the collector's cached measurement (a size walk on
	// every scrape stat()ed every file of the database, untimed).
	if coll != nil {
		b.WriteString(databaseSizeMetric(coll.LatestSnapshot()))
	}

	// Cache hit ratio.
	var hit, read int64
	if pool.QueryRow(ctx, "SELECT blks_hit, blks_read FROM pg_stat_database "+
		"WHERE datname = current_database()").Scan(&hit, &read) == nil &&
		(hit+read) > 0 {
		ratio := float64(hit) / float64(hit+read)
		b.WriteString("# HELP pg_sage_cache_hit_ratio Buffer cache hit ratio\n" +
			"# TYPE pg_sage_cache_hit_ratio gauge\n")
		fmt.Fprintf(b, "pg_sage_cache_hit_ratio %g\n\n", ratio)
	}
}

// databaseSizeMetric renders the database size gauge from a collector
// snapshot; an unknown size (no snapshot yet, never measured) is omitted.
func databaseSizeMetric(snap *collector.Snapshot) string {
	if snap == nil || snap.System.DBSizeBytes <= 0 {
		return ""
	}
	return "# HELP pg_sage_database_size_bytes Database size\n" +
		"# TYPE pg_sage_database_size_bytes gauge\n" +
		fmt.Sprintf("pg_sage_database_size_bytes %d\n\n", snap.System.DBSizeBytes)
}
