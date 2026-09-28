package analyzer

import (
	"context"
	"encoding/json"
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

// buildHistoricalAverages loads recent query snapshots and computes
// per-queryid average mean_exec_time for regression detection.
func (a *Analyzer) buildHistoricalAverages(
	ctx context.Context,
) map[int64]float64 {
	rows, err := a.pool.Query(ctx,
		`/* pg_sage */ SELECT data FROM sage.snapshots
		 WHERE category = 'queries'
		   AND collected_at > now() - make_interval(days => $1)
		 ORDER BY collected_at DESC`,
		a.cfg.Analyzer.RegressionLookbackDays,
	)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil
	}
	defer rows.Close()

	type queryEntry struct {
		QueryID        int64   `json:"queryid"`
		MeanExecTimeMs float64 `json:"mean_exec_time"` // collector.QueryStats field (G1-B06)
	}

	var allSnapshots [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			continue
		}
		allSnapshots = append(allSnapshots, data)
	}
	if err := rows.Err(); err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: iterate history: %v", err)
	}

	// Downsample to ~100 snapshots.
	sampled := downsample(allSnapshots, 100)

	sums := make(map[int64]float64)
	counts := make(map[int64]int)

	for _, data := range sampled {
		var entries []queryEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			continue
		}
		for _, e := range entries {
			sums[e.QueryID] += e.MeanExecTimeMs
			counts[e.QueryID]++
		}
	}

	avgs := make(map[int64]float64, len(sums))
	for qid, sum := range sums {
		if c := counts[qid]; c > 0 {
			avgs[qid] = sum / float64(c)
		}
	}
	return avgs
}

// downsample returns up to maxN evenly-spaced items from the input.
func downsample[T any](items []T, maxN int) []T {
	n := len(items)
	if n <= maxN {
		return items
	}
	step := float64(n) / float64(maxN)
	out := make([]T, 0, maxN)
	for i := 0; i < maxN; i++ {
		idx := int(float64(i) * step)
		if idx >= n {
			idx = n - 1
		}
		out = append(out, items[idx])
	}
	return out
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
