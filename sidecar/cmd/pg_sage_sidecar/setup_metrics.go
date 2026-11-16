package main

import (
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// writeSetupMetrics exports pg_sage_setup_within_hour_ratio (ux-04b): of
// the new installs this process monitors, the share with a model and with
// a notification channel set within one hour of install. With no new
// install the ratio is unknown, so nothing is written.
func writeSetupMetrics(b *strings.Builder, r onboarding.SetupRatio) {
	if r.Installs == 0 {
		return
	}
	const name = "pg_sage_setup_within_hour_ratio"
	metricsreg.Header(b, name)
	for _, part := range []struct {
		label string
		value float64
	}{{"model", r.Model}, {"channel", r.Channel}} {
		writeSample(b, name, []labelPair{{"part", part.label}},
			strconv.FormatFloat(part.value, 'f', -1, 64))
	}
	b.WriteByte('\n')
}
