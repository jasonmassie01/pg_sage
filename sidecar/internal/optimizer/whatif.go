package optimizer

import "fmt"

// What-if verdicts for a recommendation's HypoPG evaluation (Phase 0
// item 7). Only "verified" may pass an autonomous gate; "unverified" is
// still a recommendation, but one an operator must approve.
const (
	WhatIfVerified   = "verified"
	WhatIfUnverified = "unverified"
	WhatIfRejected   = "rejected"
)

// WhatIfResult is the evidence of one hypothetical-index evaluation.
type WhatIfResult struct {
	// Improvement is the planner-cost reduction in percent, weighted by
	// each measured query's total execution time.
	Improvement float64
	// SizeBytes is HypoPG's estimated size of the index.
	SizeBytes int64
	// Measured counts queries planned both without and with the index.
	Measured int
	// Failed counts workload queries whose EXPLAIN failed (each is
	// isolated in a savepoint and does not abort the evaluation).
	Failed int
}

// whatIfVerdict turns an evaluation into a verdict and, unless verified,
// the reason. Only a complete measurement can reject: when some queries
// could not be planned, the unmeasured ones may be the beneficiaries.
func whatIfVerdict(res WhatIfResult, err error, minPct float64) (string, string) {
	switch {
	case err != nil:
		return WhatIfUnverified, "what-if evaluation failed: " + err.Error()
	case res.Measured == 0:
		return WhatIfUnverified, "no workload query could be measured"
	case res.Failed > 0:
		return WhatIfUnverified, fmt.Sprintf(
			"%d of %d workload queries could not be planned",
			res.Failed, res.Failed+res.Measured)
	case res.SizeBytes <= 0:
		return WhatIfUnverified, "hypothetical index size unknown"
	case res.Improvement < minPct:
		return WhatIfRejected, fmt.Sprintf(
			"call-weighted improvement %.1f%% is below the %.1f%% minimum",
			res.Improvement, minPct)
	}
	return WhatIfVerified, ""
}

// weightedImprovement is the cost reduction across the queries planned
// both before and after, weighted by total execution time (falling back
// to calls, then to equal weights), so a large win on the hot query is
// not averaged away by cold ones. It returns the measured query count.
func weightedImprovement(queries []QueryInfo, before, after map[int64]float64,
) (float64, int) {
	type sample struct{ pct, totalTime, calls float64 }
	var samples []sample
	seen := make(map[int64]bool, len(queries))
	for _, q := range queries {
		cost, ok := before[q.QueryID]
		next, okAfter := after[q.QueryID]
		if seen[q.QueryID] || !ok || !okAfter || cost <= 0 {
			continue
		}
		seen[q.QueryID] = true
		samples = append(samples, sample{(cost - next) / cost * 100,
			q.TotalTimeMs, float64(q.Calls)})
	}
	if len(samples) == 0 {
		return 0, 0
	}
	weight := func(s sample) float64 { return s.totalTime }
	if sumWeights(samples, weight) <= 0 {
		weight = func(s sample) float64 { return s.calls }
	}
	if sumWeights(samples, weight) <= 0 {
		weight = func(sample) float64 { return 1 }
	}
	var num float64
	for _, s := range samples {
		if w := weight(s); w > 0 {
			num += w * s.pct
		}
	}
	return num / sumWeights(samples, weight), len(samples)
}

func sumWeights[T any](items []T, weight func(T) float64) float64 {
	var total float64
	for _, it := range items {
		if w := weight(it); w > 0 {
			total += w
		}
	}
	return total
}
