package forecaster

import "math"

// DataPoint is a single (x, y) observation for regression.
type DataPoint struct {
	X float64 // day offset from earliest sample
	Y float64 // metric value
}

// RegressionResult holds linear regression output.
type RegressionResult struct {
	Slope     float64
	Intercept float64
	R2        float64 // coefficient of determination
}

// LinearRegression computes ordinary least squares on the given points.
// Returns a zero result if fewer than 2 points or zero variance in Y.
func LinearRegression(points []DataPoint) RegressionResult {
	n := float64(len(points))
	if n < 2 {
		return RegressionResult{}
	}

	var sumX, sumY, sumXY, sumX2 float64
	for _, p := range points {
		sumX += p.X
		sumY += p.Y
		sumXY += p.X * p.Y
		sumX2 += p.X * p.X
	}

	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return RegressionResult{}
	}

	slope := (n*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / n

	// Coefficient of determination (R²).
	meanY := sumY / n
	var ssTot, ssRes float64
	for _, p := range points {
		diff := p.Y - meanY
		ssTot += diff * diff
		residual := p.Y - (slope*p.X + intercept)
		ssRes += residual * residual
	}

	var r2 float64
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}

	return RegressionResult{
		Slope:     slope,
		Intercept: intercept,
		R2:        r2,
	}
}

// EWMA computes the exponentially weighted moving average and returns
// the final smoothed value. Alpha controls the decay (0 < alpha <= 1).
// Returns 0 for an empty slice.
func EWMA(values []float64, alpha float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := values[0]
	for i := 1; i < len(values); i++ {
		s = alpha*values[i] + (1-alpha)*s
	}
	return s
}

// DaysUntilThreshold estimates how many days until a linearly growing
// metric reaches the given threshold.
// Returns +Inf when slope <= 0 (not growing) and 0 when already exceeded.
func DaysUntilThreshold(
	current, slope, threshold float64,
) float64 {
	if current >= threshold {
		return 0
	}
	if slope <= 0 {
		return math.Inf(1)
	}
	return (threshold - current) / slope
}
