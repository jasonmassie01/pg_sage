package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// writeSREProbeMetrics emits the Sage SRE probe slots' queue by database,
// caller class (investigation, detector, runway, log_rca, narration,
// action_target, other) and limiter (database: the database's one slot;
// sidecar: the sidecar's shared slots). Every wait counts, granted or
// refused; a refusal is a probe that never ran. Investigation refusals
// show investigations losing their slot to background sampling (sre-06).
func writeSREProbeMetrics(b *strings.Builder, stats []probes.QueueStat) {
	b.WriteString("# HELP pg_sage_sre_probe_queue_wait_seconds Time Sage SRE probes " +
		"waited for a probe slot, by caller class and limiter\n" +
		"# TYPE pg_sage_sre_probe_queue_wait_seconds summary\n")
	for _, s := range stats {
		l := sreProbeLabels(s)
		fmt.Fprintf(b, "pg_sage_sre_probe_queue_wait_seconds_sum{%s} %g\n", l, s.WaitSeconds)
		fmt.Fprintf(b, "pg_sage_sre_probe_queue_wait_seconds_count{%s} %d\n", l, s.Waits)
	}
	b.WriteString("# HELP pg_sage_sre_probe_refusals_total Sage SRE probes refused a " +
		"probe slot within the queue wait (never run), by caller class and limiter\n" +
		"# TYPE pg_sage_sre_probe_refusals_total counter\n")
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
