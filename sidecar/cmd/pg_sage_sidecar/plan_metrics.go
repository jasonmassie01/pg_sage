package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writePlanApprovalMetrics renders pg_sage_plan_approvals_total (ux-03):
// new plan approvals by mode, every mode present (zero included) in a
// fixed order.
func writePlanApprovalMetrics(b *strings.Builder, counts map[changeplan.Mode]uint64) {
	const name = "pg_sage_plan_approvals_total"
	metricsreg.Header(b, name)
	for _, m := range []changeplan.Mode{changeplan.ModeAll, changeplan.ModeStep,
		changeplan.ModeUpTo} {
		writeSample(b, name, []labelPair{{"mode", string(m)}}, float64(counts[m]))
	}
	b.WriteString("\n")
}
