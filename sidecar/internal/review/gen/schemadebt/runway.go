package schemadebt

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// RateNotMeasured is said instead of a date when the series has fewer
// than MinSamples samples (decision 9 of the plan: never a guess).
const RateNotMeasured = "rate not measured yet"

// MinSamples is the fewest samples a rate is fitted from.
const MinSamples = 3

// errorSpread is how many standard errors of the fitted rate the stated
// range spans on each side.
const errorSpread = 2

// Runway is a series' least-squares fit against a limit. Measured is
// false below MinSamples finite samples or when they share one instant.
// Days is the whole days until the limit, 0 at or past it, -1 with no
// date (not measured, or a rate at or below zero). DaysLow and DaysHigh
// are the days at the rate plus and minus errorSpread standard errors
// (DaysHigh is +Inf when that rate is not positive). Remaining and Limit
// are in the series' rising direction.
type Runway struct {
	Measured          bool
	Descending        bool
	Samples           int
	RatePerDay        float64
	Remaining, Limit  float64
	Days              int
	DaysLow, DaysHigh float64
	From, To          time.Time
}

// Fit fits s against the smaller of its own limit and limit (the column's
// type limit, in the series' rising direction). descending says the
// sequence counts down, so the statement names its minimum.
func Fit(s Series, limit float64, descending bool) Runway {
	pts := finitePoints(s.Points)
	r := Runway{Descending: descending, Samples: len(pts), Days: -1, Limit: limit}
	if len(s.Points) > 0 && isFinite(s.Limit) && s.Limit < limit {
		r.Limit = s.Limit
	}
	if len(pts) < MinSamples || !pts[len(pts)-1].At.After(pts[0].At) {
		return r
	}
	slope, se := leastSquares(pts)
	r.Measured, r.RatePerDay = true, slope
	r.From, r.To = pts[0].At, pts[len(pts)-1].At
	r.Remaining = math.Max(r.Limit-pts[len(pts)-1].Value, 0)
	switch {
	case r.Remaining == 0:
		r.Days, r.DaysLow, r.DaysHigh = 0, 0, 0
	case slope > 0:
		r.Days = int(math.Floor(r.Remaining / slope))
		r.DaysLow = r.Remaining / (slope + errorSpread*se)
		r.DaysHigh = math.Inf(1)
		if low := slope - errorSpread*se; low > 0 {
			r.DaysHigh = r.Remaining / low
		}
	}
	return r
}

// finitePoints is the finite points, oldest first.
func finitePoints(in []Point) []Point {
	out := make([]Point, 0, len(in))
	for _, p := range in {
		if isFinite(p.Value) && !p.At.IsZero() {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// leastSquares is the slope per day of value over time and its standard
// error. Values and times are centered first, so whole-day steps fit
// exactly.
func leastSquares(pts []Point) (slope, se float64) {
	n := float64(len(pts))
	xs := make([]float64, len(pts))
	var mx, my float64
	for i, p := range pts {
		xs[i] = p.At.Sub(pts[0].At).Hours() / 24
		mx += xs[i]
		my += p.Value - pts[0].Value
	}
	mx, my = mx/n, my/n
	var sxx, sxy float64
	for i, p := range pts {
		dx := xs[i] - mx
		sxx += dx * dx
		sxy += dx * (p.Value - pts[0].Value - my)
	}
	slope = sxy / sxx
	var ssr float64
	for i, p := range pts {
		e := p.Value - pts[0].Value - my - slope*(xs[i]-mx)
		ssr += e * e
	}
	if len(pts) > 2 {
		se = math.Sqrt(ssr / (n - 2) / sxx)
	}
	return slope, se
}

// Statement is the runway as a sentence: the days with their range, the
// rate and the samples it rests on, and the limit; or RateNotMeasured.
func (r Runway) Statement() string {
	if !r.Measured {
		return fmt.Sprintf("%s (%d runway samples; at least %d at different times are "+
			"needed)", RateNotMeasured, r.Samples, MinSamples)
	}
	basis := fmt.Sprintf("%s values a day, %d samples from %s to %s", num(r.RatePerDay),
		r.Samples, r.From.Format(time.DateOnly), r.To.Format(time.DateOnly))
	switch {
	case r.Remaining == 0:
		return fmt.Sprintf("exhausted: at or past the %s, at %s", r.limitText(), basis)
	case r.Days < 0:
		return fmt.Sprintf("not consuming values toward the %s: the fitted rate is %s",
			r.limitText(), basis)
	}
	high := "unbounded"
	if !math.IsInf(r.DaysHigh, 1) {
		high = strconv.Itoa(int(math.Floor(r.DaysHigh)))
	}
	return fmt.Sprintf("exhausts in %d days (%d standard errors: %d to %s days) at %s, "+
		"toward the %s", r.Days, errorSpread, int(math.Floor(r.DaysLow)), high, basis,
		r.limitText())
}

// limitText names the limit as the sequence sees it.
func (r Runway) limitText() string {
	if r.Descending {
		return "minimum " + num(-r.Limit)
	}
	return "maximum " + num(r.Limit)
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
