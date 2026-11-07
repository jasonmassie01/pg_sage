package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

// policy-01: /metrics exposes the standing-gate decisions that could not be
// recorded in sage.decision, by database and decision reason.

func TestWriteRecordFailureMetrics(t *testing.T) {
	var b strings.Builder
	writeRecordFailureMetrics(&b, []executor.RecordFailureCount{
		{Database: "lifeos", Reason: "no_typed_contract", Count: 4},
		{Database: "orders", Reason: "authorized", Count: 1}})
	out := b.String()
	for _, want := range []string{
		"# HELP pg_sage_policy_record_failures_total",
		"# TYPE pg_sage_policy_record_failures_total counter",
		`pg_sage_policy_record_failures_total{database="lifeos",reason="no_typed_contract"} 4`,
		`pg_sage_policy_record_failures_total{database="orders",reason="authorized"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
}

// With nothing counted the family is still declared, with no series.
func TestWriteRecordFailureMetricsEmpty(t *testing.T) {
	var b strings.Builder
	writeRecordFailureMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_policy_record_failures_total counter") ||
		strings.Contains(out, "pg_sage_policy_record_failures_total{") {
		t.Fatalf("empty metrics:\n%s", out)
	}
}
