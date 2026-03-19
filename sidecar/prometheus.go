package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// startPrometheusServer runs the /metrics endpoint on the configured port.
func startPrometheusServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("[prometheus] listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[prometheus] server error: %v", err)
		}
	}()
	return srv
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder

	if extensionAvailable {
		// ----- pg_sage_info -----
		writeInfo(&b, ctx)

		// ----- pg_sage_findings_total -----
		writeFindings(&b, ctx)

		// ----- pg_sage_circuit_breaker_state -----
		writeCircuitBreaker(&b, ctx)

		// ----- pg_sage_status (generic from sage.status()) -----
		writeStatus(&b, ctx)
	} else {
		// Sidecar-only mode: emit metrics from direct catalog queries
		writeSidecarInfo(&b, ctx)
		writeSidecarConnectionMetrics(&b, ctx)
		writeSidecarDatabaseMetrics(&b, ctx)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

// ---------------------------------------------------------------------------
// Metric writers
// ---------------------------------------------------------------------------

func writeInfo(b *strings.Builder, ctx context.Context) {
	var version string
	err := pool.QueryRow(ctx, "SELECT sage.status()->>'version'").Scan(&version)
	if err != nil || version == "" {
		version = "unknown"
	}
	b.WriteString("# HELP pg_sage_info pg_sage version information\n")
	b.WriteString("# TYPE pg_sage_info gauge\n")
	fmt.Fprintf(b, "pg_sage_info{version=%q} 1\n", version)
	b.WriteString("\n")
}

func writeFindings(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_findings_total Number of open findings by severity\n")
	b.WriteString("# TYPE pg_sage_findings_total gauge\n")

	rows, err := pool.Query(ctx,
		`SELECT severity, count(*)
		 FROM sage.findings
		 WHERE status = 'open'
		 GROUP BY severity`)
	if err != nil {
		// Table might not exist yet
		fmt.Fprintf(b, "pg_sage_findings_total{severity=\"critical\"} 0\n")
		fmt.Fprintf(b, "pg_sage_findings_total{severity=\"warning\"} 0\n")
		fmt.Fprintf(b, "pg_sage_findings_total{severity=\"info\"} 0\n")
		b.WriteString("\n")
		return
	}
	defer rows.Close()

	found := map[string]int64{}
	for rows.Next() {
		var sev string
		var cnt int64
		if err := rows.Scan(&sev, &cnt); err == nil {
			found[sev] = cnt
		}
	}
	for _, sev := range []string{"critical", "warning", "info"} {
		fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, found[sev])
	}
	b.WriteString("\n")
}

func writeCircuitBreaker(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_circuit_breaker_state Circuit breaker state (0=closed, 1=open)\n")
	b.WriteString("# TYPE pg_sage_circuit_breaker_state gauge\n")

	statusJSON, err := queryStatus(ctx)
	if err != nil {
		fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"db\"} 0\n")
		fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"llm\"} 0\n")
		b.WriteString("\n")
		return
	}

	var status map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"db\"} 0\n")
		fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"llm\"} 0\n")
		b.WriteString("\n")
		return
	}

	dbState := 0
	llmState := 0
	if v, ok := status["circuit_state"]; ok {
		if vs, ok := v.(string); ok && vs != "closed" {
			dbState = 1
		}
	}
	if v, ok := status["llm_circuit_state"]; ok {
		if vs, ok := v.(string); ok && vs != "closed" {
			llmState = 1
		}
	}

	fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"db\"} %d\n", dbState)
	fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"llm\"} %d\n", llmState)
	b.WriteString("\n")
}

