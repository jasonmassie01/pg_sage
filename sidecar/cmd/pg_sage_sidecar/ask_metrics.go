package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/ask"
)

// writeAskMetrics emits Ask Sage's answers by status and stop since the
// process started. A growing wall_clock series means questions need more
// than ask.max_seconds_per_question; cancelled ones were cut off by their
// caller after they took an action (and are stored as incomplete).
func writeAskMetrics(b *strings.Builder, counts []ask.AnswerCount) {
	b.WriteString("# HELP pg_sage_ask_answers_total Ask Sage answers by status and the " +
		"reason the run stopped\n# TYPE pg_sage_ask_answers_total counter\n")
	for _, c := range counts {
		fmt.Fprintf(b, "pg_sage_ask_answers_total{database=%q,status=%q,stop=%q} %d\n",
			c.Database, c.Status, c.Stop, c.Count)
	}
	b.WriteString("\n")
}
