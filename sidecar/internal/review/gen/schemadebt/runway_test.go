package schemadebt

import (
	"math"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: Fit is a pure function of its arguments.

func TestFit_Int4SequenceAt1_9e9Consuming1e6ADayExhaustsIn247Days(t *testing.T) {
	r := Fit(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max), int4Max, false)
	if !r.Measured || r.Samples != 5 {
		t.Fatalf("runway %+v, want measured over 5 samples", r)
	}
	if r.Days != 247 || math.Abs(r.RatePerDay-1e6) > 1e-6 || r.Remaining != 247483647 {
		t.Fatalf("runway %+v, want 247 days at 1e6 a day with 247483647 left", r)
	}
	if !r.From.Equal(sampleEnd.AddDate(0, 0, -4)) || !r.To.Equal(sampleEnd) {
		t.Fatalf("runway spans %s to %s", r.From, r.To)
	}
	// an exact line has no error: both bounds are the point estimate
	if math.Abs(r.DaysLow-247.483647) > 1e-6 || math.Abs(r.DaysHigh-247.483647) > 1e-6 {
		t.Fatalf("bounds %v-%v, want 247.48 both", r.DaysLow, r.DaysHigh)
	}
	mustContain(t, "statement", r.Statement(), "exhausts in 247 days", "1000000 values a day",
		"5 samples", "2026-10-27", "2026-10-31", "maximum 2147483647")
}

func TestFit_NoisySamplesStateTheirError(t *testing.T) {
	s := linear(ordersSeq, 1.9e9, 1e6, 6, int4Max)
	s.Points[1].Value += 3e5
	s.Points[4].Value -= 2e5
	r := Fit(s, int4Max, false)
	if !r.Measured || !(r.DaysLow < float64(r.Days) && float64(r.Days) < r.DaysHigh) {
		t.Fatalf("runway %+v, want a range around the estimate", r)
	}
	mustContain(t, "statement", r.Statement(), "2 standard errors")
}

func TestFit_FewerThanThreeSamplesIsNotMeasured(t *testing.T) {
	for n := 0; n < 3; n++ {
		r := Fit(linear(ordersSeq, 1.9e9, 1e6, n, int4Max), int4Max, false)
		if r.Measured || r.Samples != n {
			t.Fatalf("%d samples: runway %+v, want not measured", n, r)
		}
		st := r.Statement()
		mustContain(t, "statement", st, RateNotMeasured)
		if strings.Contains(st, "exhausts") || strings.Contains(st, "days") {
			t.Errorf("%d samples: statement %q guesses a date", n, st)
		}
	}
}

func TestFit_SamplesAtOneInstantAreNotMeasured(t *testing.T) {
	s := Series{Subject: ordersSeq, Limit: int4Max}
	for i := 0; i < 4; i++ {
		s.Points = append(s.Points, Point{At: sampleEnd, Value: 1.9e9 + float64(i)})
	}
	if r := Fit(s, int4Max, false); r.Measured {
		t.Fatalf("runway %+v measured from one instant", r)
	}
}

func TestFit_NonFinitePointsAreSkipped(t *testing.T) {
	s := linear(ordersSeq, 1.9e9, 1e6, 3, int4Max)
	s.Points = append(s.Points, Point{At: sampleEnd.Add(time.Hour), Value: math.NaN()},
		Point{At: sampleEnd.Add(2 * time.Hour), Value: math.Inf(1)})
	r := Fit(s, int4Max, false)
	if !r.Measured || r.Samples != 3 || r.Days != 247 {
		t.Fatalf("runway %+v, want the 3 finite samples giving 247 days", r)
	}
}

func TestFit_PointOrderDoesNotMatter(t *testing.T) {
	s := linear(ordersSeq, 1.9e9, 1e6, 5, int4Max)
	s.Points[0], s.Points[4] = s.Points[4], s.Points[0]
	r := Fit(s, int4Max, false)
	if r.Days != 247 || !r.To.Equal(sampleEnd) {
		t.Fatalf("runway %+v, want 247 days to %s", r, sampleEnd)
	}
}

func TestFit_AFallingSeriesHasNoDate(t *testing.T) {
	r := Fit(linear(ordersSeq, 1.9e9, -1e6, 5, int4Max), int4Max, false)
	if !r.Measured || r.RatePerDay >= 0 || r.Days != -1 {
		t.Fatalf("runway %+v, want a measured negative rate and no date", r)
	}
	st := r.Statement()
	mustContain(t, "statement", st, "not consuming")
	if strings.Contains(st, "exhausts") {
		t.Errorf("statement %q dates a falling series", st)
	}
}

func TestFit_AtTheLimitExhaustsToday(t *testing.T) {
	r := Fit(linear(ordersSeq, int4Max, 1e6, 3, int4Max), int4Max, false)
	if r.Days != 0 || r.Remaining != 0 {
		t.Fatalf("runway %+v, want 0 days and 0 values left", r)
	}
}

// The store samples a descending sequence negated, so its series rises
// toward the negated minimum; the statement names the real minimum.
func TestFit_DescendingRunsTowardTheMinimum(t *testing.T) {
	r := Fit(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max+1), int4Max+1, true)
	if r.Days != 247 || r.Remaining != 247483648 {
		t.Fatalf("runway %+v, want 247 days with 247483648 left", r)
	}
	mustContain(t, "statement", r.Statement(), "minimum -2147483648", "exhausts in 247 days")
}

// The binding limit is the smaller of the series' own and the column's.
func TestFit_ColumnLimitBindsBeforeTheSequenceLimit(t *testing.T) {
	r := Fit(linear(ordersSeq, 1.9e9, 1e6, 5, 9.2e18), int4Max, false)
	if r.Days != 247 || r.Limit != int4Max {
		t.Fatalf("runway %+v, want the int4 column's limit to bind at 247 days", r)
	}
	r = Fit(linear(ordersSeq, 1.9e9, 1e6, 5, 2e9), int4Max, false)
	if r.Days != 100 || r.Limit != 2e9 {
		t.Fatalf("runway %+v, want the sequence's MAXVALUE 2e9 to bind at 100 days", r)
	}
}
