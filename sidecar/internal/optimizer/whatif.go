package optimizer

import (
	"fmt"
	"strconv"
	"strings"
)

// maxReasonQueryIDs bounds the queryids a partial verdict's reason lists.
const maxReasonQueryIDs = 10

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
	// UnplannedQueryIDs are the failed queries' queryids, in workload
	// order.
	UnplannedQueryIDs []int64
	// HiddenIndex is the existing index the second plan ran without (a
	// replacement's old index or a measured drop), "" when none was hidden.
	HiddenIndex string
	// Guarded counts the statements whose baseline plan used HiddenIndex.
	Guarded int
	// Regressions are the guarded statements whose planned cost rose more
	// than maxGuardedRegressionPct once HiddenIndex was hidden.
	Regressions []QueryRegression
	// OldIndexPresent marks an index that should have been hidden but was
	// not: HypoPG lacks hypopg_hide_index (before 1.4).
	OldIndexPresent bool
}

// whatIfVerdict turns an evaluation into a verdict and, unless verified,
// the reason. A guarded statement that regresses once the replaced index
// is hidden rejects: nothing an unplanned statement could show undoes it.
// Otherwise only a complete measurement can reject or verify: when some
// queries could not be planned, the unmeasured ones may be the
// beneficiaries or the regressions, so the verdict is unverified and the
// reason says what the planned ones showed and which were not planned. A
// replacement measured with its old index present says so in the reason.
func whatIfVerdict(res WhatIfResult, err error, minPct float64) (string, string) {
	verdict, reason := createVerdict(res, err, minPct)
	if err == nil && res.OldIndexPresent {
		reason = strings.TrimPrefix(reason+"; "+hideUnsupportedReason, "; ")
	}
	return verdict, reason
}

func createVerdict(res WhatIfResult, err error, minPct float64) (string, string) {
	switch {
	case err != nil:
		return WhatIfUnverified, "what-if evaluation failed: " + err.Error()
	case len(res.Regressions) > 0:
		return WhatIfRejected, regressionReason("replacing", res)
	case res.Measured == 0:
		return WhatIfUnverified, "no workload query could be measured"
	case res.Failed > 0:
		return WhatIfUnverified, partialReason(res)
	case res.SizeBytes <= 0:
		return WhatIfUnverified, "hypothetical index size unknown"
	case res.Improvement < minPct:
		return WhatIfRejected, fmt.Sprintf(
			"call-weighted improvement %.1f%% is below the %.1f%% minimum",
			res.Improvement, minPct)
	}
	return WhatIfVerified, ""
}

// partialReason describes a measurement some statements were missing
// from: "planned statements improved 62.0% (call-weighted, 4 of 5); not
// planned: queryid 123".
func partialReason(res WhatIfResult) string {
	change, pct := "improved", res.Improvement
	if pct < 0 {
		change, pct = "regressed", -pct
	}
	return fmt.Sprintf("planned statements %s %.1f%% (call-weighted, %d of %d); "+
		"not planned: %s", change, pct, res.Measured, res.Measured+res.Failed,
		unplannedList(res))
}

func unplannedList(res WhatIfResult) string {
	ids := res.UnplannedQueryIDs
	if len(ids) == 0 {
		return fmt.Sprintf("%d of %d statements", res.Failed, res.Measured+res.Failed)
	}
	shown := ids[:min(len(ids), maxReasonQueryIDs)]
	parts := make([]string, len(shown))
	for i, id := range shown {
		parts[i] = strconv.FormatInt(id, 10)
	}
	list := "queryid " + strings.Join(parts, ", ")
	if rest := len(ids) - len(shown); rest > 0 {
		list += fmt.Sprintf(" and %d more", rest)
	}
	return list
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
