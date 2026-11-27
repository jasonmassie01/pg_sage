package rca

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// ---------------------------------------------------------------------------
// Signal detection
// ---------------------------------------------------------------------------

func (e *Engine) detectSignals(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) []*Signal {
	type detector func() *Signal
	fns := []detector{
		func() *Signal { return e.detectConnectionsHigh(curr, prev, cfg) },
		func() *Signal { return e.detectIdleInTxElevated(curr, cfg) },
		func() *Signal { return e.detectCacheHitDrop(curr, prev, cfg) },
		func() *Signal { return e.detectReplicationLag(curr, prev, cfg) },
		func() *Signal { return e.detectVacuumBlocked(curr, cfg) },
		func() *Signal { return e.detectLockContention(lockChainFindings) },
		func() *Signal { return e.detectWALSpike(curr, prev, cfg) },
		func() *Signal { return e.detectOrphanedPreparedTx(curr) },
	}

	var fired []*Signal
	for _, fn := range fns {
		if s := fn(); s != nil {
			fired = append(fired, s)
		}
	}
	return fired
}

func (e *Engine) detectConnectionsHigh(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	if curr.System.MaxConnections == 0 || !e.ClusterOwner() {
		return nil
	}
	used := connectionSlotsUsed(curr.System)
	pct := used * 100 / curr.System.MaxConnections
	threshold := cfg.RCA.ConnectionSaturationPct
	if threshold == 0 {
		threshold = 80
	}
	if pct < threshold {
		return nil
	}
	return &Signal{
		ID:       signalid.ConnectionsHigh,
		FiredAt:  curr.CollectedAt,
		Severity: severityForPct(pct, 90),
		Metrics: map[string]any{
			"total_backends":  curr.System.TotalBackends,
			"client_backends": used,
			"max_connections": curr.System.MaxConnections,
			"pct":             pct,
		},
	}
}

// detectIdleInTxElevated fires when this database's longest
// idle-in-transaction client session has been idle for
// analyzer.idle_in_tx_timeout_minutes (rca-03: the longest, not the
// average, so one session idle for an hour among many short ones fires).
// It reads the system stats, collected with or without the advisor.
func (e *Engine) detectIdleInTxElevated(
	curr *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	threshold := cfg.Analyzer.IdleInTxTimeoutMinutes
	if threshold == 0 {
		threshold = 5
	}
	idleCount := curr.System.IdleInTransaction
	maxDuration := curr.System.IdleInTxMaxSeconds
	if idleCount == 0 || maxDuration < float64(threshold*60) {
		return nil
	}
	return &Signal{
		ID:       signalid.IdleInTxElevated,
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"idle_in_tx_count":     idleCount,
			"max_duration_seconds": maxDuration,
		},
	}
}

// detectCacheHitDrop fires on the cache hit ratio of the last collector
// interval (collection-06), when it accessed enough blocks to be evidence.
// The lifetime ratio (since the last reset, often months) barely moves
// when a working set outgrows shared_buffers. Across a reset the interval
// ratio is unknown: no signal (rca-03).
func (e *Engine) detectCacheHitDrop(
	curr, _ *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	warnThreshold := cfg.Analyzer.CacheHitRatioWarning
	if warnThreshold == 0 {
		warnThreshold = 0.95
	}
	ratio, blocks, ok := analyzer.IntervalCacheRatio(curr)
	if !ok || ratio >= warnThreshold {
		return nil
	}
	sev := "warning"
	if ratio < warnThreshold-0.05 {
		sev = "critical"
	}
	return &Signal{
		ID:       signalid.CacheHitRatioDrop,
		FiredAt:  curr.CollectedAt,
		Severity: sev,
		Metrics: map[string]any{
			"cache_hit_ratio":  ratio,
			"blocks_accessed":  blocks,
			"interval_seconds": curr.System.Interval.Seconds,
			"threshold":        warnThreshold,
		},
	}
}

