package forecaster

import (
	"math"
	"testing"
)

// forecast-08: the projection rules gate on the fit and the span of the
// series, and query volume compares two complete calendar weeks.

// No concurrent access tests here: the rules are pure functions of their
// arguments; connection_limit_db_test.go runs two Forecast calls at once.

// noisyPeaks returns n daily peaks rising `slope` per day plus a fixed
// zig-zag of amplitude `noise`, so the fit's R² is set by the noise.
func noisyPeaks(n int, base, slope, noise float64) []float64 {
	pattern := []float64{1, -1, 0.5, -0.5, 1, 0, -1}
	out := make([]float64, n)
	for i := range out {
		out[i] = base + slope*float64(i) + noise*pattern[i%len(pattern)]
	}
	return out
}

func r2Of(values []float64) float64 {
	pts := make([]DataPoint, len(values))
	for i, v := range values {
		pts[i] = DataPoint{X: float64(i), Y: v}
	}
	return LinearRegression(pts).R2
}

// connPeaks is a connection history of the given daily peaks.
func connPeaks(peaks []float64) []DaySystemAgg {
	return makeSysAggs(len(peaks), func(i int) DaySystemAgg {
		return DaySystemAgg{MaxTotalBackends: peaks[i], MaxConnections: 200}
	})
}

// Invalid input: noisy connection peaks (R² about 0.2) with a positive
// slope give no finding; the same slope at R² about 0.9 does.
func TestConnectionGate_NoisyPeaksGiveNoFinding(t *testing.T) {
	cfg := ForecasterConfig{ConnectionWarnPct: 80, MinRSquared: 0.5}
	noisy := noisyPeaks(14, 100, 2, 15)
	clean := noisyPeaks(14, 100, 2, 3.2)
	if r := r2Of(noisy); r < 0.1 || r > 0.3 {
		t.Fatalf("noisy fixture R² = %.2f, want about 0.2", r)
	}
	if r := r2Of(clean); r < 0.85 || r > 0.95 {
		t.Fatalf("clean fixture R² = %.2f, want about 0.9", r)
	}
	if f := forecastConnectionSaturation(connPeaks(noisy), cfg); len(f) != 0 {
		t.Fatalf("noisy peaks projected: %+v", f)
	}
	f := forecastConnectionSaturation(connPeaks(clean), cfg)
	if len(f) != 1 {
		t.Fatalf("clean peaks: %d findings, want 1", len(f))
	}
	d := f[0].Detail
	if r := d["r_squared"].(float64); r < 0.85 {
		t.Errorf("r_squared = %v", r)
	}
	if d["span_days"] != 14 || d["covered_days"] != 14 {
		t.Errorf("span/covered = %v/%v, want 14/14", d["span_days"], d["covered_days"])
	}
}

// The R² gate reads forecaster.min_r_squared; 0 is the default 0.5.
func TestConnectionGate_MinRSquaredIsConfigurable(t *testing.T) {
	peaks := noisyPeaks(14, 100, 2, 3.5) // R² about 0.9
	if f := forecastConnectionSaturation(connPeaks(peaks),
		ForecasterConfig{ConnectionWarnPct: 80, MinRSquared: 0.95}); len(f) != 0 {
		t.Fatalf("R² 0.9 passed a 0.95 gate: %+v", f)
	}
	noisy := noisyPeaks(14, 100, 2, 28)
	if f := forecastConnectionSaturation(connPeaks(noisy),
		ForecasterConfig{ConnectionWarnPct: 80}); len(f) != 0 {
		t.Fatalf("an unset min_r_squared let R² 0.2 through: %+v", f)
	}
}

