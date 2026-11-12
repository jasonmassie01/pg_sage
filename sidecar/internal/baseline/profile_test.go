package baseline

import (
	"math"
	"testing"
	"time"
)

// market-02: hour-of-week buckets and robust scoring. The functions are
// pure and keep no state.
// No concurrent access tests here: Window, Clean and Judge share nothing;
// the store's concurrency is covered in store_db_test.go.

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	return loc
}

// Monday 00:00 is bucket 0 and Sunday 23:00 bucket 167, in the window's
// time zone, not UTC.
func TestBaselineWindowBucket_MondayMidnightIsZeroInTheConfiguredZone(t *testing.T) {
	utc := Window{Loc: time.UTC, Weeks: 4}
	cases := []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), 0},      // Monday
		{time.Date(2026, 11, 9, 9, 30, 0, 0, time.UTC), 9},     // Monday 09:30
		{time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 33},    // Tuesday 09:00
		{time.Date(2026, 11, 15, 23, 59, 0, 0, time.UTC), 167}, // Sunday 23:59
	}
	for _, c := range cases {
		if got := utc.Bucket(c.at); got != c.want {
			t.Errorf("Bucket(%s) = %d, want %d", c.at, got, c.want)
		}
	}
	// 2026-11-09 03:00 UTC is Sunday 21:00 in Chicago (CST, UTC-6).
	chi := Window{Loc: chicago(t), Weeks: 4}
	if got := chi.Bucket(time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)); got != 6*24+21 {
		t.Fatalf("Chicago bucket = %d, want Sunday 21:00 (%d)", got, 6*24+21)
	}
}

// State transition (DST): 09:00 Monday local is bucket 9 on both sides of
// the 2026-03-08 spring-forward in America/Chicago, although its UTC hour
// moves from 15:00 to 14:00. The week starts on the local Monday.
func TestBaselineWindowBucket_DSTShiftKeepsLocalWallClockBucket(t *testing.T) {
	w := Window{Loc: chicago(t), Weeks: 4}
	before := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC) // CST: 09:00
	after := time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)  // CDT: 09:00
	if b, a := w.Bucket(before), w.Bucket(after); b != 9 || a != 9 {
		t.Fatalf("buckets across DST = %d, %d, want 9 and 9", b, a)
	}
	// The same UTC hour lands one local hour later after the shift.
	if got := w.Bucket(time.Date(2026, 3, 9, 15, 0, 0, 0, time.UTC)); got != 10 {
		t.Fatalf("15:00 UTC after DST = bucket %d, want 10", got)
	}
	wantBefore := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	wantAfter := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	if got := w.WeekStart(before); !got.Equal(wantBefore) {
		t.Errorf("WeekStart(before) = %s, want %s", got, wantBefore)
	}
	if got := w.WeekStart(after); !got.Equal(wantAfter) {
		t.Errorf("WeekStart(after) = %s, want %s", got, wantAfter)
	}
	// Sunday 23:30 local is still the previous week (Monday 2026-03-02).
	sunday := time.Date(2026, 3, 9, 4, 30, 0, 0, time.UTC) // Sun 23:30 CDT
	if got := w.WeekStart(sunday); !got.Equal(wantBefore) || w.Bucket(sunday) != 167 {
		t.Fatalf("Sunday 23:30 = week %s bucket %d, want %s bucket 167",
			got, w.Bucket(sunday), wantBefore)
	}
}

// Nil/zero: a window without a zone is UTC and without weeks uses
// DefaultWeeks.
func TestBaselineWindow_ZeroValueIsUTCAndDefaultWeeks(t *testing.T) {
	var w Window
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.FixedZone("x", 5*3600))
	if got := w.Bucket(at); got != 24+4 {
		t.Fatalf("zero window bucket = %d, want Tuesday 04:00 UTC (28)", got)
	}
	if w.weeks() != DefaultWeeks {
		t.Fatalf("zero window weeks = %d, want %d", w.weeks(), DefaultWeeks)
	}
}

// Boundary: 2 weeks of history is learning, 3 is active.
func TestBaselineJudge_LearningUntilThreeWeeks(t *testing.T) {
	two := Judge([]float64{10, 10}, 100, 5)
	if !two.Learning() || two.Weeks != 2 || two.Z != 0 {
		t.Fatalf("2 weeks = %+v, want learning with z 0", two)
	}
	three := Judge([]float64{10, 11, 9}, 100, 5)
	if three.Learning() || three.Weeks != 3 || three.Median != 10 {
		t.Fatalf("3 weeks = %+v, want active with median 10", three)
	}
	if three.MAD != 1 {
		t.Fatalf("MAD = %v, want 1", three.MAD)
	}
	wantZ := (100.0 - 10) / (1.4826 * 1)
	if math.Abs(three.Z-wantZ) > 1e-9 {
		t.Fatalf("z = %v, want %v", three.Z, wantZ)
	}
}