func (e *Engine) detectReplicationLag(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	if curr.Replication == nil || len(curr.Replication.Replicas) == 0 ||
		!e.ClusterOwner() {
		return nil
	}
	thresholdS := cfg.RCA.ReplicationLagThresholdS
	if thresholdS == 0 {
		thresholdS = 30
	}
	var worstLagS float64
	var worstAddr string
	for _, r := range curr.Replication.Replicas {
		lagS := parseIntervalSeconds(r.ReplayLag)
		if lagS > worstLagS {
			worstLagS = lagS
			if r.ClientAddr != nil {
				worstAddr = *r.ClientAddr
			}
		}
	}
	if worstLagS < float64(thresholdS) {
		return nil
	}
	return &Signal{
		ID:       signalid.ReplicationLagIncreasing,
		FiredAt:  curr.CollectedAt,
		Severity: severityForFloat(worstLagS, float64(thresholdS*2)),
		Metrics: map[string]any{
			"worst_lag_seconds": worstLagS,
			"worst_replica":     worstAddr,
			"threshold":         thresholdS,
		},
	}
}

func (e *Engine) detectVacuumBlocked(
	curr *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	deadPct := cfg.Analyzer.TableBloatDeadTuplePct
	if deadPct == 0 {
		deadPct = 10
	}
	minDead := cfg.RCA.VacuumDeadTupleFloor()
	minBytes := cfg.RCA.VacuumTableBytesFloor()
	var blocked []string
	for _, t := range curr.Tables {
		// Size floor (dogfood round 2): a ratio on a table with a handful
		// of rows, or a few pages, is not autovacuum falling behind.
		if t.NLiveTup == 0 || t.NDeadTup < minDead || t.TableBytes < minBytes {
			continue
		}
		ratio := float64(t.NDeadTup) * 100 / float64(t.NLiveTup+t.NDeadTup)
		if ratio >= float64(deadPct) {
			blocked = append(blocked,
				fmt.Sprintf("%s.%s", t.SchemaName, t.RelName))
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	return &Signal{
		ID:       signalid.VacuumBlocked,
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"blocked_tables": blocked,
			"dead_tuple_pct": deadPct,
		},
	}
}

func (e *Engine) detectLockContention(
	lockChainFindings []analyzer.Finding,
) *Signal {
	if len(lockChainFindings) == 0 {
		return nil
	}
	worstSev := "info"
	totalBlocked := 0
	for _, f := range lockChainFindings {
		if f.Category != "lock_chain" {
			continue
		}
		if severityRank(f.Severity) > severityRank(worstSev) {
			worstSev = f.Severity
		}
		if tb, ok := f.Detail["total_blocked"].(int); ok {
			totalBlocked += tb
		}
	}
	if worstSev == "info" && totalBlocked == 0 {
		return nil
	}
	return &Signal{
		ID:       signalid.LockContention,
		FiredAt:  time.Now(),
		Severity: worstSev,
		Metrics: map[string]any{
			"lock_chain_count": len(lockChainFindings),
			"total_blocked":    totalBlocked,
		},
		blockers: blockersFromFindings(lockChainFindings),
	}
}

// heldSignals are the signals this cycle cannot evaluate because the
// collector could not read the category they need: their silence is not
// a clear, so an open incident they back is not counted toward
// resolution (collection-05).
func heldSignals(curr *collector.Snapshot) map[string]bool {
	held := map[string]bool{}
	if curr != nil && !curr.Available("prepared_xacts") {
		held["orphaned_prepared_tx"] = true
	}
	return held
}

// detectOrphanedPreparedTx fires when this database has a prepared
// transaction at least rca.prepared_xact_min_age_minutes old (rca-01:
// two-phase commit prepares and commits transactions in normal
// operation). Prepared transactions survive connection drops and server
// restarts, hold xmin and locks indefinitely, and are invisible to
// pg_stat_activity. count is every prepared transaction listed; the
// oldest is the oldest (by XID age) past the floor.
func (e *Engine) detectOrphanedPreparedTx(
	curr *collector.Snapshot,
) *Signal {
	if !curr.Available("prepared_xacts") { // unknown, not "none" (heldSignals)
		return nil
	}
	oldest, ok := agedPrepared(curr, e.cfg.PreparedXactFloor())
	if !ok {
		return nil
	}
	sev := "warning"
	if oldest.XIDAge > 100_000_000 {
		sev = "critical"
	}
	return &Signal{
		ID:       signalid.OrphanedPreparedTx,
		FiredAt:  curr.CollectedAt,
		Severity: sev,
		Metrics: map[string]any{
			"count":       len(curr.PreparedXacts),
			"oldest_gid":  oldest.GID,
			"max_xid_age": oldest.XIDAge,
		},
	}
}
