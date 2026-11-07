package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
)

// writeParkMetrics emits the standing gate's parks by reason (each
// evaluation of a parked candidate counts, so a rate shows how long
// candidates wait) and the verification waits that ended without a
// verdict, by cause: an operator override or the hard deadline.
func writeParkMetrics(b *strings.Builder, parks []executor.ParkCount,
	releases []executor.WaitReleaseCount) {
	b.WriteString("# HELP pg_sage_policy_parks_total Self-initiated changes the policy " +
		"gate parked, by reason (one per evaluation)\n" +
		"# TYPE pg_sage_policy_parks_total counter\n")
	for _, p := range parks {
		fmt.Fprintf(b, "pg_sage_policy_parks_total{database=%q,reason=%q} %d\n",
			p.Database, p.Reason, p.Count)
	}
	b.WriteString("# HELP pg_sage_verification_wait_releases_total Verification waits " +
		"that ended without a verdict, by cause (operator_override, hard_deadline)\n" +
		"# TYPE pg_sage_verification_wait_releases_total counter\n")
	for _, r := range releases {
		fmt.Fprintf(b, "pg_sage_verification_wait_releases_total{database=%q,cause=%q} %d\n",
			r.Database, r.Cause, r.Count)
	}
	b.WriteString("\n")
}

// writeRecordFailureMetrics emits the standing-gate decisions that could
// not be recorded in sage.decision, by the decision's reason. Each one was
// answered policy_unavailable (fail closed); any series above zero means
// the ledger is missing decisions (policy-01).
func writeRecordFailureMetrics(b *strings.Builder, failures []executor.RecordFailureCount) {
	b.WriteString("# HELP pg_sage_policy_record_failures_total Policy gate decisions that " +
		"could not be recorded in the decision ledger, by decision reason\n" +
		"# TYPE pg_sage_policy_record_failures_total counter\n")
	for _, f := range failures {
		fmt.Fprintf(b, "pg_sage_policy_record_failures_total{database=%q,reason=%q} %d\n",
			f.Database, f.Reason, f.Count)
	}
	b.WriteString("\n")
}
