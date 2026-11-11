package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// writeSREProbeMetrics emits the Sage SRE probe slots' queue by database,
// caller class (investigation, detector, runway, log_rca, narration,
// action_target, other) and limiter (database: the database's one slot;
// sidecar: the sidecar's shared slots). Every wait counts, granted or
// refused; a refusal is a probe that never ran. Investigation refusals
// show investigations losing their slot to background sampling (sre-06).
func writeSREProbeMetrics(b *strings.Builder, stats []probes.QueueStat) {
	metricsreg.Header(b, "pg_sage_sre_probe_queue_wait_seconds")
	for _, s := range stats {
		l := sreProbeLabels(s)
		fmt.Fprintf(b, "pg_sage_sre_probe_queue_wait_seconds_sum{%s} %g\n", l, s.WaitSeconds)
		fmt.Fprintf(b, "pg_sage_sre_probe_queue_wait_seconds_count{%s} %d\n", l, s.Waits)
	}
	metricsreg.Header(b, "pg_sage_sre_probe_refusals_total")
	for _, s := range stats {
		fmt.Fprintf(b, "pg_sage_sre_probe_refusals_total{%s} %d\n", sreProbeLabels(s),
			s.Refusals)
	}
	b.WriteString("\n")
}

func sreProbeLabels(s probes.QueueStat) string {
	return fmt.Sprintf("database=%q,caller=%q,limiter=%q", s.Database, string(s.Caller),
		s.Limiter)
}
