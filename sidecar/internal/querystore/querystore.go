// Package querystore maintains a per-queryid metrics time-series in
// sage.query_store (F2). Because pg_stat_statements only exposes lifetime
// averages, pg_sage samples per-queryid totals each cycle (Recorder) so it
// can compute *windowed* latency: the substrate for per-queryid
// verify-and-revert (F1, QueryIntervals) and the SLO latency proxy
// (CaptureDeltas). The window rules (anchor and epoch, R10) live only in
// intervals.go.
package querystore

import "time"

// Sample is one per-queryid metrics observation for a cycle.
type Sample struct {
	QueryID     int64
	Calls       int64
	TotalExecMs float64 // pg_stat_statements.total_exec_time (already ms)
	MeanExecMs  float64
	Rows        int64
	// StatsEpoch is the pg_stat_statements statistics epoch the counters
	// belong to (zero = unknown, stored as NULL). Samples from different
	// epochs are never differenced (R10).
	StatsEpoch time.Time
}

// epochParam maps an unknown (zero) epoch to NULL.
func epochParam(epoch time.Time) *time.Time {
	if epoch.IsZero() {
		return nil
	}
	return &epoch
}

// aggregateSamples sums samples that share a queryid, keeping first-seen
// order. The mean is recomputed from the summed totals.
func aggregateSamples(samples []Sample) []Sample {
	if len(samples) == 0 {
		return nil
	}
	index := make(map[int64]int, len(samples))
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		i, seen := index[s.QueryID]
		if !seen {
			index[s.QueryID] = len(out)
			out = append(out, s)
			continue
		}
		out[i].Calls += s.Calls
		out[i].TotalExecMs += s.TotalExecMs
		out[i].Rows += s.Rows
	}
	for i := range out {
		if out[i].Calls > 0 {
			out[i].MeanExecMs = out[i].TotalExecMs / float64(out[i].Calls)
		}
	}
	return out
}
