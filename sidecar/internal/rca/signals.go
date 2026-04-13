package rca

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
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
	if curr.System.MaxConnections == 0 {
		return nil
	}
	pct := curr.System.TotalBackends * 100 / curr.System.MaxConnections
	threshold := cfg.RCA.ConnectionSaturationPct
	if threshold == 0 {
		threshold = 80
	}
	if pct < threshold {
		return nil
	}
	return &Signal{
		ID:       "connections_high",
		FiredAt:  curr.CollectedAt,
		Severity: severityForPct(pct, 90),
		Metrics: map[string]any{
			"total_backends":  curr.System.TotalBackends,
			"max_connections": curr.System.MaxConnections,
			"pct":             pct,
		},
	}
}

func (e *Engine) detectIdleInTxElevated(
	curr *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	threshold := cfg.Analyzer.IdleInTxTimeoutMinutes
	if threshold == 0 {
		threshold = 5
	}
	if curr.ConfigData == nil {
		return nil
	}
	var idleCount int
	var maxDuration float64
	for _, cs := range curr.ConfigData.ConnectionStates {
		if cs.State == "idle in transaction" {
			idleCount = cs.Count
			maxDuration = cs.AvgDurationSeconds
		}
	}
	if idleCount == 0 {
		return nil
	}
	// Fire if any idle-in-tx sessions exceed the threshold.
	if maxDuration < float64(threshold*60) {
		return nil
	}
	return &Signal{
		ID:       "idle_in_tx_elevated",
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"idle_in_tx_count":     idleCount,
			"avg_duration_seconds": maxDuration,
		},
	}
}

func (e *Engine) detectCacheHitDrop(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	warnThreshold := cfg.Analyzer.CacheHitRatioWarning
	if warnThreshold == 0 {
		warnThreshold = 0.95
	}
	if curr.System.CacheHitRatio >= warnThreshold {
		return nil
	}
	sev := "warning"
	if curr.System.CacheHitRatio < warnThreshold-0.05 {
		sev = "critical"
	}
	return &Signal{
		ID:       "cache_hit_ratio_drop",
		FiredAt:  curr.CollectedAt,
		Severity: sev,
		Metrics: map[string]any{
			"cache_hit_ratio": curr.System.CacheHitRatio,
			"threshold":       warnThreshold,
		},
	}
}

func (e *Engine) detectReplicationLag(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	if curr.Replication == nil || len(curr.Replication.Replicas) == 0 {
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
		ID:       "replication_lag_increasing",
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
	var blocked []string
	for _, t := range curr.Tables {
		if t.NLiveTup == 0 {
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
		ID:       "vacuum_blocked",
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
		ID:       "lock_contention",
		FiredAt:  time.Now(),
		Severity: worstSev,
		Metrics: map[string]any{
			"lock_chain_count": len(lockChainFindings),
			"total_blocked":    totalBlocked,
		},
	}
}

func (e *Engine) detectWALSpike(
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) *Signal {
	if prev == nil {
		return nil
	}
	multiplier := cfg.RCA.WALSpikeMultiplier
	if multiplier == 0 {
		multiplier = 2.0
	}
	currWAL := totalWALBytes(curr)
	prevWAL := totalWALBytes(prev)
	if prevWAL == 0 {
		return nil
	}
	ratio := float64(currWAL) / float64(prevWAL)
	if ratio < multiplier {
		return nil
	}
	return &Signal{
		ID:       "wal_growth_spike",
		FiredAt:  curr.CollectedAt,
		Severity: "warning",
		Metrics: map[string]any{
			"current_wal_bytes":  currWAL,
			"previous_wal_bytes": prevWAL,
			"ratio":              ratio,
			"multiplier":         multiplier,
		},
	}
}
