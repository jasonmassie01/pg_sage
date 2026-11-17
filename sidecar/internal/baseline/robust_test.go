package baseline

import (
	"math"
	"testing"
)

// Robust (lab-01b) is Judge's scale, shared with the verifier's CUSUM so
// the two never disagree on what a robust deviation is. Pure function: no
// shared state, so no concurrent-access test applies.

func TestRobustIsJudgesScale(t *testing.T) {
	vals := []float64{10, 12, 11, 30, 9}
	s := Robust(vals, 0)
	// median 11; deviations 1,1,0,19,2 -> MAD 1; scale max(1, 0.55) x 1.4826
	if !s.OK || s.N != 5 || s.Median != 11 || s.MAD != 1 || math.Abs(s.Scale-1.4826) > 1e-12 {
		t.Fatalf("Robust = %+v", s)
	}
	j := Judge(vals, 14, 0)
	if math.Abs(j.Z-(14-s.Median)/s.Scale) > 1e-12 {
		t.Fatalf("Judge z %v disagrees with Robust's scale %v", j.Z, s.Scale)
	}
	if vals[0] != 10 || vals[3] != 30 {
		t.Fatalf("Robust sorted the caller's slice: %v", vals)
	}
}

// Boundary: the MAD is floored at 5% of the median; with both zero the
// floor argument, and 1 without one.
func TestRobustFloors(t *testing.T) {
	if s := Robust([]float64{100, 100, 100}, 0); math.Abs(s.Scale-1.4826*5) > 1e-9 {
		t.Fatalf("flat at 100: scale %v, want 1.4826 x 5", s.Scale)
	}
	if s := Robust([]float64{0, 0, 0}, 0.25); s.Scale != 0.25 {
		t.Fatalf("flat at 0 with floor: scale %v", s.Scale)
	}
	if s := Robust([]float64{0, 0, 0}, 0); s.Scale != 1 {
		t.Fatalf("flat at 0 without floor: scale %v", s.Scale)
	}
}

// Nil/empty and invalid input: fewer than MinWeeks usable values are not
// OK; NaN and infinities are dropped before counting.
func TestRobustNeedsMinWeeksUsableValues(t *testing.T) {
	for _, vals := range [][]float64{nil, {}, {1, 2}, {1, math.NaN(), math.Inf(1), 2}} {
		if s := Robust(vals, 0); s.OK || s.Scale != 0 {
			t.Errorf("Robust(%v) = %+v, want not OK", vals, s)
		}
	}
	if s := Robust([]float64{1, math.NaN(), 2, 3}, 0); !s.OK || s.N != 3 || s.Median != 2 {
		t.Fatalf("NaN not dropped: %+v", s)
	}
}
