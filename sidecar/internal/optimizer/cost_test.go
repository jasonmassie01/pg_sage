package optimizer

import (
	"math"
	"testing"
	"time"
)

func TestEstimateIndexSize(t *testing.T) {
	tests := []struct {
		name          string
		rows          int64
		avgEntryBytes int
		want          int64
	}{
		{
			name:          "known values 1000 rows x 32 bytes",
			rows:          1000,
			avgEntryBytes: 32,
			want:          38400, // 1000 * 32 * 1.2
		},
		{
			name:          "zero rows returns zero",
			rows:          0,
			avgEntryBytes: 32,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateIndexSize(tt.rows, tt.avgEntryBytes)
			if got != tt.want {
				t.Errorf("EstimateIndexSize(%d, %d) = %d, want %d",
					tt.rows, tt.avgEntryBytes, got, tt.want)
			}
		})
	}
}

func TestEstimateBuildTime(t *testing.T) {
	tests := []struct {
		name       string
		tableBytes int64
		want       time.Duration
	}{
		{
			name:       "100MB yields 10 seconds",
			tableBytes: 100 * 1024 * 1024,
			want:       10 * time.Second,
		},
		{
			name:       "5MB yields minimum 1 second",
			tableBytes: 5 * 1024 * 1024,
			want:       time.Second,
		},
		{
			name:       "zero bytes yields minimum 1 second",
			tableBytes: 0,
			want:       time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateBuildTime(tt.tableBytes)
			if got != tt.want {
				t.Errorf("EstimateBuildTime(%d) = %v, want %v",
					tt.tableBytes, got, tt.want)
			}
		})
	}
}

func TestEstimateWriteAmplification(t *testing.T) {
	tests := []struct {
		name       string
		indexCount int
		writeRate  float64
		want       float64
	}{
		{
			name:       "0 existing indexes yields 100 percent",
			indexCount: 0,
			writeRate:  10.0,
			want:       100.0,
		},
		{
			name:       "4 existing indexes yields 20 percent",
			indexCount: 4,
			writeRate:  10.0,
			want:       20.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateWriteAmplification(tt.indexCount, tt.writeRate)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("EstimateWriteAmplification(%d, %f) = %f, want %f",
					tt.indexCount, tt.writeRate, got, tt.want)
			}
		})
	}
}

func TestComputeQuerySavings(t *testing.T) {
	tests := []struct {
		name         string
		beforeMs     float64
		afterMs      float64
		callsPerDay  int64
		wantPositive bool
		wantZero     bool
	}{
		{
			name:         "positive savings when after < before",
			beforeMs:     100.0,
			afterMs:      10.0,
			callsPerDay:  1000,
			wantPositive: true,
		},
		{
			name:        "same cost yields zero",
			beforeMs:    50.0,
			afterMs:     50.0,
			callsPerDay: 1000,
			wantZero:    true,
		},
		{
			name:        "zero calls yields zero",
			beforeMs:    100.0,
			afterMs:     10.0,
			callsPerDay: 0,
			wantZero:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeQuerySavings(tt.beforeMs, tt.afterMs, tt.callsPerDay)
			if tt.wantPositive && got <= 0 {
				t.Errorf("ComputeQuerySavings(%f, %f, %d) = %v, want positive",
					tt.beforeMs, tt.afterMs, tt.callsPerDay, got)
			}
			if tt.wantZero && got != 0 {
				t.Errorf("ComputeQuerySavings(%f, %f, %d) = %v, want 0",
					tt.beforeMs, tt.afterMs, tt.callsPerDay, got)
			}
		})
	}
}
