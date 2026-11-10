package slo

import (
	"context"
	"testing"
	"time"
)

// Review of playbooks-07: a sparse window may be decided by the other
// window, invalid data may not. A window that is both partial and
// invalid must report invalid_value, or a broken query hidden behind a
// partial window would turn the SLO ok.
//
// No concurrent access tests: Combine, PromSource.Window and Evaluate
// hold no shared state.

// A stored window covering 20 of 60 minutes with more bad than eligible
// events is invalid, not partial, and keeps the rule unknown.
func TestCombine_PartialInvalidIsInvalid(t *testing.T) {
	o := appObjective()
	from := now.Add(-time.Hour)
	w := Combine([]SeriesAgg{agg("s", now.Add(-20*time.Minute), now, 501, 500, 0)}, o,
		from, now, now)
	if w.Unknown != ReasonInvalidValue {
		t.Fatalf("partial window with bad>eligible: unknown = %q, want %q (%+v)",
			w.Unknown, ReasonInvalidValue, w)
	}
	ws := windowsAt(allBurning(0.1))
	w.Duration = 5 * time.Minute
	ws[5*time.Minute] = w
	st := Evaluate(o, DefaultRules(), ws, known(30*24*time.Hour, 0.1), now)
	if st.State != StateUnknown || st.Rules[0].Unknown != ReasonInvalidValue ||
		st.Rules[0].Partial != "" {
		t.Fatalf("status = %s rule = %+v, want unknown invalid_value", st.State,
			st.Rules[0])
	}
	// A valid partial window is still partial (and may be decided).
	ok := Combine([]SeriesAgg{agg("s", now.Add(-20*time.Minute), now, 5, 500, 0)}, o,
		from, now, now)
	if ok.Unknown != ReasonPartialWindow {
		t.Fatalf("valid partial window: unknown = %q", ok.Unknown)
	}
}

// The same for a Prometheus window measured by its coverage query.
func TestPromSource_CoveragePartialInvalidIsInvalid(t *testing.T) {
	stub := &promStub{answer: func(_, q string) (int, string) {
		switch {
		case q == substitute(coverageQ, 5*time.Minute):
			return 200, vector(unixString(now.Add(-time.Minute)))
		case q == substitute(coverageQ, 30*24*time.Hour):
			return 200, vector(unixString(now.Add(-2 * time.Hour)))
		case q == `sum(increase(errors[30d]))`:
			return 200, vector("20000")
		}
		return 200, vector("10000")
	}}
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	o := promCoverageObjective()
	w := src.Window(context.Background(), o, o.Window, now)
	if w.Unknown != ReasonInvalidValue || w.Known() {
		t.Fatalf("partial window with bad>eligible: %+v, want invalid_value", w)
	}
}

// A Prometheus SLO not yet evaluated already says its coverage is
// unverified; with a coverage query, or for a pushed SLI, it does not.
func TestUnevaluatedStatus_CoverageUnverified(t *testing.T) {
	prom := promCoverageObjective()
	prom.CoverageQuery = ""
	cases := map[string]struct {
		o    Objective
		want bool
	}{"prometheus": {prom, true}, "covered": {promCoverageObjective(), false},
		"push": {appObjective(), false}}
	for name, c := range cases {
		st := unevaluatedStatus(c.o, c.o.Hash())
		if st.CoverageUnverified != c.want || st.State != StateUnknown {
			t.Errorf("%s: state %s coverage unverified = %v, want %v", name, st.State,
				st.CoverageUnverified, c.want)
		}
	}
}
