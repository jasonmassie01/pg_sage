package analyzer

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func (a *Analyzer) loadRecentlyCreatedIndexes(ctx context.Context) {
	windowDays := a.cfg.Analyzer.UnusedIndexWindowDays
	if windowDays <= 0 {
		windowDays = 7
	}
	rows, err := a.pool.Query(ctx,
		`/* pg_sage */ SELECT sql_executed, executed_at FROM sage.action_log
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
	err := a.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT age(datfrozenxid) FROM pg_database
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
	rows, err := a.pool.Query(ctx,
		`/* pg_sage */ SELECT pid, usename, application_name, state,
		        now() - state_change AS idle_duration
		 FROM pg_stat_activity
		 WHERE state = 'idle in transaction'
		   AND now() - state_change > make_interval(mins => $1)
		   AND pid != pg_backend_pid()`,
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

// maxHistorySamples caps how many 'queries' snapshots the regression
// baseline expands per cycle, however long the lookback.
const maxHistorySamples = 100

// defaultRegressionLookbackDays applies when the configured lookback is
// zero or negative (it used to read nothing at all).
const defaultRegressionLookbackDays = 7

// historicalAveragesSQL is the regression baseline, bounded in SQL (Phase
// 0 item 8, like the forecaster fix). It numbers the lookback's non-empty
// snapshots by time reading only ids and timestamps, keeps at most $2
// evenly spaced ones (the sample the analyzer used to take in Go after
// loading every snapshot), and expands only those. Rows that are not
// arrays and elements without an integer queryid and a numeric
// mean_exec_time are skipped. pg_column_size tells empty or null
// snapshots apart without detoasting them.
const historicalAveragesSQL = `/* pg_sage */
WITH ranked AS (
    SELECT s.id,
           row_number() OVER (ORDER BY s.collected_at, s.id) AS rn,
           count(*) OVER () AS total
      FROM sage.snapshots s
     WHERE s.category = 'queries'
       AND s.collected_at > now() - make_interval(days => $1)
       AND pg_column_size(s.data) > 12
), picked AS (
    SELECT id FROM ranked
     WHERE (rn - 1) % GREATEST(1, ceil(total::numeric / $2::int)::bigint) = 0
     ORDER BY rn
     LIMIT $2::int
)
SELECT (e->>'queryid')::bigint, avg((e->>'mean_exec_time')::float8)
  FROM sage.snapshots s
  JOIN picked p ON p.id = s.id
 CROSS JOIN LATERAL jsonb_array_elements(
           CASE WHEN jsonb_typeof(s.data) = 'array' THEN s.data
                ELSE '[]'::jsonb END) AS e
 WHERE (e->>'queryid') ~ '^-?[0-9]+$'
   AND jsonb_typeof(e->'mean_exec_time') = 'number'
 GROUP BY 1`

// buildHistoricalAverages returns the per-queryid average mean_exec_time
// over the sampled snapshots of the regression lookback. On a query error
// it marks query_regression failed and returns nil.
func (a *Analyzer) buildHistoricalAverages(
	ctx context.Context,
) map[int64]float64 {
	days := a.cfg.Analyzer.RegressionLookbackDays
	if days <= 0 {
		days = defaultRegressionLookbackDays
	}
	rows, err := a.pool.Query(ctx, historicalAveragesSQL, days, maxHistorySamples)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil
	}
	defer rows.Close()
	avgs := make(map[int64]float64)
	for rows.Next() {
		var qid int64
		var avg float64
		if err := rows.Scan(&qid, &avg); err != nil {
			a.evalFail("query_regression")
			a.logFn("ERROR", "analyzer: scan history: %v", err)
			return nil
		}
		avgs[qid] = avg
	}
	if err := rows.Err(); err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: iterate history: %v", err)
		return nil
	}
	return avgs
}

// computeIOUtilPct estimates I/O utilization as the ratio of
// combined I/O wait time (blk_read_time + blk_write_time from
// pg_stat_database) to total query execution time. Returns 0-100.
//
// When I/O wait dominates execution time, aggressive vacuum
// recommendations would make things worse on an already I/O-bound
// system.
func computeIOUtilPct(snap *collector.Snapshot) float64 {
	if snap == nil {
		return 0
	}
	ioWait := snap.System.BlkReadTime + snap.System.BlkWriteTime
	if ioWait <= 0 {
		return 0
	}
	var totalExecTime float64
	for _, q := range snap.Queries {
		totalExecTime += q.TotalExecTime
	}
	if totalExecTime <= 0 {
		return 0
	}
	pct := ioWait / totalExecTime * 100
	if pct > 100 {
		pct = 100
	}
	return pct
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
	rows, err := a.pool.Query(ctx,
		`/* pg_sage */ SELECT DISTINCT object_identifier
		 FROM sage.findings
		 WHERE category ILIKE '%index%'
		   AND status NOT IN ('resolved','suppressed')`,
	)
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
const statsEpochSQL = `/* pg_sage */ SELECT GREATEST(
    COALESCE(d.stats_reset, '-infinity'::timestamptz),
    pg_postmaster_start_time())
  FROM pg_stat_database d
 WHERE d.datname = current_database()`

// loadStatsEpoch refreshes extras.StatsEpoch (G2-B07). On failure it
// fails closed: the epoch is set to now, so no index can look unused for
// longer than the window, and unused_index is not resolved this cycle.
func (a *Analyzer) loadStatsEpoch(ctx context.Context) {
	var epoch time.Time
	if err := a.pool.QueryRow(ctx, statsEpochSQL).Scan(&epoch); err != nil {
		a.logFn("WARN", "analyzer: load stats epoch: %v", err)
		a.extras.StatsEpoch = time.Now()
		a.evalFail("unused_index")
		return
	}
	a.extras.StatsEpoch = epoch
}
