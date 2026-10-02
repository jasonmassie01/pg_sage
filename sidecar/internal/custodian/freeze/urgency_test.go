package freeze

import "testing"

func TestBufferThresholds(t *testing.T) {
	tests := []struct {
		name            string
		red             float64
		wantRed, wantAm float64
	}{
		{"configured default", 25, 25, 50},
		{"zero falls back to default", 0, 25, 50},
		{"negative falls back to default", -1, 25, 50},
		{"one hundred falls back to default", 100, 25, 50},
		{"above one hundred falls back", 140, 25, 50},
		{"small buffer", 10, 10, 20},
		{"amber caps at one hundred", 60, 60, 100},
		{"half doubles to exactly one hundred", 50, 50, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BufferThresholds(tt.red)
			if got.RedBufferPct != tt.wantRed || got.AmberBufferPct != tt.wantAm {
				t.Fatalf("BufferThresholds(%v) = %+v, want %v/%v",
					tt.red, got, tt.wantRed, tt.wantAm)
			}
		})
	}
}

func TestTableUrgencyBoundaries(t *testing.T) {
	const maximum = int64(200_000_000)
	thresholds := BufferThresholds(25)
	tests := []struct {
		name            string
		xidAge, mxidAge int64
		want            Urgency
	}{
		{"fresh table", 0, 0, UrgencyGreen},
		{"one xid outside amber", 99_999_999, 0, UrgencyGreen},
		{"exactly at amber buffer", 100_000_000, 0, UrgencyAmber},
		{"one xid outside red", 149_999_999, 0, UrgencyAmber},
		{"exactly at red buffer", 150_000_000, 0, UrgencyRed},
		{"past the maximum", 250_000_000, 0, UrgencyRed},
		{"multixact red beats xid green", 0, 150_000_000, UrgencyRed},
		{"multixact amber beats xid green", 0, 100_000_000, UrgencyAmber},
		{"xid red beats multixact amber", 160_000_000, 120_000_000, UrgencyRed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TableUrgency(tt.xidAge, maximum, tt.mxidAge, maximum, thresholds)
			if got != tt.want {
				t.Fatalf("TableUrgency(xid=%d, mxid=%d) = %q, want %q",
					tt.xidAge, tt.mxidAge, got, tt.want)
			}
		})
	}
}

// An unknown maximum must never read as safe: a caller that credits a
// return to green would otherwise credit on a missing measurement.
func TestTableUrgencyFailsClosedOnInvalidMaximum(t *testing.T) {
	thresholds := BufferThresholds(25)
	for _, maximum := range []int64{0, -1} {
		if got := TableUrgency(0, maximum, 0, 200_000_000, thresholds); got != UrgencyRed {
			t.Fatalf("xid maximum %d = %q, want red", maximum, got)
		}
		if got := TableUrgency(0, 200_000_000, 0, maximum, thresholds); got != UrgencyRed {
			t.Fatalf("multixact maximum %d = %q, want red", maximum, got)
		}
	}
}

// No concurrent access tests: these functions are pure and share no state.
