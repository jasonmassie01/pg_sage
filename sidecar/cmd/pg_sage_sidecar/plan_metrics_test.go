package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// pg_sage_plan_approvals_total{mode} (ux-03) renders every mode, zeros
// included, in a fixed order.
func TestWritePlanApprovalMetrics(t *testing.T) {
	var b strings.Builder
	writePlanApprovalMetrics(&b, map[changeplan.Mode]uint64{changeplan.ModeAll: 4,
		changeplan.ModeStep: 1})
	out := b.String()
	want := []string{"# TYPE pg_sage_plan_approvals_total counter",
		`pg_sage_plan_approvals_total{mode="all"} 4`,
		`pg_sage_plan_approvals_total{mode="step"} 1`,
		`pg_sage_plan_approvals_total{mode="up_to"} 0`}
	last := -1
	for _, w := range want {
		at := strings.Index(out, w)
		if at < 0 || at < last {
			t.Fatalf("output lacks %q in order:\n%s", w, out)
		}
		last = at
	}
	var empty strings.Builder
	writePlanApprovalMetrics(&empty, nil)
	if !strings.Contains(empty.String(), `pg_sage_plan_approvals_total{mode="all"} 0`) {
		t.Fatalf("no approvals yet:\n%s", empty.String())
	}
}
