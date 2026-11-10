package rca

import (
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
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

// walRateAt is the WAL rate of the interval that ended at a snapshot.
type walRateAt struct {
	end    time.Time
	perMin float64
	ok     bool
}

// detectWALSpike fires when the cluster's WAL rate this interval is at
// least wal_spike_multiplier times the previous interval's and at least
// walSpikeFloorPerMin. It stores this interval's rate for the next cycle.
// There is no signal on the first interval, after an LSN regression
// (failover) or on a standby, and only the cluster owner reports it (the
// WAL is the cluster's). Caller holds e.mu.
func (e *Engine) detectWALSpike(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	perMin, ok := walRatePerMin(prev, curr)
	last := e.walRate
	e.walRate = walRateAt{end: curr.CollectedAt, perMin: perMin, ok: ok}
	if !ok || !last.ok || !last.end.Equal(prev.CollectedAt) || last.perMin <= 0 ||
		!e.ClusterOwner() || perMin < walSpikeFloorPerMin {
		return nil
	}
	multiplier := cfg.RCA.WALSpikeMultiplier
	if multiplier <= 0 {
		multiplier = config.DefaultRCAWALSpikeMultiplier
	}
	ratio := perMin / last.perMin
	if ratio < multiplier {
		return nil
	}
	return &Signal{
		ID:       "wal_growth_spike",
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"current_wal_bytes_per_min":  int64(perMin),
			"previous_wal_bytes_per_min": int64(last.perMin),
			"interval_wal_bytes":         curr.System.WALLSN - prev.System.WALLSN,
			"ratio":                      ratio,
			"multiplier":                 multiplier,
		},
	}
}

// walRatePerMin is the WAL bytes a minute between two primary snapshots,
// over the instants their positions were read. It is unknown without a
// previous snapshot, on a standby, without a read position, when the
// position went back (a failover to a server that was behind) and when no
// time passed.
func walRatePerMin(prev, curr *collector.Snapshot) (float64, bool) {
	if prev == nil || curr == nil {
		return 0, false
	}
	p, c := prev.System, curr.System
	if p.IsReplica || c.IsReplica || p.WALReadAt.IsZero() || c.WALReadAt.IsZero() ||
		c.WALLSN < p.WALLSN {
		return 0, false
	}
	elapsed := c.WALReadAt.Sub(p.WALReadAt).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return float64(c.WALLSN-p.WALLSN) * 60 / elapsed, true
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
