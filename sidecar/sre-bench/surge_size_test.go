package srebench

import (
	"errors"
	"testing"
	"time"
)

// A surge must clear the investigator's absolute floor (4 MiB/s) and be at
// least surgeMargin times the cluster's long-run WAL rate, measured over a
// window that allows the write itself to stretch the sample interval.
func TestSurgeMiB(t *testing.T) {
	const mib = float64(1 << 20)
	window := surgeWindow
	tests := []struct {
		name    string
		minMiB  int
		avg     float64
		want    int
		wantErr bool
	}{
		{"idle cluster keeps the minimum", 64, 0, 64, false},
		{"unknown average keeps the minimum", 64, -1, 64, false},
		{"floor dominates a quiet cluster", 1, 0.1 * mib, 24, false},
		{"busy cluster scales the surge", 64, 10 * mib, 240, false},
		{"exactly at the cap", 1, maxSurgeMiB * mib / (surgeMargin * 6), maxSurgeMiB, false},
		{"beyond the cap is unreachable", 1, 100 * mib, 0, true},
	}
	if window != 6*time.Second {
		t.Fatalf("surgeWindow = %s, the cases assume 6s", window)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := surgeMiB(tt.minMiB, tt.avg)
			if tt.wantErr {
				var unsupported *Unsupported
				if !errors.As(err, &unsupported) || got != 0 {
					t.Fatalf("surgeMiB = %d, %v; want an Unsupported error", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("surgeMiB(%d, %.0f) = %d, %v; want %d", tt.minMiB, tt.avg, got,
					err, tt.want)
			}
		})
	}
}

// No concurrent access tests: surgeMiB is pure.
