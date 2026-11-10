package slo

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Prometheus window coverage (playbooks-07): with a coverage query (the
// earliest sample time over $window) a Prometheus window is partial or
// stale like a stored series; without one it keeps today's numbers and
// the SLO says its coverage is unverified.
//
// No concurrent access tests: PromSource holds no state of its own (the
// client's concurrency is covered in prometheus_test.go).

const coverageQ = `min_over_time(timestamp(requests)[$window:1m])`

func promCoverageObjective() Objective {
	o := appObjective()
	o.Source = SourcePrometheus
	o.BadQuery = `sum(increase(errors[$window]))`
	o.EligibleQuery = `sum(increase(requests[$window]))`
	o.CoverageQuery = coverageQ
	return o
}

func unixString(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// coverageStub answers the SLI queries with 15 bad of 10000 eligible and
// the coverage query by window: long is the answer over any window but
// the stale window, fresh the answer over the stale window (5m).
func coverageStub(long, fresh func() (int, string)) *promStub {
	return &promStub{answer: func(_, q string) (int, string) {
		switch {
		case strings.HasPrefix(q, "min_over_time") && strings.Contains(q, "[5m:1m]"):
			return fresh()
		case strings.HasPrefix(q, "min_over_time"):
			return long()
		case strings.HasPrefix(q, "sum(increase(errors"):
			return 200, vector("15")
		}
		return 200, vector("10000")
	}}
}

func answer(code int, body string) func() (int, string) {
	return func() (int, string) { return code, body }
}

// A series that began two hours ago inside a 30-day window covers under
// 1% of it: the budget is partial_window, not a 30-day budget.
func TestPromSource_CoverageQueryPartialBudget(t *testing.T) {
	o := promCoverageObjective()
	stub := coverageStub(answer(200, vector(unixString(now.Add(-2*time.Hour)))),
		answer(200, vector(unixString(now.Add(-time.Minute)))))
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	w := src.Window(context.Background(), o, o.Window, now)
	wantCoverage := float64(2*time.Hour) / float64(o.Window)
	if w.Unknown != ReasonPartialWindow || math.Abs(w.Coverage-wantCoverage) > 1e-9 ||
		w.Bad != 15 || w.Eligible != 10000 || !w.LatestAt.Equal(now) {
		t.Fatalf("window = %+v, want partial_window at coverage %v", w, wantCoverage)
	}
	st := Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)), w, now)
	if st.BudgetRemaining != nil || st.BudgetUnknown != ReasonPartialWindow ||
		st.CoverageUnverified {
		t.Fatalf("budget = %v (%s) unverified=%v", st.BudgetRemaining, st.BudgetUnknown,
			st.CoverageUnverified)
	}
	joined := strings.Join(stub.queries, "\n")
	for _, want := range []string{"timestamp(requests)[30d:1m]", "timestamp(requests)[5m:1m]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("queries %q lack %s", joined, want)
		}
	}
}

// Coverage at and around the 90% boundary, and history older than the
// window (coverage is capped at 1).
func TestPromSource_CoverageBoundary(t *testing.T) {
	d := 72 * time.Hour
	for _, c := range []struct {
		age     time.Duration
		unknown string
		cover   float64
	}{
		{time.Duration(float64(d) * MinCoverage), "", MinCoverage},
		{time.Duration(float64(d)*MinCoverage) - time.Minute, ReasonPartialWindow,
			(float64(d)*MinCoverage - float64(time.Minute)) / float64(d)},
		{d + 10*time.Minute, "", 1},
	} {
		stub := coverageStub(answer(200, vector(unixString(now.Add(-c.age)))),
			answer(200, vector(unixString(now))))
		src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
		w := src.Window(context.Background(), promCoverageObjective(), d, now)
		if w.Unknown != c.unknown || math.Abs(w.Coverage-c.cover) > 1e-9 {
			t.Errorf("age %s: window = %+v, want unknown %q coverage %v", c.age, w,
				c.unknown, c.cover)
		}
	}
}

// A scrape outage: no sample within the stale limit makes every window
// stale, even when the window's increase still has a value.
func TestPromSource_CoverageQueryStale(t *testing.T) {
	stub := coverageStub(answer(200, vector(unixString(now.Add(-40*24*time.Hour)))),
		answer(200, vector()))
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	w := src.Window(context.Background(), promCoverageObjective(), time.Hour, now)
	if w.Unknown != ReasonStale || w.Known() {
		t.Fatalf("window = %+v, want stale", w)
	}
}

