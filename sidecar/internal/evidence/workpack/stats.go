package workpack

import (
	"math"

	"github.com/pg-sage/sidecar/internal/forecaster"
)

// Trends of a fitted series.
const (
	trendUnknown = "unknown" // fewer than minTrendPoints points
	trendFlat    = "flat"    // the slope is within two standard errors of zero
	trendRising  = "rising"
	trendFalling = "falling"
)

// minTrendPoints is the fewest points a slope is stated from.
const minTrendPoints = 3

// Fit is a least-squares line through a series whose x is the point's
// index (a missing point keeps its x): the slope per step, R2 and the
// slope's standard error, sqrt(SSR / (n - 2)) / sqrt(Sxx). The forecaster
// computes slope and R2; its result has no standard error.
type Fit struct {
	Points  int      `json:"points"`
	Slope   *float64 `json:"slope"`
	R2      *float64 `json:"r2"`
	SlopeSE *float64 `json:"slope_se"`
	Trend   string   `json:"trend"`
}

// fit fits values; nil entries are missing points.
func fit(values []*float64) Fit {
	var pts []forecaster.DataPoint
	for i, v := range values {
		if v != nil {
			pts = append(pts, forecaster.DataPoint{X: float64(i), Y: *v})
		}
	}
	out := Fit{Points: len(pts), Trend: trendUnknown}
	if len(pts) < minTrendPoints {
		return out
	}
	reg := forecaster.LinearRegression(pts)
	if noiseSlope(pts, reg.Slope) {
		reg = flatLine(pts)
	}
	se := slopeSE(pts, reg)
	slope, r2 := round12(reg.Slope), round12(reg.R2)
	out.Slope, out.R2, out.SlopeSE = &slope, &r2, &se
	switch {
	case slope == 0 || math.Abs(slope) <= 2*se:
		out.Trend = trendFlat
	case slope > 0:
		out.Trend = trendRising
	default:
		out.Trend = trendFalling
	}
	return out
}

// noiseSlope reports a slope whose whole rise over the series is float
// noise against its values (a constant series of non-integers fits a
// slope of about 1e-15): it is stated as 0, so the trend is flat.
func noiseSlope(pts []forecaster.DataPoint, slope float64) bool {
	scale := 1.0
	for _, p := range pts {
		scale = math.Max(scale, math.Abs(p.Y))
	}
	span := pts[len(pts)-1].X - pts[0].X
	return math.Abs(slope)*span <= 1e-9*scale
}

// flatLine is the zero-slope fit of pts: the mean, R2 0.
func flatLine(pts []forecaster.DataPoint) forecaster.RegressionResult {
	var sum float64
	for _, p := range pts {
		sum += p.Y
	}
	return forecaster.RegressionResult{Intercept: sum / float64(len(pts))}
}

// slopeSE is the standard error of reg's slope over pts (at least 3).
func slopeSE(pts []forecaster.DataPoint, reg forecaster.RegressionResult) float64 {
	n := float64(len(pts))
	var meanX float64
	for _, p := range pts {
		meanX += p.X
	}
	meanX /= n
	var ssr, sxx float64
	for _, p := range pts {
		r := p.Y - (reg.Slope*p.X + reg.Intercept)
		if math.Abs(r) <= 1e-9*math.Max(1, math.Abs(p.Y)) {
			r = 0 // float noise of an exact line, not a residual
		}
		ssr += r * r
		sxx += (p.X - meanX) * (p.X - meanX)
	}
	if sxx == 0 {
		return 0
	}
	return round12(math.Sqrt(ssr/(n-2)) / math.Sqrt(sxx))
}
