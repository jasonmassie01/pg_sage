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

	// ----- pg_sage_info -----
	writeInfo(&b, ctx)

	// ----- pg_sage_findings_total -----
	writeFindings(&b, ctx)

	// ----- pg_sage_circuit_breaker_state -----
	writeCircuitBreaker(&b, ctx)

	// ----- pg_sage_status (generic from sage.status()) -----
	writeStatus(&b, ctx)

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
