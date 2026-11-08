package optimizer

import (
	"context"
	"testing"
	"time"
)

// Rejection memory against a lowered hypopg_min_improvement_pct
// (indexes-06): a remembered rejection only suppresses a re-measurement
// while its improvement is still below the current minimum.

func rejectionAt(t *testing.T, improvement float64) rejection {
	t.Helper()
	r := storedRejection(t, statusDDL, time.Hour)
	r.ImprovementPct, r.MinImprovementPct = improvement, 10
	return r
}

func TestTableMemory_SuppressHonoursTheCurrentMinimum(t *testing.T) {
	rec := Recommendation{DDL: statusDDL}
	cases := []struct {
		name        string
		improvement float64
		minPct      float64
		suppressed  bool
	}{
		{"lowered to the remembered improvement re-measures", 8, 8.0, false},
		{"just above the remembered improvement suppresses", 8, 8.1, true},
		{"the minimum it was rejected under suppresses", 8, 10, true},
		{"a raised minimum suppresses", 8, 25, true},
		{"lowered below the remembered improvement re-measures", 8, 7, false},
		{"zero improvement at a zero minimum re-measures", 0, 0, false},
		{"a regression at a zero minimum suppresses", -5, 0, true},
	}
	for _, c := range cases {
		v := testMemory(newMemStore(rejectionAt(t, c.improvement)), nil).
			view(context.Background(), memTable())
		if _, ok := v.suppress(rec, c.minPct); ok != c.suppressed {
			t.Errorf("%s: suppressed = %t, want %t", c.name, ok, c.suppressed)
		}
	}
}

func TestTableMemory_SuppressNilViewWithMinimum(t *testing.T) {
	var v *tableMemory
	if _, ok := v.suppress(Recommendation{DDL: statusDDL}, 0); ok {
		t.Fatal("a nil view suppressed a candidate")
	}
}

func TestAdmit_LoweredMinimumRemeasuresARememberedRejection(t *testing.T) {
	for _, c := range []struct {
		minPct   float64
		whatIfs  int32
		outcome  AdmissionOutcome
		boundary string
	}{
		{8.0, 1, AdmitAccepted, "8.0 re-measures"},
		{8.1, 0, AdmitMeasured, "8.1 suppresses"},
	} {
		o, w, _ := admissionOptimizer(newMemStore(rejectionAt(t, 8)), WhatIfResult{
			Measured: 2, Improvement: 9, SizeBytes: 8192})
		o.cfg.HypoPGMinImprovePct = c.minPct
		a := o.Admit(context.Background(), candidate(statusDDL), admitTable())
		if a.Outcome != c.outcome || w.calls.Load() != c.whatIfs {
			t.Fatalf("%s: outcome %q what-ifs %d (%s)", c.boundary, a.Outcome,
				w.calls.Load(), a.Reason)
		}
	}
}
