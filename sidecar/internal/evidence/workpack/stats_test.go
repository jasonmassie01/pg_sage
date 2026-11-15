package workpack

import (
	"math"
	"testing"
)

// evid-02a: growth states a slope with R2 and the slope's standard error;
// fewer than 3 points is trend unknown. No DB or concurrency test here:
// the fit is a pure function.

func f(v float64) *float64 { return &v }

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestFit_TwoPointsAreUnknownThreeGiveASlope(t *testing.T) {
	two := fit([]*float64{f(1), f(2)})
	if two.Trend != trendUnknown || two.Slope != nil || two.SlopeSE != nil || two.Points != 2 {
		t.Fatalf("two points: %+v, want unknown with no slope", two)
	}
	three := fit([]*float64{f(1), f(2), f(3)})
	if three.Trend != trendRising || three.Slope == nil || !near(*three.Slope, 1) ||
		three.SlopeSE == nil || *three.SlopeSE != 0 || three.R2 == nil || !near(*three.R2, 1) {
		t.Fatalf("three points on a line: %+v, want slope 1, se 0, R2 1, rising", three)
	}
}

func TestFit_StandardErrorMatchesTheHandComputedValue(t *testing.T) {
	// x 0..3, y 1,3,2,5: slope 1.1, residuals -0.1,0.8,-1.3,0.6, SSR 2.7,
	// Sxx 5, se = sqrt(2.7/2)/sqrt(5) = 0.519615...
	got := fit([]*float64{f(1), f(3), f(2), f(5)})
	if got.Slope == nil || !near(*got.Slope, 1.1) || got.SlopeSE == nil ||
		!near(*got.SlopeSE, math.Sqrt(2.7/2)/math.Sqrt(5)) {
		t.Fatalf("fit %+v, want slope 1.1 and se %v", got, math.Sqrt(2.7/2)/math.Sqrt(5))
	}
	if got.Trend != trendRising {
		t.Fatalf("slope 1.1 above two standard errors (%v) is rising, got %s",
			*got.SlopeSE, got.Trend)
	}
	noisy := fit([]*float64{f(1), f(5), f(0), f(4), f(1)})
	if noisy.Trend != trendFlat || noisy.Slope == nil || !near(*noisy.Slope, -0.1) {
		t.Fatalf("noisy series %+v, want flat (slope -0.1 within two standard errors)", noisy)
	}
}

func TestFit_MissingPointsKeepTheirX(t *testing.T) {
	got := fit([]*float64{f(10), nil, f(30), nil, f(50)})
	if got.Points != 3 || got.Slope == nil || !near(*got.Slope, 10) ||
		got.Trend != trendRising {
		t.Fatalf("fit %+v, want 3 points with slope 10 per step (x keeps the gap)", got)
	}
}

func TestFit_EmptyNilAndConstantSeries(t *testing.T) {
	for _, s := range [][]*float64{nil, {}, {nil, nil, nil, nil}} {
		if got := fit(s); got.Trend != trendUnknown || got.Points != 0 {
			t.Fatalf("fit(%v) = %+v, want unknown with no points", s, got)
		}
	}
	flat := fit([]*float64{f(4), f(4), f(4)})
	if flat.Trend != trendFlat || flat.Slope == nil || *flat.Slope != 0 {
		t.Fatalf("constant series %+v, want flat with slope 0", flat)
	}
}

func TestFit_FallingSeries(t *testing.T) {
	if got := fit([]*float64{f(9), f(6), f(3)}); got.Trend != trendFalling {
		t.Fatalf("falling series %+v", got)
	}
}

// A constant series of non-integers fits a float-noise slope (about
// 1e-15, residuals 0): it is flat with slope 0, not rising or falling.
func TestFit_ConstantNonIntegerSeriesIsFlat(t *testing.T) {
	for _, v := range []float64{12.3, 0.1, 1234.567, 7.7, 3.3333} {
		for n := 3; n <= 14; n++ {
			vals := make([]*float64, n)
			for i := range vals {
				vals[i] = f(v)
			}
			got := fit(vals)
			if got.Trend != trendFlat || got.Slope == nil || *got.Slope != 0 ||
				got.SlopeSE == nil || *got.SlopeSE != 0 || got.Points != n {
				t.Fatalf("constant %v x%d: %+v, want flat with slope 0 and se 0", v, n, got)
			}
		}
	}
	// A real rise on a large value is still a slope: 1 TB growing 1 MB a day.
	grow := make([]*float64, 28)
	for i := range grow {
		grow[i] = f(1e12 + float64(i)*1e6)
	}
	if got := fit(grow); got.Trend != trendRising || got.Slope == nil || !near(*got.Slope, 1e6) {
		t.Fatalf("1 MB/day on 1 TB: %+v, want rising at 1e6 per step", got)
	}
}
