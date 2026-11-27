package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeAskMetrics emits Ask Sage's answers by status and stop since the
// process started. A growing wall_clock series means questions need more
// than ask.max_seconds_per_question; cancelled ones were cut off by their
// caller after they took an action (and are stored as incomplete).
func writeAskMetrics(b *strings.Builder, counts []ask.AnswerCount) {
	metricsreg.Header(b, "pg_sage_ask_answers_total")
	for _, c := range counts {
		fmt.Fprintf(b, "pg_sage_ask_answers_total{database=%q,status=%q,stop=%q} %d\n",
			c.Database, c.Status, c.Stop, c.Count)
	}
	b.WriteString("\n")
}

// writeAskRunMetrics emits Ask Sage's asynchronous runs: in progress on
// this sidecar, and ended by state since the process started.
func writeAskRunMetrics(b *strings.Builder, active []ask.ActiveRun, ended []ask.RunCount) {
	metricsreg.Header(b, "pg_sage_ask_runs_active")
	for _, a := range active {
		fmt.Fprintf(b, "pg_sage_ask_runs_active{database=%q} %d\n", a.Database, a.Active)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_ask_runs_total")
	for _, c := range ended {
		fmt.Fprintf(b, "pg_sage_ask_runs_total{database=%q,state=%q} %d\n", c.Database,
			c.State, c.Count)
	}
	b.WriteString("\n")
}
