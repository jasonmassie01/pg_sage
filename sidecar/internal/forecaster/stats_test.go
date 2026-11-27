package forecaster

import (
	"math"
	"testing"
)

func TestLinearRegression(t *testing.T) {
	tests := []struct {
		name      string
		points    []DataPoint
		wantSlope float64
		wantIntc  float64
		wantR2    float64
		slopeTol  float64
		intcTol   float64
		r2Min     float64
	}{
		{
			name: "perfect line y=2x+1",
			points: []DataPoint{
				{0, 1}, {1, 3}, {2, 5}, {3, 7}, {4, 9},
			},
			wantSlope: 2, wantIntc: 1, wantR2: 1.0,
			slopeTol: 1e-9, intcTol: 1e-9, r2Min: 1.0,
		},
		{
			name: "flat line y=5 (SStot=0)",
			points: []DataPoint{
				{0, 5}, {1, 5}, {2, 5}, {3, 5},
			},
			wantSlope: 0, wantIntc: 5, wantR2: 0,
			slopeTol: 1e-9, intcTol: 1e-9, r2Min: 0,
		},
		{
			name: "negative slope y=-x+10",
			points: []DataPoint{
				{0, 10}, {1, 9}, {2, 8}, {3, 7}, {4, 6},
			},
			wantSlope: -1, wantIntc: 10, wantR2: 1.0,
			slopeTol: 1e-9, intcTol: 1e-9, r2Min: 1.0,
		},
		{
			name:      "single point returns zero",
			points:    []DataPoint{{1, 1}},
			wantSlope: 0, wantIntc: 0, wantR2: 0,
			slopeTol: 1e-9, intcTol: 1e-9, r2Min: 0,
		},
		{
			name:      "empty returns zero",
			points:    nil,
			wantSlope: 0, wantIntc: 0, wantR2: 0,
			slopeTol: 1e-9, intcTol: 1e-9, r2Min: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := LinearRegression(tt.points)
			if math.Abs(r.Slope-tt.wantSlope) > tt.slopeTol {
				t.Errorf(
					"slope = %f, want %f",
					r.Slope, tt.wantSlope,
				)
			}
			if math.Abs(r.Intercept-tt.wantIntc) > tt.intcTol {
				t.Errorf(
					"intercept = %f, want %f",
					r.Intercept, tt.wantIntc,
				)
			}
			if r.R2 < tt.r2Min {
				t.Errorf("R2 = %f, want >= %f", r.R2, tt.r2Min)
			}
		})
	}
}

func TestLinearRegression_NoisyData(t *testing.T) {
	// y = 3x + 2 with small offsets
	points := []DataPoint{
		{0, 2.1}, {1, 5.0}, {2, 7.9}, {3, 11.1},
		{4, 14.0}, {5, 16.9}, {6, 20.1}, {7, 23.0},
	}
	r := LinearRegression(points)
	if r.R2 < 0.8 {
		t.Errorf("R2 = %f, want > 0.8 for noisy data", r.R2)
	}
	if math.Abs(r.Slope-3.0) > 0.5 {
		t.Errorf("slope = %f, want ~3.0", r.Slope)
	}
}

func TestEWMA(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		alpha  float64
		wantLo float64
		wantHi float64
	}{
		{
			name:   "constant series",
			values: []float64{5, 5, 5, 5},
			alpha:  0.3,
			wantLo: 5, wantHi: 5,
		},
		{
			name:   "step change",
			values: []float64{0, 0, 0, 10, 10, 10},
			alpha:  0.3,
			wantLo: 6, wantHi: 9,
		},
		{
			name:   "single value",
			values: []float64{7},
			alpha:  0.3,
			wantLo: 7, wantHi: 7,
		},
		{
			name:   "empty",
			values: nil,
			alpha:  0.3,
			wantLo: 0, wantHi: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EWMA(tt.values, tt.alpha)
			if got < tt.wantLo || got > tt.wantHi {
				t.Errorf(
					"EWMA = %f, want [%f, %f]",
					got, tt.wantLo, tt.wantHi,
				)
			}
		})
	}
}

func TestDaysUntilThreshold(t *testing.T) {
	tests := []struct {
		name      string
		current   float64
		slope     float64
		threshold float64
		want      float64
	}{
		{
			name: "growing", current: 50,
			slope: 2, threshold: 100, want: 25,
		},
		{
			name: "already exceeded", current: 110,
			slope: 2, threshold: 100, want: 0,
		},
		{
			name: "shrinking", current: 50,
			slope: -1, threshold: 100,
			want: math.Inf(1),
		},
		{
			name: "zero slope", current: 50,
			slope: 0, threshold: 100,
			want: math.Inf(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DaysUntilThreshold(
				tt.current, tt.slope, tt.threshold,
			)
			if math.IsInf(tt.want, 1) {
				if !math.IsInf(got, 1) {
					t.Errorf("got %f, want +Inf", got)
				}
				return
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("got %f, want %f", got, tt.want)
			}
		})
	}
}
