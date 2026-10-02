package srebench

import (
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Rules-only baselines of the M6 families: naive readings of the same
// evidence, with the graph's thresholds but no contradictions, shares or
// sustained-sample checks.

// Rule thresholds.
const (
	ruleTempHistoricBytes = 64 << 20
	ruleLagBytes          = 16 << 20
)

func m6Rule(kind sre.TriggerKind, ev []probes.Result) string {
	switch kind {
	case sre.TriggerCheckpoint:
		return ckptRule(ev)
	case sre.TriggerTempFiles:
		return tempRule(ev)
	case sre.TriggerReplicationLag:
		return replRule(ev)
	case sre.TriggerLWLock:
		return lwlockRule(ev)
	}
	return ""
}

// ckptRule: any requested checkpoint means max_wal_size is too small.
func ckptRule(ev []probes.Result) string {
	s := series(ev, probes.CheckpointActivity)
	if len(s) < 2 {
		return ""
	}
	a, errA := probes.CheckpointStats(s[0])
	b, errB := probes.CheckpointStats(s[len(s)-1])
	if errA == nil && errB == nil && b.Requested > a.Requested {
		return "max_wal_size_undersized"
	}
	return ""
}

// tempRule: any live temp file is a runaway, any statement spill a
// repeated statement, and a large cumulative counter a runaway.
func tempRule(ev []probes.Result) string {
	if s := series(ev, probes.TempFileHolders); len(s) > 0 {
		if hs, err := probes.TempHolders(s[len(s)-1]); err == nil && len(hs) > 0 {
			return "runaway_spill_query"
		}
	}
	if s := series(ev, probes.TempSpillStatements); len(s) > 1 {
		a, errA := probes.SpillStatements(s[0])
		b, errB := probes.SpillStatements(s[len(s)-1])
		if errA == nil && errB == nil && spilledMore(a, b) {
			return "repeated_spill_statement"
		}
	}
	if s := series(ev, probes.TempFileActivity); len(s) > 0 {
		if t, err := probes.TempStats(s[len(s)-1]); err == nil &&
			t.Bytes >= ruleTempHistoricBytes {
			return "runaway_spill_query"
		}
	}
	return ""
}

func spilledMore(a, b []probes.SpillStatement) bool {
	before := map[int64]float64{}
	for _, s := range a {
		before[s.QueryID] = s.TempBlksWritten
	}
	for _, s := range b {
		if s.TempBlksWritten > before[s.QueryID] {
			return true
		}
	}
	return false
}

// replRule: any lagging replica is a replay backlog.
func replRule(ev []probes.Result) string {
	s := series(ev, probes.ReplicationLag)
	if len(s) == 0 {
		return ""
	}
	rs, err := probes.ReplicationStages(s[len(s)-1])
	if err != nil {
		return ""
	}
	for _, r := range rs {
		if r.ReplayLagBytes >= ruleLagBytes {
			return "standby_replay_backlog"
		}
	}
	return ""
}

// lwlockRule: the most waited modeled LWLock of the last sample.
func lwlockRule(ev []probes.Result) string {
	s := series(ev, probes.LWLockWaits)
	if len(s) == 0 {
		return ""
	}
	gs, err := probes.WaitGroups(s[len(s)-1])
	if err != nil {
		return ""
	}
	counts := map[causal.NodeID]int64{}
	best, bestN := causal.NodeID(""), int64(0)
	for _, g := range gs {
		id, ok := causal.LWLockClass(g.Event)
		if !ok || g.Type != "LWLock" {
			continue
		}
		counts[id] += g.Backends
		if counts[id] > bestN {
			best, bestN = id, counts[id]
		}
	}
	return string(best)
}
