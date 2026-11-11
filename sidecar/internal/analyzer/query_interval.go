package analyzer

import (
	"fmt"

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

// measuredInterval is q's interval against its counters in before, when
// it is valid and made at least minCalls calls: the only interval whose
// mean is evidence either way.
func measuredInterval(q collector.QueryStats, before map[int64]collector.QueryStats,
	minCalls int64) (queryInterval, bool) {
	p, ok := before[q.QueryID]
	if !ok {
		return queryInterval{}, false
	}
	iv, ok := intervalMean(q, p)
	if !ok || iv.Calls < minCalls {
		return queryInterval{}, false
	}
	return iv, true
}

// undecidedQueries are the identities of the current statements without a
// measured interval: idle in the interval, below the call floor, new to
// the list or evicted and re-entered. The interval rules have no verdict
// on them, so their open findings are kept (an idle minute is not a
// recovery, as with temp_spill). A statement that never reached the floor
// in total is not kept: a finding opened from one ad hoc call resolves.
func undecidedQueries(current, previous *collector.Snapshot, minCalls int64) []string {
	before := queriesByID(previous)
	var out []string
	for _, q := range adviceQueries(current) {
		if q.Calls < minCalls {
			continue
		}
		if _, ok := measuredInterval(q, before, minCalls); !ok {
			out = append(out, queryIdent(q.QueryID))
		}
	}
	return out
}

// queryIdent is a statement finding's object identifier.
func queryIdent(id int64) string { return fmt.Sprintf("queryid:%d", id) }

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
