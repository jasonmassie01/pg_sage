package modellift

// overrideCountClears reports whether k right of n overrides clear the
// rule's count conditions: at least MinOverrides, with a Wilson 95% lower
// bound of at least MinOverrideLowerBound.
func overrideCountClears(k, n int) bool {
	if n < MinOverrides {
		return false
	}
	lo, _ := Wilson(k, n)
	return lo >= MinOverrideLowerBound
}

// MoreCorrectOverridesNeeded is how many more correct held-out overrides
// a family with k right of n needs before its override precision clears
// the rule: the smallest x such that k+x of n+x does (0 when k of n
// already does). It counts the override shortfall only; the rule's other
// conditions (live model, Safe Pass, no forbidden action) are separate.
// Invalid counts give -1.
func MoreCorrectOverridesNeeded(k, n int) int {
	if k < 0 || n < 0 || k > n {
		return -1
	}
	if overrideCountClears(k, n) {
		return 0
	}
	// The lower bound of k+x of n+x rises with x toward 1, so the
	// shortfall exists: grow an upper bound, then bisect.
	hi := 1
	for !overrideCountClears(k+hi, n+hi) {
		hi *= 2
	}
	lo := hi / 2 // fails (or is 0, which fails)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if overrideCountClears(k+mid, n+mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}
