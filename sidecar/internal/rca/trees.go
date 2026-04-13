package rca

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// Decision trees: map fired signals to Incident values
// ---------------------------------------------------------------------------

func (e *Engine) runDecisionTrees(
	signals []*Signal,
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) []Incident {
	sigMap := make(map[string]*Signal, len(signals))
	for _, s := range signals {
		sigMap[s.ID] = s
	}

	var incidents []Incident
	if s, ok := sigMap["connections_high"]; ok {
		incidents = append(incidents,
			e.treeConnectionsHigh(s, sigMap, curr, prev, cfg))
	}
	if s, ok := sigMap["cache_hit_ratio_drop"]; ok {
		incidents = append(incidents,
			e.treeCacheHitDrop(s, curr, prev))
	}
	if s, ok := sigMap["vacuum_blocked"]; ok {
		incidents = append(incidents,
			e.treeVacuumBlocked(s, curr))
	}
	if s, ok := sigMap["replication_lag_increasing"]; ok {
		incidents = append(incidents, e.simpleIncident(s,
			"replication_lag",
			"Replication lag exceeds threshold",
			fmt.Sprintf("Worst replica lag: %.0fs",
				floatMetric(s, "worst_lag_seconds"))))
	}
	if s, ok := sigMap["lock_contention"]; ok {
		incidents = append(incidents, e.simpleIncident(s,
			"lock_contention",
			"Lock chain contention detected",
			fmt.Sprintf("%d lock chains, %d total blocked",
				intMetric(s, "lock_chain_count"),
				intMetric(s, "total_blocked"))))
	}
	if s, ok := sigMap["wal_growth_spike"]; ok {
		incidents = append(incidents, e.simpleIncident(s,
			"wal_spike",
			fmt.Sprintf("WAL generation spiked %.1fx over previous cycle",
				floatMetric(s, "ratio")),
			fmt.Sprintf("Current: %d bytes, Previous: %d bytes",
				intMetric(s, "current_wal_bytes"),
				intMetric(s, "previous_wal_bytes"))))
	}
	return incidents
}

// treeConnectionsHigh implements the connections_high decision tree.
//
//	connections_high fires:
//	  -> idle_in_tx_elevated also firing?
//	    -> YES: root cause = idle-in-tx holding connections
//	    -> NO: churn > 2x previous?
//	      -> YES: root cause = connection storm
//	      -> NO: root cause = gradual growth
func (e *Engine) treeConnectionsHigh(
	connSig *Signal,
	sigMap map[string]*Signal,
	curr, prev *collector.Snapshot,
	cfg *config.Config,
) Incident {
	ids := []string{"connections_high"}
	now := curr.CollectedAt

	if _, hasIdle := sigMap["idle_in_tx_elevated"]; hasIdle {
		ids = append(ids, "idle_in_tx_elevated")
		return buildIncident(now, connSig.Severity, ids,
			"Idle-in-transaction sessions saturating connection pool",
			[]ChainLink{
				{Order: 1, Signal: "idle_in_tx_elevated",
					Description: "Sessions stuck in idle-in-transaction state",
					Evidence: fmt.Sprintf("%d idle-in-tx sessions",
						curr.System.IdleInTransaction)},
				{Order: 2, Signal: "connections_high",
					Description: "Connection slots exhausted by idle sessions",
					Evidence: fmt.Sprintf("%d/%d connections used",
						curr.System.TotalBackends,
						curr.System.MaxConnections)},
			},
			[]string{"pg_stat_activity"},
			"SELECT pid, state, query, now()-state_change AS duration "+
				"FROM pg_stat_activity "+
				"WHERE state = 'idle in transaction' "+
				"ORDER BY state_change LIMIT 20;",
			"moderate",
		)
	}

	if prev != nil && curr.ConfigData != nil && prev.ConfigData != nil {
		prevChurn := prev.ConfigData.ConnectionChurn
		currChurn := curr.ConfigData.ConnectionChurn
		if prevChurn > 0 && currChurn > prevChurn*2 {
			return buildIncident(now, connSig.Severity, ids,
				"Connection storm: rapid connection creation/teardown",
				[]ChainLink{
					{Order: 1, Signal: "connections_high",
						Description: "Connection churn doubled since last cycle",
						Evidence: fmt.Sprintf("Churn: %d (prev %d)",
							currChurn, prevChurn)},
				},
				[]string{"pg_stat_activity"}, "", "moderate",
			)
		}
	}

	return buildIncident(now, connSig.Severity, ids,
		"Gradual connection pool growth approaching max_connections",
		[]ChainLink{
			{Order: 1, Signal: "connections_high",
				Description: "Backend count growing without churn spike",
				Evidence: fmt.Sprintf("%d/%d (%d%%)",
					curr.System.TotalBackends,
					curr.System.MaxConnections,
					intMetric(connSig, "pct"))},
		},
		[]string{"pg_stat_activity"}, "", "safe",
	)
}

