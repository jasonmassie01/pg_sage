package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleSlowQueries flags queries whose mean execution time over the last
// collector interval exceeds the configured threshold (rules-05), with at
// least analyzer.slow_query_min_calls calls in the interval. Warning above
// the threshold; critical above 5x when the query also used at least 1% of
// the interval's wall time, so one ad hoc call is never critical.
func ruleSlowQueries(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if !hasQueryInterval(current, previous) {
		return nil
	}
	threshold := float64(cfg.Analyzer.SlowQueryThresholdMs)
	if threshold <= 0 {
		// 0 passes config validation but would make every query
		// "+Inf x threshold, critical" (G2-B23).
		threshold = config.DefaultSlowQueryThresholdMs
	}
	minCalls, seconds := slowQueryMinCalls(cfg), intervalSeconds(current, previous)
	before := queriesByID(previous)
	var findings []Finding
	for _, q := range adviceQueries(current) {
		iv, ok := measuredInterval(q, before, minCalls)
		if !ok || iv.MeanMs <= threshold {
			continue
		}
		findings = append(findings, slowQueryFinding(q, iv, seconds, threshold))
	}
	return findings
}

// slowQueryFinding is the slow_query finding for q's interval iv.
func slowQueryFinding(q collector.QueryStats, iv queryInterval, seconds,
	threshold float64) Finding {
	ratio := iv.MeanMs / threshold
	share := iv.TotalMs / (seconds * 1000)
	severity := "warning"
	if ratio > 5 && share >= 0.01 {
		severity = "critical"
	}
	return Finding{
		Category:         "slow_query",
		Severity:         severity,
		ObjectType:       "query",
		ObjectIdentifier: queryIdent(q.QueryID),
		Title: fmt.Sprintf(
			"Slow query (%.1fms mean over %d calls in the last %.0fs, %.1fx threshold)",
			iv.MeanMs, iv.Calls, seconds, ratio,
		),
		Detail: map[string]any{
			"queryid":            q.QueryID,
			"query":              q.Query,
			"interval_mean_ms":   iv.MeanMs,
			"interval_calls":     iv.Calls,
			"interval_seconds":   seconds,
			"interval_total_ms":  iv.TotalMs,
			"pct_wall_clock":     share * 100,
			"cumulative_mean_ms": q.MeanExecTime,
			"calls":              q.Calls,
			"total_exec_ms":      q.TotalExecTime,
			"threshold_ms":       threshold,
		},
		Recommendation: "Review query plan with EXPLAIN ANALYZE and optimize.",
		ActionRisk:     "safe",
	}
}

// ruleHighPlanTime flags queries where planning time dominates execution.
func ruleHighPlanTime(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	var findings []Finding
	for _, q := range adviceQueries(current) {
		if q.MeanPlanTime <= 0 || q.Calls < 100 {
			continue
		}
		// A zero mean execution time gives no ratio to measure: dividing
		// by it yields +Inf, which cannot be persisted as JSON detail.
		if q.MeanExecTime <= 0 {
			continue
		}
		if q.MeanPlanTime <= q.MeanExecTime {
			continue
		}
		ratio := q.MeanPlanTime / q.MeanExecTime
		if ratio < 2.0 {
			continue
		}

		severity := "warning"
		if ratio > 10 {
			severity = "critical"
		}
		ident := fmt.Sprintf("queryid:%d", q.QueryID)
		findings = append(findings, Finding{
			Category:         "high_plan_time",
			Severity:         severity,
			ObjectType:       "query",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Plan time %.1fms >> exec time %.1fms (%.1fx)",
				q.MeanPlanTime, q.MeanExecTime, ratio,
			),
			Detail: map[string]any{
				"queryid":      q.QueryID,
				"query":        q.Query,
				"mean_plan_ms": q.MeanPlanTime,
				"mean_exec_ms": q.MeanExecTime,
				"ratio":        ratio,
				"calls":        q.Calls,
			},
			Recommendation: "Consider using prepared statements or simplifying the query.",
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleSeqScanWatchdog flags tables whose sequential scans in the interval
// since the previous snapshot dwarf their index scans, when the table is
// large enough to matter (rules-10): more than 100 seq scans in the
// interval and more than ten times its index scans. Lifetime counters
// would flag an old storm forever and hide a new one on an index-heavy
// table. Without an interval (hasRelationInterval) nothing is judged.
// skipTables is a set of schema.table identifiers already flagged by
// missing FK index rules (to avoid noisy double-flagging).
func ruleSeqScanWatchdog(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	skipTables map[string]bool,
) []Finding {
	if !hasRelationInterval(current, previous) {
		return nil
	}
	minRows := int64(cfg.Analyzer.SeqScanMinRows)
	before := tablesByIdent(previous)
	seconds := current.CollectedAt.Sub(previous.CollectedAt).Seconds()
	var findings []Finding
	for _, t := range current.Tables {
		ident := t.SchemaName + "." + t.RelName
		p, seen := before[ident]
		if skipTables[ident] || !seen || t.NLiveTup < minRows {
			continue
		}
		d, ok := scanDelta(p, t)
		if !ok || d.seq <= 100 || d.seq <= d.idx*10 {
			continue
		}
		findings = append(findings, seqScanFinding(t, d, seconds))
	}
	return findings
}

func seqScanFinding(t collector.TableStats, d scanCounts, seconds float64) Finding {
	ident := t.SchemaName + "." + t.RelName
	rec := "Consider adding indexes for frequent query patterns."
	detail := map[string]any{
		"seq_scan":          t.SeqScan,
		"idx_scan":          t.IdxScan,
		"interval_seq_scan": d.seq,
		"interval_idx_scan": d.idx,
		"interval_seconds":  seconds,
		"n_live_tup":        t.NLiveTup,
		"relpersistence":    t.Relpersistence,
	}
	if t.IsUnlogged() {
		detail["unlogged"] = true
		rec += " Note: unlogged table — indexes are also " +
			"unlogged and lost on crash."
	}
	return Finding{
		Category:         "seq_scan_heavy",
		Severity:         "warning",
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Table %s: %d seq scans vs %d idx scans in the last %.0fs (%d rows)",
			ident, d.seq, d.idx, seconds, t.NLiveTup,
		),
		Detail:         detail,
		Recommendation: rec,
		ActionRisk:     "safe",
	}
}
