package analyzer

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

func (a *Analyzer) loadRecentlyCreatedIndexes(ctx context.Context) {
	windowDays := a.cfg.Analyzer.UnusedIndexWindowDays
	if windowDays <= 0 {
		windowDays = 7
	}
	rows, err := a.catalog().Query(ctx,
		`/* pg_sage analyzer */ SELECT sql_executed, executed_at FROM sage.action_log
		 WHERE sql_executed ILIKE 'CREATE INDEX%'
		   AND outcome = 'success'
		   AND executed_at > now() - make_interval(days => $1)`,
		windowDays,
	)
	if err != nil {
		a.logFn("WARN", "analyzer: load recently created indexes: %v", err)
		return
	}
	defer rows.Close()

	created := make(map[string]time.Time)
	for rows.Next() {
		var sql string
		var executedAt time.Time
		if err := rows.Scan(&sql, &executedAt); err != nil {
			continue
		}
		name := extractIndexNameFromSQL(sql)
		if name != "" {
			created[name] = executedAt
		}
	}
	a.extras.RecentlyCreated = created
}

func (a *Analyzer) checkXIDWraparound(ctx context.Context) []Finding {
	var xidAge int64
	err := a.catalog().QueryRow(ctx,
		`/* pg_sage analyzer */ SELECT age(datfrozenxid) FROM pg_database
		 WHERE datname = current_database()`,
	).Scan(&xidAge)
	if err != nil {
		a.evalFail("xid_wraparound")
		a.logFn("ERROR", "analyzer: xid query: %v", err)
		return nil
	}
	return ruleXIDWraparound(xidAge, a.cfg)
}

func (a *Analyzer) checkConnectionLeaks(ctx context.Context) []Finding {
	rows, err := a.catalog().Query(ctx,
		`/* pg_sage analyzer */ SELECT pid, usename, application_name, state,
		        (now() - state_change)::text AS idle_duration
		 FROM pg_stat_activity
		 WHERE state = 'idle in transaction'
		   AND now() - state_change > make_interval(mins => $1)
		   AND pid != pg_backend_pid()
		   AND `+selfmonitor.ActivityExclusionSQL(""),
		a.cfg.Analyzer.IdleInTxTimeoutMinutes,
	)
	if err != nil {
		a.evalFail("connection_leak")
		a.logFn("ERROR", "analyzer: leak query: %v", err)
		return nil
	}
	defer rows.Close()

	var leaked []LeakedConn
	for rows.Next() {
		var c LeakedConn
		var state string
		if err := rows.Scan(
			&c.PID, &c.UserName, &c.AppName,
			&state, &c.IdleDuration,
		); err != nil {
			a.logFn("ERROR", "analyzer: scan leak: %v", err)
			continue
		}
		leaked = append(leaked, c)
	}
	if err := rows.Err(); err != nil {
		a.evalFail("connection_leak")
		a.logFn("ERROR", "analyzer: iterate leaks: %v", err)
	}
	return ruleConnectionLeaks(leaked)
}

// computeIOUtilPct estimates I/O utilization over the last collector
// interval (collection-06): the interval's database I/O wait
// (blk_read_time + blk_write_time of pg_stat_database) over the
// interval's execution time of the statements present in both snapshots
// under the same pg_stat_statements epoch. Returns 0-100, and 0 when
// either side is unknown; lifetime totals from two different resets are
// not a ratio.
//
// When I/O wait dominates execution time, aggressive vacuum
// recommendations would make things worse on an already I/O-bound
// system.
func computeIOUtilPct(cur, prev *collector.Snapshot) float64 {
	if cur == nil || cur.System.Interval == nil {
		return 0
	}
	iv := cur.System.Interval
	if iv.BlkReadTimeMs == nil || iv.BlkWriteTimeMs == nil {
		return 0
	}
	ioWait := *iv.BlkReadTimeMs + *iv.BlkWriteTimeMs
	execDelta := intervalExecTime(cur, prev)
	if ioWait <= 0 || execDelta <= 0 {
		return 0
	}
	return math.Min(ioWait/execDelta*100, 100)
}

// intervalExecTime is the total_exec_time the statements present in both
// snapshots added between them; 0 across a statements reset or an
// unknown epoch. A statement whose total fell was evicted and re-entered,
// so it is left out.
func intervalExecTime(cur, prev *collector.Snapshot) float64 {
	if prev == nil || cur.StatsReset || cur.StatsEpoch.IsZero() ||
		!cur.StatsEpoch.Equal(prev.StatsEpoch) {
		return 0
	}
	before := make(map[int64]float64, len(prev.Queries))
	for _, q := range prev.Queries {
		before[q.QueryID] = q.TotalExecTime
	}
	var delta float64
	for _, q := range cur.Queries {
		if was, ok := before[q.QueryID]; ok && q.TotalExecTime >= was {
			delta += q.TotalExecTime - was
		}
	}
	return delta
}

// canonicalTable normalizes a table reference (e.g. "orders" or
// "public.orders") to canonical lowercase "schema.table" form,
// defaulting to schema "public". Returns "" for empty input.
//
// Used to build the deferredTables set so the tuner can reliably
// match plan-extracted relations against optimizer recommendations
// even when the LLM returns unqualified names.
func canonicalTable(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, ".") {
		return ref
	}
	return "public." + ref
}

// openIndexFindingsSQL reads the open index findings. A finding is open,
// resolved or suppressed; naming the open state (rather than excluding
// the other two) lets idx_findings_category_status, which holds only
// open findings, serve it instead of a scan of every finding ever
// recorded (performance gate).
const openIndexFindingsSQL = `/* pg_sage analyzer */ SELECT DISTINCT object_identifier
	FROM sage.findings
	WHERE category ILIKE '%index%' AND status = 'open'`

// openIndexRecommendationTables returns the canonical names of
// tables that have open (unresolved, unsuppressed) index-related
// findings. The tuner uses these to defer queries on tables where
// an index recommendation is still pending — applying a hint plan
// before the index lands risks installing a directive that becomes
// stale once the executor creates the index.
func (a *Analyzer) openIndexRecommendationTables(
	ctx context.Context,
) []string {
	if a.pool == nil {
		return nil
	}
	rows, err := a.catalog().Query(ctx, openIndexFindingsSQL)
	if err != nil {
		a.logFn("WARN",
			"analyzer: load open index findings: %v", err)
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ident string
		if err := rows.Scan(&ident); err != nil {
			continue
		}
		// Optimizer identities are "schema.table|<index definition>".
		table, _, _ := strings.Cut(ident, "|")
		if t := canonicalTable(table); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// statsEpochSQL returns the instant since which cumulative index counters
// have been accumulating: the later of the last pg_stat_reset for this
// database and the postmaster start (crash recovery discards stats).
const statsEpochSQL = `/* pg_sage analyzer */ SELECT GREATEST(
    COALESCE(d.stats_reset, '-infinity'::timestamptz),
    pg_postmaster_start_time())
  FROM pg_stat_database d
 WHERE d.datname = current_database()`

// loadStatsEpoch refreshes extras.StatsEpoch (G2-B07). On failure it
// fails closed: the epoch is set to now, so no index can look unused for
// longer than the window, and unused_index is not resolved this cycle.
func (a *Analyzer) loadStatsEpoch(ctx context.Context) {
	var epoch time.Time
	if err := a.catalog().QueryRow(ctx, statsEpochSQL).Scan(&epoch); err != nil {
		a.logFn("WARN", "analyzer: load stats epoch: %v", err)
		a.extras.StatsEpoch = time.Now()
		a.evalFail("unused_index")
		return
	}
	a.extras.StatsEpoch = epoch
}