// Nil/empty: no history is learning with zero weeks and no z.
func TestBaselineJudge_EmptyHistory(t *testing.T) {
	for _, h := range [][]float64{nil, {}} {
		s := Judge(h, 42, 5)
		if !s.Learning() || s.Weeks != 0 || s.Z != 0 || s.Observed != 42 {
			t.Fatalf("Judge(%v) = %+v, want learning, 0 weeks, observed 42", h, s)
		}
	}
}

// MAD = 0 on a constant series: the 5%-of-median floor applies, so z is
// finite and no division by zero happens.
func TestBaselineJudge_ConstantSeriesUsesTheMADFloor(t *testing.T) {
	s := Judge([]float64{20, 20, 20, 20}, 30, 5)
	if s.MAD != 0 || s.Median != 20 {
		t.Fatalf("score = %+v, want median 20, MAD 0", s)
	}
	want := 10 / (1.4826 * 0.05 * 20)
	if math.IsInf(s.Z, 0) || math.IsNaN(s.Z) || math.Abs(s.Z-want) > 1e-9 {
		t.Fatalf("z = %v, want the floored %v", s.Z, want)
	}
	// A zero median and zero MAD fall back to the absolute floor as scale.
	zero := Judge([]float64{0, 0, 0}, 20, 5)
	if zero.Z != 4 {
		t.Fatalf("zero series z = %v, want 20/5 = 4", zero.Z)
	}
	// Without any floor the scale is 1, never 0.
	if z := Judge([]float64{0, 0, 0}, 3, 0).Z; z != 3 {
		t.Fatalf("no-floor z = %v, want 3", z)
	}
}

// Invalid input: NaN and infinite history weeks are dropped before the
// week count, so they cannot make a bucket active or poison the median.
func TestBaselineJudge_DropsNaNHistory(t *testing.T) {
	s := Judge([]float64{math.NaN(), 10, math.Inf(1), 10}, 10, 5)
	if s.Weeks != 2 || !s.Learning() {
		t.Fatalf("score = %+v, want 2 usable weeks (learning)", s)
	}
	nan := Judge([]float64{10, 10, 10}, math.NaN(), 5)
	if nan.Z != 0 || nan.Exceeds(4, 5) {
		t.Fatalf("NaN observation = %+v, want no z and no anomaly", nan)
	}
}

// Boundary: Exceeds needs z at or above the threshold and the absolute
// delta floor, and never fires while learning.
func TestBaselineScoreExceeds_ZAndDeltaFloors(t *testing.T) {
	cases := []struct {
		name  string
		s     Score
		floor float64
		want  bool
	}{
		{"z 3.99", Score{Observed: 100, Median: 10, Z: 3.99, Weeks: 4}, 5, false},
		{"z 4.0", Score{Observed: 100, Median: 10, Z: 4.0, Weeks: 4}, 5, true},
		{"delta below floor", Score{Observed: 14, Median: 10, Z: 9, Weeks: 4}, 5, false},
		{"delta at floor", Score{Observed: 15, Median: 10, Z: 9, Weeks: 4}, 5, true},
		{"learning", Score{Observed: 100, Median: 10, Z: 9, Weeks: 2}, 5, false},
		{"below usual", Score{Observed: 1, Median: 10, Z: -9, Weeks: 4}, 5, false},
	}
	for _, c := range cases {
		if got := c.s.Exceeds(4, c.floor); got != c.want {
			t.Errorf("%s: Exceeds = %v, want %v", c.name, got, c.want)
		}
	}
}

// Invalid input: Clean drops NaN, infinite and negative values and keeps
// the last sample of a repeated key, so one statement upserts one row.
func TestBaselineClean_DropsInvalidValuesAndDuplicateKeys(t *testing.T) {
	k := Key{Metric: "query_mean_ms", Key: "queryid:1"}
	in := []Sample{
		{Key: k, How: "interval_mean", Value: 3},
		{Key: Key{Metric: "m", Key: "nan"}, How: "x", Value: math.NaN()},
		{Key: Key{Metric: "m", Key: "inf"}, How: "x", Value: math.Inf(1)},
		{Key: Key{Metric: "m", Key: "neg"}, How: "x", Value: -1},
		{Key: Key{Metric: "", Key: "nokey"}, How: "x", Value: 1},
		{Key: k, How: "interval_mean", Value: 7},
		{Key: Key{Metric: "active_backends", Key: "database"}, How: "gauge", Value: 0},
	}
	got := Clean(in)
	if len(got) != 2 {
		t.Fatalf("Clean = %+v, want the query (last value) and the zero gauge", got)
	}
	if got[0].Key != k || got[0].Value != 7 {
		t.Errorf("first = %+v, want %v = 7", got[0], k)
	}
	if got[1].Value != 0 || got[1].Key.Metric != "active_backends" {
		t.Errorf("second = %+v, want the zero gauge kept", got[1])
	}
	if Clean(nil) != nil {
		t.Error("Clean(nil) is not nil")
	}
}
