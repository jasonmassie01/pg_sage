package slo

import (
	"math"
	"time"
)

// Window is the SLI over one evaluation window: bad and eligible event
// counts, counter resets seen, the share of the window the data covers
// and the newest data point. Unknown names why the window cannot be used
// ("" when it can).
type Window struct {
	Duration time.Duration
	Bad      float64
	Eligible float64
	Resets   int
	Coverage float64
	LatestAt time.Time
	Unknown  string
}

// Known reports whether the window can be used.
func (w Window) Known() bool { return w.Unknown == "" && w.Eligible > 0 }

// BurnRate is (bad/eligible) / (1 - target): 1 spends the error budget
// exactly over the SLO window. ok is false for an unknown window.
func (w Window) BurnRate(target float64) (float64, bool) {
	if !w.Known() || !(target > 0 && target < 1) {
		return 0, false
	}
	return (w.Bad / w.Eligible) / (1 - target), true
}

// classify applies the objective's traffic floor and value sanity to a
// window whose source found data.
func classify(w Window, o Objective) Window {
	if w.Unknown != "" {
		return w
	}
	switch {
	case math.IsNaN(w.Bad) || math.IsNaN(w.Eligible) || w.Bad < 0 || w.Bad > w.Eligible:
		w.Unknown = ReasonInvalidValue
	case w.Eligible == 0:
		w.Unknown = ReasonZeroEligible
	case w.Eligible < o.MinEligible:
		w.Unknown = ReasonLowTraffic
	}
	return w
}

// SeriesAgg is one counter series over a window: the samples it had,
// its first and last sample times, the summed increases of bad and
// eligible events, and the counter resets among them.
type SeriesAgg struct {
	Series   string
	Samples  int
	First    time.Time
	Last     time.Time
	Bad      float64
	Eligible float64
	Resets   int
}

// Combine sums stored counter series into the window [from, to]: absent
// data, a newest sample older than the objective's stale limit, or data
// covering under 90% of the window is unknown; so are low traffic and a
// zero denominator.
func Combine(aggs []SeriesAgg, o Objective, from, to, now time.Time) Window {
	w := Window{Duration: to.Sub(from)}
	var earliest time.Time
	samples := 0
	for _, a := range aggs {
		if a.Samples == 0 {
			continue
		}
		samples += a.Samples
		w.Bad += a.Bad
		w.Eligible += a.Eligible
		w.Resets += a.Resets
		if a.Last.After(w.LatestAt) {
			w.LatestAt = a.Last
		}
		if earliest.IsZero() || a.First.Before(earliest) {
			earliest = a.First
		}
	}
	if samples == 0 {
		w.Unknown = ReasonNoData
		return w
	}
	if w.Duration > 0 {
		w.Coverage = math.Min(1, float64(w.LatestAt.Sub(earliest))/float64(w.Duration))
	}
	switch {
	case now.Sub(w.LatestAt) > o.staleAfter():
		w.Unknown = ReasonStale
	case w.Coverage < MinCoverage:
		w.Unknown = ReasonPartialWindow
	}
	return classify(w, o)
}
