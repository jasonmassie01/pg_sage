package querystore

import "testing"

// TestWindowedLatencyMs covers the pure windowed-latency computation,
// including the pg_stat_statements-reset guard.
func TestWindowedLatencyMs(t *testing.T) {
	cases := []struct {
		name             string
		earliest, latest sampleRow
		wantMs           float64
		wantOK           bool
	}{
		{
			name:     "normal window",
			earliest: sampleRow{calls: 100, total: 1000}, // 10ms/call lifetime so far
			latest:   sampleRow{calls: 200, total: 3000}, // +100 calls, +2000ms => 20ms/call in window
			wantMs:   20, wantOK: true,
		},
		{
			name:     "improvement after index",
			earliest: sampleRow{calls: 100, total: 5000},
			latest:   sampleRow{calls: 200, total: 5500}, // +100 calls, +500ms => 5ms/call
			wantMs:   5, wantOK: true,
		},
		{
			name:     "no new calls",
			earliest: sampleRow{calls: 100, total: 1000},
			latest:   sampleRow{calls: 100, total: 1000},
			wantOK:   false,
		},
		{
			name:     "stats reset (counters decreased)",
			earliest: sampleRow{calls: 500, total: 9000},
			latest:   sampleRow{calls: 50, total: 100},
			wantOK:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms, ok := windowedLatencyMs(c.earliest, c.latest)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && ms != c.wantMs {
				t.Errorf("ms = %v, want %v", ms, c.wantMs)
			}
		})
	}
}
