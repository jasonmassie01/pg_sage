package rca

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// Rate signals (rca-03) compare what happened in the last collector
// interval with the interval before it. The counters they read are
// cumulative (the WAL insert position, pg_stat_statements, the sessions
// counter), so each is differenced between two snapshots first.

// walSpikeFloorPerMin is the WAL rate under which no spike is reported: an
// idle cluster going from 10 KiB to 500 KiB a minute is not an incident.
const walSpikeFloorPerMin = 1 << 20

// cacheSpikeFactor is how many times its earlier reads per call a
// statement must read this interval to be named as evicting the cache.
const cacheSpikeFactor = 10

// detectWALSpike fires when the cluster's WAL rate over the collector
// interval that ended at curr is at least wal_spike_multiplier times the
// rate over the interval that ended at prev, and at least
// walSpikeFloorPerMin. Both intervals are the collector's own (each
// snapshot carries the WAL advance since the one before it), so the
// comparison is of two consecutive intervals whatever the analyzer's
// cadence. There is no signal on the first interval, after an LSN
// regression (failover) or on a standby (the collector leaves the WAL
// interval unknown there), and only the cluster owner reports it (the WAL
// is the cluster's).
func (e *Engine) detectWALSpike(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	if !e.ClusterOwner() {
		return nil
	}
	perMin, ok := walRatePerMin(curr)
	before, beforeOK := walRatePerMin(prev)
	if !ok || !beforeOK || before <= 0 || perMin < walSpikeFloorPerMin {
		return nil
	}
	multiplier := cfg.RCA.WALSpikeMultiplier
	if multiplier <= 0 {
		multiplier = config.DefaultRCAWALSpikeMultiplier
	}
	ratio := perMin / before
	if ratio < multiplier {
		return nil
	}
	return &Signal{
		ID:       signalid.WALGrowthSpike,
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"current_wal_bytes_per_min":  int64(perMin),
			"previous_wal_bytes_per_min": int64(before),
			"interval_wal_bytes":         *curr.System.Interval.WALBytes,
			"ratio":                      ratio,
			"multiplier":                 multiplier,
		},
	}
}

// walRatePerMin is the WAL bytes a minute over the collector interval
// that ended at s, unknown without a snapshot or a measured WAL interval.
func walRatePerMin(s *collector.Snapshot) (float64, bool) {
	if s == nil || s.System.Interval == nil || s.System.Interval.WALBytes == nil ||
		s.System.Interval.WALSeconds <= 0 {
		return 0, false
	}
	iv := s.System.Interval
	return float64(*iv.WALBytes) * 60 / iv.WALSeconds, true
}

// queryReadSpike is the statement reading the most blocks this interval
// among those whose reads per call this interval exceed cacheSpikeFactor
// times their reads per call before it. The counters must be comparable:
// the same known pg_stat_statements epoch and no flagged reset; a
// statement whose calls or reads fell was evicted and re-entered.
func queryReadSpike(curr, prev *collector.Snapshot) (readSpike, bool) {
	if prev == nil || curr.StatsReset || prev.StatsEpoch.IsZero() ||
		!prev.StatsEpoch.Equal(curr.StatsEpoch) {
		return readSpike{}, false
	}
	before := make(map[int64]collector.QueryStats, len(prev.Queries))
	for _, q := range prev.Queries {
		before[q.QueryID] = q
	}
	var best readSpike
	found := false
	for _, q := range curr.Queries {
		p, ok := before[q.QueryID]
		calls, reads := q.Calls-p.Calls, q.SharedBlksRead-p.SharedBlksRead
		if !ok || calls <= 0 || reads < 0 || p.Calls <= 0 || p.SharedBlksRead <= 0 {
			continue
		}
		perCall := float64(reads) / float64(calls)
		earlier := float64(p.SharedBlksRead) / float64(p.Calls)
		if perCall <= earlier*cacheSpikeFactor || (found && reads <= best.reads) {
			continue
		}
		best = readSpike{query: q, reads: reads, perCall: perCall, earlier: earlier}
		found = true
	}
	return best, found
}

// readSpike is a statement's interval reads against its earlier rate.
type readSpike struct {
	query   collector.QueryStats
	reads   int64
	perCall float64
	earlier float64
}

// sessionsPerMinute is the sessions opened a minute over the snapshot's
// interval (pg_stat_database.sessions, closed connections included);
// unknown without a measured interval.
func sessionsPerMinute(s *collector.Snapshot) (float64, bool) {
	if s == nil || s.System.Interval == nil || s.System.Interval.Sessions == nil ||
		s.System.Interval.Seconds <= 0 {
		return 0, false
	}
	iv := s.System.Interval
	return float64(*iv.Sessions) * 60 / iv.Seconds, true
}