// treeCacheHitDrop implements the cache_hit_ratio_drop decision tree.
//
//	cache_hit_ratio_drop fires:
//	  -> any query SharedBlksRead delta > 10x previous?
//	    -> YES: root cause = specific query evicting buffers
//	    -> NO: root cause = working set exceeds shared_buffers
func (e *Engine) treeCacheHitDrop(
	sig *Signal,
	curr, prev *collector.Snapshot,
) Incident {
	ids := []string{"cache_hit_ratio_drop"}
	now := curr.CollectedAt

	if prev != nil {
		prevReads := queryReadsMap(prev)
		for _, q := range curr.Queries {
			pr, ok := prevReads[q.QueryID]
			if !ok || pr == 0 {
				continue
			}
			if q.SharedBlksRead > pr*10 {
				truncQ := q.Query
				if len(truncQ) > 120 {
					truncQ = truncQ[:120] + "..."
				}
				return buildIncident(now, sig.Severity, ids,
					fmt.Sprintf("Query %d reading excessive blocks, "+
						"evicting shared_buffers", q.QueryID),
					[]ChainLink{
						{Order: 1, Signal: "cache_hit_ratio_drop",
							Description: "Specific query read 10x+ more blocks",
							Evidence: fmt.Sprintf(
								"QueryID %d: %d blks (prev %d) -- %s",
								q.QueryID, q.SharedBlksRead, pr, truncQ)},
					},
					[]string{fmt.Sprintf("query:%d", q.QueryID)},
					"", "safe",
				)
			}
		}
	}

	return buildIncident(now, sig.Severity, ids,
		"Working set exceeds shared_buffers -- cache hit ratio degraded",
		[]ChainLink{
			{Order: 1, Signal: "cache_hit_ratio_drop",
				Description: "No single query spike; aggregate working set too large",
				Evidence: fmt.Sprintf("Cache hit ratio: %.4f",
					curr.System.CacheHitRatio)},
		},
		[]string{"shared_buffers"}, "", "safe",
	)
}

// treeVacuumBlocked implements the vacuum_blocked decision tree.
//
//	vacuum_blocked fires:
//	  -> any idle-in-tx session with old xmin?
//	    -> YES: root cause = transaction holding oldest xmin
//	    -> NO: root cause = autovacuum falling behind
func (e *Engine) treeVacuumBlocked(
	sig *Signal,
	curr *collector.Snapshot,
) Incident {
	ids := []string{"vacuum_blocked"}
	now := curr.CollectedAt

	for _, l := range curr.Locks {
		if l.State != nil && *l.State == "idle in transaction" {
			return buildIncident(now, sig.Severity, ids,
				fmt.Sprintf("Idle-in-transaction PID %d holding oldest xmin, "+
					"blocking autovacuum", l.PID),
				[]ChainLink{
					{Order: 1, Signal: "vacuum_blocked",
						Description: "Session holding transaction open " +
							"prevents dead-tuple cleanup",
						Evidence: fmt.Sprintf(
							"PID %d in state: idle in transaction",
							l.PID)},
				},
				blockedTables(sig),
				fmt.Sprintf(
					"SELECT pg_terminate_backend(%d); "+
						"-- idle-in-tx blocker", l.PID),
				"high_risk",
			)
		}
	}

	return buildIncident(now, sig.Severity, ids,
		"Autovacuum falling behind -- dead tuple accumulation",
		[]ChainLink{
			{Order: 1, Signal: "vacuum_blocked",
				Description: "No idle-in-tx blocker found; " +
					"autovacuum workers cannot keep pace",
				Evidence: fmt.Sprintf("Tables with high dead tuples: %v",
					sig.Metrics["blocked_tables"])},
		},
		blockedTables(sig), "", "safe",
	)
}

func (e *Engine) simpleIncident(
	sig *Signal,
	source, rootCause, evidence string,
) Incident {
	_ = source // reserved for future Tier 2 source tagging
	return buildIncident(sig.FiredAt, sig.Severity, []string{sig.ID},
		rootCause,
		[]ChainLink{
			{Order: 1, Signal: sig.ID,
				Description: rootCause, Evidence: evidence},
		},
		nil, "", "safe",
	)
}
