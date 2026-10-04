package optimizer

// WhatIfVerdict is the verdict the optimizer gives a what-if evaluation
// with a minimum improvement of minPct (verified, unverified, rejected)
// and why. Shadow mode (roadmap 1.4) scores a recorded index create with
// the same bar the optimizer proposes it with.
func WhatIfVerdict(res WhatIfResult, err error, minPct float64) (string, string) {
	return whatIfVerdict(res, err, minPct)
}
