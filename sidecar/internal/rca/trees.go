package rca

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/signalid"
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
	if s, ok := sigMap[signalid.ConnectionsHigh]; ok {
		incidents = append(incidents,
			e.treeConnectionsHigh(s, sigMap, curr, prev, cfg))
	}
	if s, ok := sigMap[signalid.CacheHitRatioDrop]; ok {
		incidents = append(incidents,
			e.treeCacheHitDrop(s, curr, prev))
	}
	if s, ok := sigMap[signalid.VacuumBlocked]; ok {
		incidents = append(incidents,
			e.treeVacuumBlocked(s, curr, e.vacuumRules(cfg)))
	}
	if s, ok := sigMap[signalid.ReplicationLagIncreasing]; ok {
		incidents = append(incidents, e.simpleIncident(s,
			"Replication lag exceeds threshold",
			fmt.Sprintf("Worst replica lag: %.0fs",
				floatMetric(s, "worst_lag_seconds"))))
	}
	if s, ok := sigMap[signalid.LockContention]; ok {
		incidents = append(incidents, lockContentionIncident(s))
	}
	if s, ok := sigMap[signalid.WALGrowthSpike]; ok {
		incidents = append(incidents, e.simpleIncident(s,
			fmt.Sprintf("WAL generation spiked %.1fx over previous cycle",
				floatMetric(s, "ratio")),
			fmt.Sprintf("Current: %d bytes, Previous: %d bytes",
				intMetric(s, "current_wal_bytes"),
				intMetric(s, "previous_wal_bytes"))))
	}
	if s, ok := sigMap[signalid.OrphanedPreparedTx]; ok {
		incidents = append(incidents,
			e.treeOrphanedPreparedTx(s, curr))
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
	ids := []string{signalid.ConnectionsHigh}
	now := curr.CollectedAt

	if _, hasIdle := sigMap[signalid.IdleInTxElevated]; hasIdle {
		ids = append(ids, signalid.IdleInTxElevated)
		return buildIncident(now, connSig.Severity, ids,
			"Idle-in-transaction sessions saturating connection pool",
			[]ChainLink{
				{Order: 1, Signal: signalid.IdleInTxElevated,
					Description: "Sessions stuck in idle-in-transaction state",
					Evidence: fmt.Sprintf("%d idle-in-tx sessions",
						curr.System.IdleInTransaction)},
				{Order: 2, Signal: signalid.ConnectionsHigh,
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
					{Order: 1, Signal: signalid.ConnectionsHigh,
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
			{Order: 1, Signal: signalid.ConnectionsHigh,
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
	ids := []string{signalid.CacheHitRatioDrop}
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
						{Order: 1, Signal: signalid.CacheHitRatioDrop,
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
			{Order: 1, Signal: signalid.CacheHitRatioDrop,
				Description: "No single query spike; aggregate working set too large",
				Evidence: fmt.Sprintf("Cache hit ratio: %.4f",
					floatMetric(sig, "cache_hit_ratio"))},
		},
		[]string{"shared_buffers"}, "", "safe",
	)
}

// treeVacuumBlocked implements the vacuum_blocked decision tree (rca-01).
//
//	vacuum_blocked fires:
//	  -> oldest holder of this database's xmin horizon past its floor?
//	     (a session open rca.xmin_holder_min_minutes, a prepared xact
//	     rca.prepared_xact_min_age_minutes old, a standby's feedback, a
//	     slot's xmin, or its catalog_xmin for a catalog table)
//	    -> YES: root cause = that holder
//	    -> NO: this database's prepared tx past its floor?
//	      -> YES: root cause = prepared tx holding xmin
//	      -> NO: root cause = autovacuum falling behind
func (e *Engine) treeVacuumBlocked(
	sig *Signal,
	curr *collector.Snapshot,
	rules vacuumRules,
) Incident {
	// Branch 1: the oldest holder of the xmin horizon.
	if inc, ok := e.holderIncident(sig, curr, rules); ok {
		return inc
	}

	// Branch 2: a prepared transaction the holder read did not list.
	if p, ok := agedPrepared(curr, rules.preparedFloor); ok {
		inc := preparedIncident(sig, curr, p, p.XIDAge)
		inc.liveEvidence = true
		return inc
	}

	// Branch 3: no obvious blocker found. Live evidence too: an open
	// incident whose holder no longer qualifies drops its proposal.
	inc := buildIncident(curr.CollectedAt, sig.Severity, []string{signalid.VacuumBlocked},
		"Autovacuum falling behind -- dead tuple accumulation",
		[]ChainLink{
			{Order: 1, Signal: signalid.VacuumBlocked,
				Description: "No idle-in-tx or prepared tx blocker found; " +
					"autovacuum workers cannot keep pace",
				Evidence: fmt.Sprintf("Tables with high dead tuples: %v",
					sig.Metrics["blocked_tables"])},
		},
		blockedTables(sig), "", "safe",
	)
	inc.liveEvidence = true
	return inc
}

// treeOrphanedPreparedTx handles orphaned prepared transactions.
// These are invisible to pg_stat_activity but hold xmin and locks.
// Cross-references with vacuum_blocked signal to escalate severity
// when the prepared tx is actively blocking vacuum.
func (e *Engine) treeOrphanedPreparedTx(
	sig *Signal,
	curr *collector.Snapshot,
) Incident {
	ids := []string{signalid.OrphanedPreparedTx}
	now := curr.CollectedAt

	count := intMetric(sig, "count")
	oldestGID := stringMetric(sig, "oldest_gid")
	maxAge := intMetric(sig, "max_xid_age")

	sql := "SELECT gid, prepared, owner, database, " +
		"age(transaction) AS xid_age " +
		"FROM pg_prepared_xacts WHERE database = current_database() " +
		"ORDER BY prepared;"

	return buildIncident(now, sig.Severity, ids,
		fmt.Sprintf("Orphaned prepared transaction(s) holding xmin "+
			"(oldest: %s, xid_age: %d)", oldestGID, maxAge),
		[]ChainLink{
			{Order: 1, Signal: signalid.OrphanedPreparedTx,
				Description: fmt.Sprintf(
					"%d prepared transaction(s) found -- "+
						"invisible to pg_stat_activity", count),
				Evidence: fmt.Sprintf(
					"oldest_gid=%s xid_age=%d", oldestGID, maxAge)},
			{Order: 2, Signal: signalid.OrphanedPreparedTx,
				Description: "Prepared transactions survive " +
					"connection drops and server restarts, " +
					"holding xmin and locks indefinitely",
				Evidence: "ROLLBACK PREPARED '<gid>' to release"},
		},
		nil, sql, "high_risk",
	)
}

func (e *Engine) simpleIncident(
	sig *Signal,
	rootCause, evidence string,
) Incident {
	return buildIncident(sig.FiredAt, sig.Severity, []string{sig.ID},
		rootCause,
		[]ChainLink{
			{Order: 1, Signal: sig.ID,
				Description: rootCause, Evidence: evidence},
		},
		nil, "", "safe",
	)
}
