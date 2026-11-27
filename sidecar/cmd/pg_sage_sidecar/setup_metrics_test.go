package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/onboarding"
)

// ux-04b: pg_sage_setup_within_hour_ratio{part=model|channel}. No
// concurrent access test: the writer formats one snapshot of the gauge
// (onboarding.TestSetupGaugeConcurrentObservers covers the gauge).

func TestWriteSetupMetricsRatios(t *testing.T) {
	var b strings.Builder
	writeSetupMetrics(&b, onboarding.SetupRatio{Installs: 4, Model: 0.75, Channel: 0.25})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_setup_within_hour_ratio gauge",
		`pg_sage_setup_within_hour_ratio{part="model"} 0.75`,
		`pg_sage_setup_within_hour_ratio{part="channel"} 0.25`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE pg_sage_setup_within_hour_ratio") != 1 {
		t.Fatalf("one header expected:\n%s", out)
	}
}

func TestWriteSetupMetricsAbsentWithoutNewInstalls(t *testing.T) {
	var b strings.Builder
	writeSetupMetrics(&b, onboarding.SetupRatio{})
	if b.Len() != 0 {
		t.Fatalf("no new installs wrote %q, want nothing (an unknown ratio is not 0)",
			b.String())
	}
}

func TestWriteSetupMetricsZeroRatioIsExported(t *testing.T) {
	var b strings.Builder
	writeSetupMetrics(&b, onboarding.SetupRatio{Installs: 1})
	if !strings.Contains(b.String(), `pg_sage_setup_within_hour_ratio{part="model"} 0`) {
		t.Fatalf("a known zero ratio must be exported:\n%s", b.String())
	}
}
