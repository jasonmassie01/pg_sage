package modellift

import "testing"

// The Trust page says plainly how many more correct held-out overrides a
// family needs before its override precision clears the rule: the
// smallest x such that k+x of n+x overrides right reaches MinOverrides
// with a Wilson lower bound of at least MinOverrideLowerBound.

func TestMoreCorrectOverridesNeeded_Boundaries(t *testing.T) {
	for _, c := range []struct{ k, n, want int }{
		{0, 0, 16},  // nothing measured: 16/16 is the first passing count
		{15, 15, 1}, // 15/15 has a lower bound of 0.796
		{16, 16, 0}, // 16/16 passes (0.806)
		{29, 30, 0}, // passes at 29/30
		{28, 30, 3}, // 29/31 (0.793) and 30/32 fail; 31/33 passes
		{9, 9, 7},   // 16/16 again
		{10, 10, 6},
	} {
		if got := MoreCorrectOverridesNeeded(c.k, c.n); got != c.want {
			t.Errorf("MoreCorrectOverridesNeeded(%d, %d) = %d, want %d", c.k, c.n, got,
				c.want)
		}
	}
}

func TestMoreCorrectOverridesNeeded_IsExactlyTheRulesShortfall(t *testing.T) {
	for n := 0; n <= 40; n++ {
		for k := 0; k <= n; k++ {
			x := MoreCorrectOverridesNeeded(k, n)
			if x < 0 {
				t.Fatalf("(%d, %d): %d", k, n, x)
			}
			if !passes(k+x, n+x) {
				t.Fatalf("(%d, %d) + %d correct still fails the rule", k, n, x)
			}
			if x > 0 && passes(k+x-1, n+x-1) {
				t.Fatalf("(%d, %d): %d is not the smallest shortfall", k, n, x)
			}
		}
	}
}

func passes(k, n int) bool {
	lo, _ := Wilson(k, n)
	return n >= MinOverrides && lo >= MinOverrideLowerBound
}

func TestMoreCorrectOverridesNeeded_AWrongOverrideNeverLowersTheNeed(t *testing.T) {
	for n := 0; n <= 60; n++ {
		for k := 0; k <= n; k++ {
			base := MoreCorrectOverridesNeeded(k, n)
			if wrong := MoreCorrectOverridesNeeded(k, n+1); wrong < base {
				t.Fatalf("(%d, %d) needs %d; one more wrong override needs %d", k, n, base,
					wrong)
			}
			if right := MoreCorrectOverridesNeeded(k+1, n+1); base > 0 && right != base-1 {
				t.Fatalf("(%d, %d) needs %d; one more right override needs %d", k, n, base,
					right)
			}
		}
	}
}

func TestMoreCorrectOverridesNeeded_ManyWrongOverridesStillHaveAnAnswer(t *testing.T) {
	x := MoreCorrectOverridesNeeded(0, 1000)
	if x <= 1000 || !passes(x, 1000+x) || passes(x-1, 999+x) {
		t.Fatalf("0/1000 needs %d", x)
	}
}

func TestMoreCorrectOverridesNeeded_InvalidCountsAreRefused(t *testing.T) {
	for _, c := range [][2]int{{-1, 0}, {0, -1}, {3, 2}, {-5, -5}} {
		if got := MoreCorrectOverridesNeeded(c[0], c[1]); got != -1 {
			t.Errorf("MoreCorrectOverridesNeeded(%d, %d) = %d, want -1", c[0], c[1], got)
		}
	}
}