func writeStatus(b *strings.Builder, ctx context.Context) {
	statusJSON, err := queryStatus(ctx)
	if err != nil {
		return
	}

	var status map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return
	}

	// Emit numeric fields as gauges
	for key, val := range status {
		switch key {
		case "circuit_state", "llm_circuit_state", "version":
			continue // already handled above
		}
		switch v := val.(type) {
		case float64:
			metricName := "pg_sage_status_" + sanitizeMetricName(key)
			fmt.Fprintf(b, "# TYPE %s gauge\n", metricName)
			fmt.Fprintf(b, "%s %g\n", metricName, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Sidecar-only metric writers (no extension required)
// ---------------------------------------------------------------------------

func writeSidecarInfo(b *strings.Builder, ctx context.Context) {
	var pgVersion string
	err := pool.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion)
	if err != nil {
		pgVersion = "unknown"
	}
	b.WriteString("# HELP pg_sage_info pg_sage sidecar information\n")
	b.WriteString("# TYPE pg_sage_info gauge\n")
	fmt.Fprintf(b, "pg_sage_info{version=\"sidecar-only\",pg_version=%q} 1\n", pgVersion)
	b.WriteString("\n")
}

func writeSidecarConnectionMetrics(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_connections_total Current number of connections by state\n")
	b.WriteString("# TYPE pg_sage_connections_total gauge\n")

	rows, err := pool.Query(ctx,
		`SELECT coalesce(state, 'unknown'), count(*)
		 FROM pg_stat_activity
		 GROUP BY state`)
	if err != nil {
		fmt.Fprintf(b, "pg_sage_connections_total{state=\"unknown\"} 0\n")
		b.WriteString("\n")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var state string
		var cnt int64
		if err := rows.Scan(&state, &cnt); err == nil {
			fmt.Fprintf(b, "pg_sage_connections_total{state=%q} %d\n", state, cnt)
		}
	}
	b.WriteString("\n")

	// Max connections
	var maxConn int
	if err := pool.QueryRow(ctx, "SELECT setting::int FROM pg_settings WHERE name = 'max_connections'").Scan(&maxConn); err == nil {
		b.WriteString("# HELP pg_sage_max_connections Configured max_connections\n")
		b.WriteString("# TYPE pg_sage_max_connections gauge\n")
		fmt.Fprintf(b, "pg_sage_max_connections %d\n", maxConn)
		b.WriteString("\n")
	}
}

func writeSidecarDatabaseMetrics(b *strings.Builder, ctx context.Context) {
	// Database size
	var dbSizeBytes int64
	if err := pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&dbSizeBytes); err == nil {
		b.WriteString("# HELP pg_sage_database_size_bytes Database size in bytes\n")
		b.WriteString("# TYPE pg_sage_database_size_bytes gauge\n")
		fmt.Fprintf(b, "pg_sage_database_size_bytes %d\n", dbSizeBytes)
		b.WriteString("\n")
	}

	// Transaction stats from pg_stat_database
	var xactCommit, xactRollback, blksHit, blksRead, deadlocks int64
	err := pool.QueryRow(ctx,
		`SELECT xact_commit, xact_rollback, blks_hit, blks_read, deadlocks
		 FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&xactCommit, &xactRollback, &blksHit, &blksRead, &deadlocks)
	if err == nil {
		b.WriteString("# HELP pg_sage_xact_commit_total Transactions committed\n")
		b.WriteString("# TYPE pg_sage_xact_commit_total counter\n")
		fmt.Fprintf(b, "pg_sage_xact_commit_total %d\n", xactCommit)
		b.WriteString("\n")

		b.WriteString("# HELP pg_sage_xact_rollback_total Transactions rolled back\n")
		b.WriteString("# TYPE pg_sage_xact_rollback_total counter\n")
		fmt.Fprintf(b, "pg_sage_xact_rollback_total %d\n", xactRollback)
		b.WriteString("\n")

		b.WriteString("# HELP pg_sage_blks_hit_total Shared buffer hits\n")
		b.WriteString("# TYPE pg_sage_blks_hit_total counter\n")
		fmt.Fprintf(b, "pg_sage_blks_hit_total %d\n", blksHit)
		b.WriteString("\n")

		b.WriteString("# HELP pg_sage_blks_read_total Disk blocks read\n")
		b.WriteString("# TYPE pg_sage_blks_read_total counter\n")
		fmt.Fprintf(b, "pg_sage_blks_read_total %d\n", blksRead)
		b.WriteString("\n")

		b.WriteString("# HELP pg_sage_deadlocks_total Deadlocks detected\n")
		b.WriteString("# TYPE pg_sage_deadlocks_total counter\n")
		fmt.Fprintf(b, "pg_sage_deadlocks_total %d\n", deadlocks)
		b.WriteString("\n")

		// Cache hit ratio
		if (blksHit + blksRead) > 0 {
			ratio := float64(blksHit) / float64(blksHit+blksRead)
			b.WriteString("# HELP pg_sage_cache_hit_ratio Buffer cache hit ratio\n")
			b.WriteString("# TYPE pg_sage_cache_hit_ratio gauge\n")
			fmt.Fprintf(b, "pg_sage_cache_hit_ratio %g\n", ratio)
			b.WriteString("\n")
		}
	}

	// Uptime
	var uptimeSeconds int64
	if err := pool.QueryRow(ctx, "SELECT extract(epoch FROM now() - pg_postmaster_start_time())::bigint").Scan(&uptimeSeconds); err == nil {
		b.WriteString("# HELP pg_sage_uptime_seconds PostgreSQL uptime in seconds\n")
		b.WriteString("# TYPE pg_sage_uptime_seconds gauge\n")
		fmt.Fprintf(b, "pg_sage_uptime_seconds %d\n", uptimeSeconds)
		b.WriteString("\n")
	}
}

func queryStatus(ctx context.Context) (string, error) {
	var result string
	err := pool.QueryRow(ctx, "SELECT sage.status()::text").Scan(&result)
	return result, err
}

func sanitizeMetricName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
