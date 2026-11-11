package analyzer

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// The query rules judge what a statement did between the two newest
// snapshots (rules-05), not pg_stat_statements' cumulative mean: a plan
// that turns a 2 ms lookup into 20 ms moves the mean of a million earlier
// calls by a few percent, while the interval mean shows the 20 ms.

// queryInterval is one statement's calls and execution time between two
// snapshots, and their mean.
type queryInterval struct {
	Calls   int64
	TotalMs float64
	MeanMs  float64
}

// hasQueryInterval reports whether the two snapshots measure an interval:
// both read pg_stat_statements, time advanced, the statistics were not
// reset, and both counters belong to one known statistics epoch (R10).
func hasQueryInterval(cur, prev *collector.Snapshot) bool {
	return cur != nil && prev != nil && cur.Available("queries") &&
		prev.Available("queries") && !cur.StatsReset &&
		cur.CollectedAt.After(prev.CollectedAt) &&
		!cur.StatsEpoch.IsZero() && cur.StatsEpoch.Equal(prev.StatsEpoch)
}

// intervalMean is the statement's interval between its previous counters
// prev and its current ones cur. It is invalid when a counter fell (the
// entry was evicted and re-entered) or no call was added.
func intervalMean(cur, prev collector.QueryStats) (queryInterval, bool) {
	calls := cur.Calls - prev.Calls
	total := cur.TotalExecTime - prev.TotalExecTime
	if calls <= 0 || total < 0 {
		return queryInterval{}, false
	}
	return queryInterval{Calls: calls, TotalMs: total, MeanMs: total / float64(calls)}, true
}

// queriesByID indexes the snapshot's statements by queryid.
func queriesByID(snap *collector.Snapshot) map[int64]collector.QueryStats {
	out := make(map[int64]collector.QueryStats, len(snap.Queries))
	for _, q := range snap.Queries {
		out[q.QueryID] = q
	}
	return out
}

// intervalSeconds is the length of the snapshots' interval.
func intervalSeconds(cur, prev *collector.Snapshot) float64 {
	return cur.CollectedAt.Sub(prev.CollectedAt).Seconds()
}

// slowQueryMinCalls is the call floor an interval must reach before its
// mean is evidence; 0 or less uses the default.
func slowQueryMinCalls(cfg *config.Config) int64 {
	if cfg == nil || cfg.Analyzer.SlowQueryMinCalls <= 0 {
		return config.DefaultSlowQueryMinCalls
	}
	return int64(cfg.Analyzer.SlowQueryMinCalls)
}