// The disk growth rule gates the same way.
func TestDiskGate_NoisyGrowthGivesNoFinding(t *testing.T) {
	sizes := noisyPeaks(14, 100, 8, 60) // GB, 8 GB/day under heavy noise
	aggs := makeSysAggs(14, func(i int) DaySystemAgg {
		return DaySystemAgg{MaxDBSizeBytes: sizes[i] * 1e9, AvgDBSizeBytes: sizes[i] * 1e9}
	})
	if r := r2Of(sizes); r > 0.4 {
		t.Fatalf("fixture R² = %.2f, want noisy", r)
	}
	if f := forecastDiskGrowth(aggs, ForecasterConfig{DiskWarnGrowthGBDay: 5}); len(f) != 0 {
		t.Fatalf("noisy disk growth reported: %+v", f)
	}
}

// Nil/empty: 6 calendar days of span gives no projection.
func TestProjectionGate_SixDaySpanGivesNoProjection(t *testing.T) {
	cfg := ForecasterConfig{ConnectionWarnPct: 80, DiskWarnGrowthGBDay: 1}
	aggs := makeSysAggs(6, func(i int) DaySystemAgg {
		return DaySystemAgg{MaxTotalBackends: float64(100 + 10*i), MaxConnections: 200,
			MaxDBSizeBytes: float64(100+10*i) * 1e9}
	})
	if f := forecastConnectionSaturation(aggs, cfg); len(f) != 0 {
		t.Errorf("6-day span projected connections: %+v", f)
	}
	if f := forecastDiskGrowth(aggs, cfg); len(f) != 0 {
		t.Errorf("6-day span projected disk: %+v", f)
	}
	if f := forecastConnectionSaturation(nil, cfg); len(f) != 0 {
		t.Errorf("nil history projected: %+v", f)
	}
}

// Boundary: seven known days spread over a gap span more than 7 days and
// project; the detail tells covered from spanned days.
func TestProjectionGate_SpanCountsCalendarDays(t *testing.T) {
	aggs := makeSysAggs(9, func(i int) DaySystemAgg {
		return DaySystemAgg{MaxTotalBackends: float64(100 + 5*i), MaxConnections: 200}
	})
	aggs = append(aggs[:3:3], aggs[5:]...) // two days without rows
	f := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(f) != 1 {
		t.Fatalf("findings = %d, want 1", len(f))
	}
	if f[0].Detail["span_days"] != 9 || f[0].Detail["covered_days"] != 7 {
		t.Errorf("span/covered = %v/%v, want 9/7", f[0].Detail["span_days"],
			f[0].Detail["covered_days"])
	}
}

// The connection title's projected date: dates_test.go
// (TestConnectionTitle_ProjectedDateIsTheSessionDate).

// Boundary (index-based WoW): an outage removes day 9 of 16; the array
// shifts by one, so the old "last 7 entries vs the 7 before" read a week
// of normal traffic against one that held a slow day and reported growth.
func TestCalendarWoW_MissingDayGivesNoFinding(t *testing.T) {
	q := makeQueryAggs(16, func(i int) float64 {
		switch {
		case i < 4:
			return 10 // a fresh install's first days
		default:
			return 1000
		}
	})
	q = append(q[:9:9], q[10:]...)
	if f := forecastQueryVolume(q, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("a missing day produced %+v", f)
	}
}

// Happy path: a steady 10%/week rise over 3 complete weeks gives about
// 10% WoW and no critical.
func TestCalendarWoW_SteadyTenPercentPerWeek(t *testing.T) {
	daily := math.Pow(1.1, 1.0/7)
	q := makeQueryAggs(21, func(i int) float64 { return 1000 * math.Pow(daily, float64(i)) })
	for i := range q {
		q[i].Day = calToday.AddDate(0, 0, i-21)
	}
	days := closedQueryDays(q, calToday, 0)
	w, ok := weekOverWeek(days)
	if !ok || math.Abs(w.pct-10) > 1e-6 {
		t.Fatalf("wow = %+v (%v), want about 10%%", w, ok)
	}
	if f := forecastQueryVolume(days, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("10%%/week flagged: %+v", f)
	}
}

// Error propagation does not apply to the rules: they take values, not
// readers; Forecast's read failures are covered by evaluated_test.go.
