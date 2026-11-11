package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeParkMetrics emits the standing gate's parks by reason (each
// evaluation of a parked candidate counts, so a rate shows how long
// candidates wait) and the verification waits that ended without a
// verdict, by cause: an operator override or the hard deadline.
func writeParkMetrics(b *strings.Builder, parks []executor.ParkCount,
	releases []executor.WaitReleaseCount) {
	metricsreg.Header(b, "pg_sage_policy_parks_total")
	for _, p := range parks {
		writeSample(b, "pg_sage_policy_parks_total",
			[]labelPair{{"database", p.Database}, {"reason", p.Reason}}, p.Count)
	}
	metricsreg.Header(b, "pg_sage_verification_wait_releases_total")
	for _, r := range releases {
		writeSample(b, "pg_sage_verification_wait_releases_total",
			[]labelPair{{"database", r.Database}, {"cause", r.Cause}}, r.Count)
	}
	b.WriteString("\n")
}

// writeRecordFailureMetrics emits the standing-gate decisions that could
// not be recorded in sage.decision, by the decision's reason. Each one was
// answered policy_unavailable (fail closed); any series above zero means
// the ledger is missing decisions (policy-01).
func writeRecordFailureMetrics(b *strings.Builder, failures []executor.RecordFailureCount) {
	metricsreg.Header(b, "pg_sage_policy_record_failures_total")
	for _, f := range failures {
		writeSample(b, "pg_sage_policy_record_failures_total",
			[]labelPair{{"database", f.Database}, {"reason", f.Reason}}, f.Count)
	}
	b.WriteString("\n")
}