// Error propagation: a failing coverage query is the connector's
// reason, never full coverage.
func TestPromSource_CoverageQueryErrors(t *testing.T) {
	cases := map[string]struct {
		long, fresh func() (int, string)
		want        string
	}{
		"long down": {answer(502, ""), answer(200, vector(unixString(now))),
			ReasonSourceError},
		"fresh down": {answer(200, vector(unixString(now))), answer(503, ""),
			ReasonSourceError},
		"long absent": {answer(200, vector()), answer(200, vector(unixString(now))),
			ReasonNoData},
		"long ambiguous": {answer(200, vector("1", "2")), answer(200, vector()), ReasonAmbiguous},
		"long NaN":       {answer(200, vector("NaN")), answer(200, vector()), ReasonInvalidValue},
		"future earliest": {answer(200, vector(unixString(now.Add(time.Hour)))),
			answer(200, vector(unixString(now))), ReasonInvalidValue},
		"huge earliest": {answer(200, vector("1e30")), answer(200, vector(unixString(now))),
			ReasonInvalidValue},
	}
	for name, c := range cases {
		stub := coverageStub(c.long, c.fresh)
		src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
		w := src.Window(context.Background(), promCoverageObjective(), time.Hour, now)
		if w.Unknown != c.want || w.Coverage == 1 || w.Known() {
			t.Errorf("%s: window = %+v, want %s without full coverage", name, w, c.want)
		}
	}
}

// Nil/empty: without a coverage query no coverage query is sent and the
// numbers are today's (coverage taken as complete); the SLO then says
// its coverage is unverified.
func TestPromSource_NoCoverageQueryIsUnverified(t *testing.T) {
	o := promCoverageObjective()
	o.CoverageQuery = ""
	stub := coverageStub(answer(500, ""), answer(500, ""))
	src := PromSource{Client: newClient(t, stub.server(t).URL, time.Second)}
	w := src.Window(context.Background(), o, o.Window, now)
	if w.Unknown != "" || w.Coverage != 1 || !w.LatestAt.Equal(now) {
		t.Fatalf("window = %+v", w)
	}
	if strings.Contains(strings.Join(stub.queries, "\n"), "min_over_time") {
		t.Fatalf("coverage query sent without one configured: %v", stub.queries)
	}
	st := Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)), w, now)
	if !st.CoverageUnverified || st.BudgetRemaining == nil {
		t.Fatalf("status = %+v", st)
	}
}

// Invalid input: a coverage query must use $window and belongs to a
// Prometheus source only.
func TestObjective_ValidateCoverageQuery(t *testing.T) {
	if err := promCoverageObjective().Validate(); err != nil {
		t.Fatalf("prometheus objective with a coverage query: %v", err)
	}
	noWindow := promCoverageObjective()
	noWindow.CoverageQuery = "min_over_time(timestamp(requests)[30d:1m])"
	push := appObjective()
	push.CoverageQuery = coverageQ
	for name, o := range map[string]Objective{"no $window": noWindow, "push": push} {
		err := o.Validate()
		if !errors.Is(err, ErrInvalidObjective) {
			t.Errorf("%s: Validate = %v, want ErrInvalidObjective", name, err)
		}
	}
	if err := noWindow.Validate(); err == nil ||
		!strings.Contains(err.Error(), "coverage query must use $window") {
		t.Errorf("no $window: error %v does not name the coverage query", err)
	}
}

// Prometheus coverage is unverified without a coverage query; with one,
// or for a pushed or proxy SLI, it is measured.
func TestEvaluate_CoverageUnverified(t *testing.T) {
	prom := appObjective()
	prom.Source = SourcePrometheus
	prom.BadQuery, prom.EligibleQuery = "bad[$window]", "eligible[$window]"
	covered := prom
	covered.CoverageQuery = "min_over_time(timestamp(eligible)[$window:1m])"
	proxy := appObjective()
	proxy.Kind, proxy.Source, proxy.Proxy = KindProxy, SourceProxy, "db_latency"
	for name, c := range map[string]struct {
		o    Objective
		want bool
	}{"prometheus": {prom, true}, "covered": {covered, false},
		"push": {appObjective(), false}, "proxy": {proxy, false}} {
		st := Evaluate(c.o, DefaultRules(), windowsAt(allBurning(0.1)),
			known(30*24*time.Hour, 0.1), now)
		if st.CoverageUnverified != c.want {
			t.Errorf("%s: coverage unverified = %v, want %v", name,
				st.CoverageUnverified, c.want)
		}
		raw, _ := json.Marshal(st)
		if !strings.Contains(string(raw), `"coverage_unverified":`) {
			t.Errorf("%s: status json lacks coverage_unverified: %s", name, raw)
		}
	}
}

// The coverage query changes how a window is measured, not what the SLI
// counts: it does not change the definition hash (history survives).
func TestObjective_CoverageQueryKeepsHash(t *testing.T) {
	o := promCoverageObjective()
	bare := o
	bare.CoverageQuery = ""
	if o.Hash() != bare.Hash() {
		t.Fatal("adding a coverage query changed the definition hash")
	}
}
