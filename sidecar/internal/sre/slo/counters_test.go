package slo

import (
	"testing"
	"time"
)

// Counter windows from stored samples (pushed app counters and the
// database proxies): per-series increases are summed; resets are counted
// (the increase after a reset restarts from zero, as in Prometheus);
// absent, stale, partial and low-traffic windows are unknown.

func agg(series string, first, last time.Time, bad, eligible float64, resets int) SeriesAgg {
	return SeriesAgg{Series: series, Samples: 10, First: first, Last: last, Bad: bad,
		Eligible: eligible, Resets: resets}
}

func TestCombine_SumsSeries(t *testing.T) {
	o := appObjective()
	from := now.Add(-time.Hour)
	w := Combine([]SeriesAgg{
		agg("pod-a", from.Add(-30*time.Second), now.Add(-10*time.Second), 5, 600, 0),
		agg("pod-b", from.Add(-20*time.Second), now.Add(-5*time.Second), 7, 900, 1),
	}, o, from, now, now)
	if w.Unknown != "" || w.Bad != 12 || w.Eligible != 1500 || w.Resets != 1 ||
		w.Duration != time.Hour || !w.LatestAt.Equal(now.Add(-5*time.Second)) {
		t.Fatalf("window = %+v", w)
	}
	if w.Coverage < 0.99 || w.Coverage > 1 {
		t.Fatalf("coverage = %v, want ~1 (clamped)", w.Coverage)
	}
}

func TestCombine_Unknowns(t *testing.T) {
	o := appObjective()
	from := now.Add(-time.Hour)
	full := func(bad, eligible float64) SeriesAgg {
		return agg("s", from.Add(-time.Minute), now.Add(-time.Second), bad, eligible, 0)
	}
	cases := map[string]struct {
		aggs []SeriesAgg
		want string
	}{
		"no series":    {nil, ReasonNoData},
		"no samples":   {[]SeriesAgg{{Series: "s"}}, ReasonNoData},
		"stale": {[]SeriesAgg{agg("s", from, now.Add(-6*time.Minute), 1, 500, 0)},
			ReasonStale},
		"partial": {[]SeriesAgg{agg("s", now.Add(-20*time.Minute), now, 1, 500, 0)},
			ReasonPartialWindow},
		"zero":         {[]SeriesAgg{full(0, 0)}, ReasonZeroEligible},
		"low traffic":  {[]SeriesAgg{full(0, 99)}, ReasonLowTraffic},
		"bad>eligible": {[]SeriesAgg{full(101, 100)}, ReasonInvalidValue},
	}
	for name, c := range cases {
		w := Combine(c.aggs, o, from, now, now)
		if w.Unknown != c.want {
			t.Errorf("%s: unknown = %q, want %q (%+v)", name, w.Unknown, c.want, w)
		}
		if _, ok := w.BurnRate(o.Target); ok {
			t.Errorf("%s: an unknown window has a burn rate", name)
		}
	}
}

// Boundaries: exactly MinEligible events and exactly the stale limit are
// known; exactly 90% coverage is known, just under is partial.
func TestCombine_Boundaries(t *testing.T) {
	o := appObjective()
	from := now.Add(-time.Hour)
	// Coverage (55m of 60m) is 0.92, the latest sample is exactly 5m old
	// and there are exactly MinEligible events: known.
	w := Combine([]SeriesAgg{agg("s", from, now.Add(-5*time.Minute), 0, 100, 0)}, o,
		from, now, now)
	if w.Unknown != "" || w.Eligible != 100 {
		t.Fatalf("exactly 5m old and exactly 100 events: %+v", w)
	}
	at90 := Combine([]SeriesAgg{agg("s", now.Add(-54*time.Minute), now, 0, 500, 0)}, o,
		from, now, now)
	under := Combine([]SeriesAgg{agg("s", now.Add(-53*time.Minute), now, 0, 500, 0)}, o,
		from, now, now)
	if at90.Unknown != "" || under.Unknown != ReasonPartialWindow {
		t.Fatalf("coverage 0.9 -> %q, 0.883 -> %q", at90.Unknown, under.Unknown)
	}
}

// A zero StaleAfter means the default (5 minutes), not "always stale".
func TestCombine_DefaultStaleAfter(t *testing.T) {
	o := appObjective()
	o.StaleAfter = 0
	from := now.Add(-time.Hour)
	fresh := Combine([]SeriesAgg{agg("s", from, now.Add(-4*time.Minute), 0, 500, 0)}, o,
		from, now, now)
	stale := Combine([]SeriesAgg{agg("s", from, now.Add(-6*time.Minute), 0, 500, 0)}, o,
		from, now, now)
	if fresh.Unknown == ReasonStale || stale.Unknown != ReasonStale {
		t.Fatalf("fresh %q stale %q", fresh.Unknown, stale.Unknown)
	}
	if DefaultStaleAfter != 5*time.Minute {
		t.Fatalf("DefaultStaleAfter = %s", DefaultStaleAfter)
	}
}
