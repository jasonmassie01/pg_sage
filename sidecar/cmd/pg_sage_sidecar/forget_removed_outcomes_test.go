package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// A database removed from the fleet takes its measured-outcome share off
// /metrics; the other databases keep theirs.
func TestForgetRemovedDatabase_DropsItsOutcomeShare(t *testing.T) {
	preserveValueGlobals(t)
	cfg, pool, fleetMgr = &config.Config{Mode: "fleet"}, nil, nil
	def := outcomes.Query{Since: outcomes.DefaultSince, Limit: outcomes.DefaultLimit}
	for name, v := range map[string]float64{"forget_out_gone": 0.25, "forget_out_kept": 0.5} {
		v := v
		outcomes.Observe(name, def, outcomes.Result{Summary: outcomes.Summary{Changes: 4,
			MeasuredShare: &v}})
	}
	t.Cleanup(func() { outcomes.ForgetDatabase("forget_out_kept") })
	forgetRemovedDatabase("forget_out_gone")
	out := scrapeMetrics(t)
	if strings.Contains(out, `database="forget_out_gone"`) {
		t.Fatalf("metrics still carry the removed database:\n%s", out)
	}
	want := `pg_sage_outcomes_measured_ratio{database="forget_out_kept"} 0.5`
	if !strings.Contains(out, want) {
		t.Fatalf("metrics lost the kept database's share %q:\n%s", want, out)
	}
}
