package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// LockChain represents a single root-blocker and all sessions it blocks.
type LockChain struct {
	RootBlockerPID   int       `json:"root_blocker_pid"`
	RootBlockerQuery string    `json:"root_blocker_query"`
	RootBlockerState string    `json:"root_blocker_state"`
	RootBlockerApp   string    `json:"root_blocker_app"`
	RootBlockerSince time.Time `json:"root_blocker_since"`
	LockedRelation   string    `json:"locked_relation"`
	LockedRelations  []string  `json:"locked_relations,omitempty"`
	TotalRelations   int       `json:"total_relations,omitempty"`
	BlockerMode      string    `json:"blocker_mode"`
	ChainDepth       int       `json:"chain_depth"`
	BlockedPIDs      []int     `json:"blocked_pids"`
	TotalBlocked     int       `json:"total_blocked"`
}

// isSafeProcess returns true if the given PID or application name matches
// pg_sage's own backend or a configured safe pattern (e.g. replication,
// patroni). Safe processes still produce findings but never get kill SQL.
func isSafeProcess(appName string, pid int, ownPID int, patterns []string) bool {
	if pid == ownPID {
		return true
	}
	lower := strings.ToLower(appName)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// lockChainQuery is the recursive CTE that walks pg_blocking_pids() to
// find every root blocker and the tree of sessions it blocks.
const lockChainQuery = `
WITH RECURSIVE lock_chain AS (
    SELECT
        sa.pid              AS blocked_pid,
        sa.query            AS blocked_query,
        sa.state            AS blocked_state,
        sa.query_start      AS blocked_since,
        sa.application_name AS blocked_app,
        blocker_pid,
        1                   AS depth,
        ARRAY[sa.pid, blocker_pid] AS chain
    FROM pg_stat_activity sa,
         LATERAL unnest(pg_blocking_pids(sa.pid)) AS blocker_pid
    WHERE sa.wait_event_type = 'Lock'
    UNION ALL
    SELECT
        lc.blocked_pid,
        lc.blocked_query,
        lc.blocked_state,
        lc.blocked_since,
        lc.blocked_app,
        upper_blocker,
        lc.depth + 1,
        lc.chain || upper_blocker
    FROM lock_chain lc,
         LATERAL unnest(pg_blocking_pids(lc.blocker_pid)) AS upper_blocker
    WHERE upper_blocker != ALL(lc.chain)
      AND lc.depth < 10
),
root_blockers AS (
    SELECT DISTINCT blocker_pid AS root_pid
    FROM lock_chain
    WHERE blocker_pid NOT IN (SELECT blocked_pid FROM lock_chain)
)
SELECT
    rb.root_pid                        AS root_blocker_pid,
    sa.query                           AS root_blocker_query,
    sa.state                           AS root_blocker_state,
    sa.query_start                     AS root_blocker_since,
    sa.application_name                AS root_blocker_app,
    sa.wait_event_type                 AS root_blocker_wait_type,
    max(lc.depth)                      AS chain_depth,
    array_agg(DISTINCT lc.blocked_pid) AS blocked_pids,
    count(DISTINCT lc.blocked_pid)     AS total_blocked
FROM root_blockers rb
JOIN lock_chain lc ON lc.blocker_pid = rb.root_pid
    OR (rb.root_pid = ANY(lc.chain) AND lc.blocked_pid != rb.root_pid)
JOIN pg_stat_activity sa ON sa.pid = rb.root_pid
GROUP BY rb.root_pid, sa.query, sa.state, sa.query_start,
         sa.application_name, sa.wait_event_type
ORDER BY total_blocked DESC`

// lockedRelationQuery finds relations a root blocker holds locks on,
// limited to 5 to avoid noise when migrations lock hundreds of tables.
// Also returns the total count so the UI can show "and N more".
const lockedRelationQuery = `
WITH rel_locks AS (
    SELECT DISTINCT l.relation::regclass::text AS locked_relation, l.mode
    FROM pg_locks l
    WHERE l.pid = $1 AND l.granted AND l.relation IS NOT NULL
)
SELECT locked_relation, mode, (SELECT count(*) FROM rel_locks) AS total
FROM rel_locks
LIMIT 5`

// DetectLockChains queries PostgreSQL for cascading lock waits and returns
// a LockChain per root blocker. It is called directly from the analyzer
// loop (not via AllRules) because it requires a live DB connection.
func DetectLockChains(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg *config.Config,
) ([]LockChain, error) {
	if !cfg.Analyzer.LockChain.Enabled {
		return nil, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := pool.Query(queryCtx, lockChainQuery)
	if err != nil {
		return nil, fmt.Errorf("lock chain query: %w", err)
	}
	defer rows.Close()

	var chains []LockChain
	for rows.Next() {
		var (
			c             LockChain
			blockedPIDs32 []int32
			rootQuery     *string
			rootState     *string
			rootSince     *time.Time
			rootApp       *string
			waitType      *string
		)

		if err := rows.Scan(
			&c.RootBlockerPID,
			&rootQuery,
			&rootState,
			&rootSince,
			&rootApp,
			&waitType,
			&c.ChainDepth,
			&blockedPIDs32,
			&c.TotalBlocked,
		); err != nil {
			return nil, fmt.Errorf("scan lock chain row: %w", err)
		}

		if rootQuery != nil {
			c.RootBlockerQuery = *rootQuery
		}
		if rootState != nil {
			c.RootBlockerState = *rootState
		}
		if rootSince != nil {
			c.RootBlockerSince = *rootSince
		}
		if rootApp != nil {
			c.RootBlockerApp = *rootApp
		}

		c.BlockedPIDs = make([]int, len(blockedPIDs32))
		for i, pid := range blockedPIDs32 {
			c.BlockedPIDs[i] = int(pid)
		}

		// Supplementary: find the locked relation.
		enrichLockedRelation(queryCtx, pool, &c)

		chains = append(chains, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lock chains: %w", err)
	}
	return chains, nil
}

// enrichLockedRelation fills LockedRelation(s) and BlockerMode from pg_locks.
// The query returns up to 5 relations plus a total count.
func enrichLockedRelation(
	ctx context.Context,
	pool *pgxpool.Pool,
	c *LockChain,
) {
	rows, err := pool.Query(ctx, lockedRelationQuery, c.RootBlockerPID)
	if err != nil {
		return // best-effort; the main chain data is already captured
	}
	defer rows.Close()

	var rels []string
	for rows.Next() {
		var rel, mode string
		var total int
		if err := rows.Scan(&rel, &mode, &total); err != nil {
			return
		}
		rels = append(rels, rel)
		if c.BlockerMode == "" {
			c.BlockerMode = mode
		}
		c.TotalRelations = total
	}
	if len(rels) > 0 {
		c.LockedRelation = rels[0]
		c.LockedRelations = rels
	}
}

// truncateQuery shortens a query string to maxLen, appending "..." if cut.
func truncateQuery(q string, maxLen int) string {
	if len(q) <= maxLen {
		return q
	}
	return q[:maxLen] + "..."
}

// lockChainFindings converts detected lock chains into Finding values
// suitable for persistence and notification.
func lockChainFindings(
	chains []LockChain,
	lcCfg config.LockChainConfig,
	ownPID int,
) []Finding {
	var findings []Finding
	for _, c := range chains {
		safe := isSafeProcess(
			c.RootBlockerApp, c.RootBlockerPID,
			ownPID, lcCfg.SafePatterns,
		)

		// Below threshold and not safe: skip entirely.
		if c.TotalBlocked < lcCfg.MinBlockedThreshold && !safe {
			continue
		}

		// Safe processes always emit INFO with no remediation SQL.
		if safe {
			findings = append(findings, buildSafeFinding(c))
			continue
		}

		findings = append(findings, buildActionableFinding(c, lcCfg))
	}
	return findings
}

// buildSafeFinding creates an informational finding for a safe-process
// root blocker (pg_sage itself, replication, patroni, etc.).
func buildSafeFinding(c LockChain) Finding {
	return Finding{
		Category:         "lock_chain",
		Severity:         "info",
		ObjectType:       "lock",
		ObjectIdentifier: fmt.Sprintf("pid:%d", c.RootBlockerPID),
		Title: fmt.Sprintf(
			"Lock chain: safe process PID %d (%s) blocking %d sessions",
			c.RootBlockerPID, c.RootBlockerApp, c.TotalBlocked,
		),
		Detail:         lockChainDetail(c),
		Recommendation: "Safe process identified as root blocker; no action taken.",
	}
}

// buildActionableFinding creates a warning/critical finding with optional
// pg_terminate_backend or pg_cancel_backend SQL.
func buildActionableFinding(c LockChain, lcCfg config.LockChainConfig) Finding {
	severity := "warning"
	if c.TotalBlocked >= lcCfg.CriticalBlockedThreshold {
		severity = "critical"
	}

	f := Finding{
		Category:         "lock_chain",
		Severity:         severity,
		ObjectType:       "lock",
		ObjectIdentifier: fmt.Sprintf("pid:%d", c.RootBlockerPID),
		Title: fmt.Sprintf(
			"Lock chain: PID %d blocking %d sessions (depth %d)",
			c.RootBlockerPID, c.TotalBlocked, c.ChainDepth,
		),
		Detail: lockChainDetail(c),
	}

	blockedDuration := time.Since(c.RootBlockerSince)

	switch {
	case c.RootBlockerState == "idle in transaction" &&
		blockedDuration >= time.Duration(lcCfg.IdleInTxTerminateMinutes)*time.Minute:
		f.Recommendation = fmt.Sprintf(
			"Root blocker is idle in transaction for %s; terminate.",
			blockedDuration.Truncate(time.Second),
		)
		f.RecommendedSQL = fmt.Sprintf(
			"SELECT pg_terminate_backend(%d);", c.RootBlockerPID,
		)
		f.RollbackSQL = "" // termination is not rollback-able
		f.ActionRisk = "moderate"

	case c.RootBlockerState == "active" &&
		blockedDuration >= time.Duration(lcCfg.ActiveQueryCancelMinutes)*time.Minute:
		f.Recommendation = fmt.Sprintf(
			"Root blocker active query running for %s; cancel.",
			blockedDuration.Truncate(time.Second),
		)
		f.RecommendedSQL = fmt.Sprintf(
			"SELECT pg_cancel_backend(%d);", c.RootBlockerPID,
		)
		f.RollbackSQL = "" // cancellation is not rollback-able
		f.ActionRisk = "safe"

	default:
		f.Recommendation = fmt.Sprintf(
			"Root blocker PID %d in state '%s' for %s; monitoring.",
			c.RootBlockerPID, c.RootBlockerState,
			blockedDuration.Truncate(time.Second),
		)
	}

	return f
}

// lockChainDetail builds the Detail map shared by all lock chain findings.
func lockChainDetail(c LockChain) map[string]any {
	detail := map[string]any{
		"root_blocker_pid":   c.RootBlockerPID,
		"root_blocker_query": truncateQuery(c.RootBlockerQuery, 200),
		"root_blocker_state": c.RootBlockerState,
		"root_blocker_app":   c.RootBlockerApp,
		"root_blocker_since": c.RootBlockerSince,
		"locked_relation":    c.LockedRelation,
		"blocker_mode":       c.BlockerMode,
		"chain_depth":        c.ChainDepth,
		"blocked_pids":       c.BlockedPIDs,
		"total_blocked":      c.TotalBlocked,
	}
	if len(c.LockedRelations) > 1 {
		detail["locked_relations"] = c.LockedRelations
		detail["total_relations"] = c.TotalRelations
	}
	return detail
}
