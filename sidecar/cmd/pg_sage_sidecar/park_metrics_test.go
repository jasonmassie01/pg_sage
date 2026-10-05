package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

// /metrics exposes the gate's parks by reason (one per evaluation) and the
// verification waits released by an operator override or a hard deadline.

func TestWriteParkMetrics(t *testing.T) {
	var b strings.Builder
	writeParkMetrics(&b,
		[]executor.ParkCount{
			{Database: "lifeos", Reason: "awaiting_verification", Count: 3},
			{Database: "lifeos", Reason: "rate_limit_exceeded", Count: 1}},
		[]executor.WaitReleaseCount{
			{Database: "lifeos", Cause: "hard_deadline", Count: 2}})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_policy_parks_total counter",
		`pg_sage_policy_parks_total{database="lifeos",reason="awaiting_verification"} 3`,
		`pg_sage_policy_parks_total{database="lifeos",reason="rate_limit_exceeded"} 1`,
		"# TYPE pg_sage_verification_wait_releases_total counter",
		`pg_sage_verification_wait_releases_total{database="lifeos",cause="hard_deadline"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
}

// With nothing counted the families are still declared, with no series.
func TestWriteParkMetricsEmpty(t *testing.T) {
	var b strings.Builder
	writeParkMetrics(&b, nil, nil)
	out := b.String()
	if !strings.Contains(out, "# HELP pg_sage_policy_parks_total") ||
		strings.Contains(out, "pg_sage_policy_parks_total{") ||
		strings.Contains(out, "pg_sage_verification_wait_releases_total{") {
		t.Fatalf("empty metrics:\n%s", out)
	}
}
