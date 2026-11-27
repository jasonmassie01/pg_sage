package optimizer

import (
	"cmp"
	"slices"
)

// maxWhatIfStatements bounds one what-if evaluation: at most this many
// statements are planned, each once without and once with the candidate
// (the agent's what-if tool caps a request at 20 the same way).
const maxWhatIfStatements = 25

// whatIfSample is the statements one what-if plans: the recommendation's
// affected queryids first, then the rest of the workload, each group by
// total execution time, at most maxWhatIfStatements in all. Ties keep the
// workload's order; an affected queryid without a statement (or 0, an
// unknown one) is ignored. queries is not modified.
func whatIfSample(queries []QueryInfo, affected []int64) []QueryInfo {
	want := make(map[int64]bool, len(affected))
	for _, id := range affected {
		if id != 0 {
			want[id] = true
		}
	}
	var first, rest []QueryInfo
	for _, q := range queries {
		if want[q.QueryID] {
			delete(want, q.QueryID)
			first = append(first, q)
			continue
		}
		rest = append(rest, q)
	}
	hottestFirst := func(a, b QueryInfo) int { return cmp.Compare(b.TotalTimeMs, a.TotalTimeMs) }
	slices.SortStableFunc(first, hottestFirst)
	slices.SortStableFunc(rest, hottestFirst)
	out := append(first, rest...)
	return out[:min(len(out), maxWhatIfStatements)]
}
