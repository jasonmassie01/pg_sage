// Package baseline keeps seasonal workload baselines (market-02): each
// cycle's values fold into one of 168 hour-of-week buckets per week, and a
// new value is scored with a robust z against the same bucket in earlier
// weeks. Scoring is deterministic and reads only stored history.
package baseline

import (
	"math"
	"sort"
	"time"
)

// BucketsPerWeek is the number of hour-of-week buckets; Monday 00:00 is 0.
const BucketsPerWeek = 168

// MinWeeks is the fewest weeks of data a bucket needs before it is
// active; with fewer it is learning and never fires.
const MinWeeks = 3

// DefaultWeeks is the scoring window when a Window names none.
const DefaultWeeks = 4

// Method names the scoring in finding details.
const Method = "hour_of_week_mad"

// madScale makes the MAD a consistent estimator of the standard deviation
// of normal data; madFloorFraction floors the MAD at 5% of the median, so
// a near-constant series does not turn a small wobble into a huge z.
const (
	madScale         = 1.4826
	madFloorFraction = 0.05
)

// Window is the bucket time zone and how many earlier weeks a bucket is
// scored against.
type Window struct {
	Loc   *time.Location
	Weeks int
}

func (w Window) location() *time.Location {
	if w.Loc == nil {
		return time.UTC
	}
	return w.Loc
}

func (w Window) weeks() int {
	if w.Weeks <= 0 {
		return DefaultWeeks
	}
	return w.Weeks
}

// Bucket is t's hour of the week in the window's zone, by the local wall
// clock (a DST shift keeps 09:00 Monday in bucket 9).
func (w Window) Bucket(t time.Time) int {
	local := t.In(w.location())
	day := (int(local.Weekday()) + 6) % 7 // Monday 0 .. Sunday 6
	return day*24 + local.Hour()
}

// WeekStart is the date of the local Monday that starts t's week, as
// midnight UTC (the store's date column).
func (w Window) WeekStart(t time.Time) time.Time {
	local := t.In(w.location())
	day := (int(local.Weekday()) + 6) % 7
	y, m, d := local.AddDate(0, 0, -day).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Key identifies one baselined series: a metric and what it is about
// (a statement, the database, the cluster, a wait class).
type Key struct {
	Metric string
	Key    string
}

// Sample is one cycle's value of a series; How names its aggregation.
type Sample struct {
	Key   Key
	How   string
	Value float64
}

// Clean drops samples without a metric or key and with NaN, infinite or
// negative values, and keeps the last sample of a repeated key, in first
// seen order.
func Clean(samples []Sample) []Sample {
	if samples == nil {
		return nil
	}
	at := make(map[Key]int, len(samples))
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if s.Key.Metric == "" || s.Key.Key == "" || !usable(s.Value) || s.Value < 0 {
			continue
		}
		if i, ok := at[s.Key]; ok {
			out[i] = s
			continue
		}
		at[s.Key] = len(out)
		out = append(out, s)
	}
	return out
}

func usable(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Score is an observation against its bucket's weeks.
type Score struct {
	Observed float64
	Median   float64
	MAD      float64
	Z        float64
	Weeks    int
}

// Learning reports a bucket with too few weeks to judge.
func (s Score) Learning() bool { return s.Weeks < MinWeeks }

// Exceeds reports an active bucket whose observation is at least z robust
// deviations and at least floor above the median.
func (s Score) Exceeds(z, floor float64) bool {
	return !s.Learning() && s.Z >= z && s.Observed-s.Median >= floor
}

// Spread is the robust centre and scale of a set of values: the median,
// the MAD, and the scale Judge divides by (1.4826 x the MAD, floored at
// 5% of the median; when both are zero the caller's floor, and 1 without
// one). OK is false with fewer than MinWeeks usable values.
type Spread struct {
	N                  int
	Median, MAD, Scale float64
	OK                 bool
}

// Robust is the Spread of vals; NaN and infinite values are dropped and
// vals is not modified.
func Robust(vals []float64, floor float64) Spread {
	kept := make([]float64, 0, len(vals))
	for _, v := range vals {
		if usable(v) {
			kept = append(kept, v)
		}
	}
	s := Spread{N: len(kept)}
	if s.N < MinWeeks {
		return s
	}
	s.Median = median(kept)
	dev := make([]float64, len(kept))
	for i, v := range kept {
		dev[i] = math.Abs(v - s.Median)
	}
	s.MAD = median(dev)
	s.Scale = madScale * math.Max(s.MAD, madFloorFraction*math.Abs(s.Median))
	if s.Scale <= 0 {
		s.Scale = floor
	}
	if s.Scale <= 0 {
		s.Scale = 1
	}
	s.OK = true
	return s
}

// Judge scores observed against history, one value per earlier week.
// NaN and infinite weeks are dropped; the scale is Robust's.
func Judge(history []float64, observed, floor float64) Score {
	r := Robust(history, floor)
	s := Score{Observed: observed, Weeks: r.N}
	if s.Learning() || !usable(observed) {
		return s
	}
	s.Median, s.MAD = r.Median, r.MAD
	s.Z = (observed - s.Median) / r.Scale
	return s
}

// median of a non-empty slice; vals is sorted in place.
func median(vals []float64) float64 {
	sort.Float64s(vals)
	mid := len(vals) / 2
	if len(vals)%2 == 0 {
		return (vals[mid-1] + vals[mid]) / 2
	}
	return vals[mid]
}
