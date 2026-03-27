package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleSlowQueries flags queries whose mean execution time exceeds the
// configured threshold. Warning at 2-5x, critical above 5x.
func ruleSlowQueries(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	threshold := float64(cfg.Analyzer.SlowQueryThresholdMs)
	var findings []Finding

	for _, q := range current.Queries {
		if q.MeanExecTime <= threshold {
			continue
		}

		ratio := q.MeanExecTime / threshold
		severity := "warning"
		if ratio > 5 {
			severity = "critical"
		}

		ident := fmt.Sprintf("queryid:%d", q.QueryID)
		findings = append(findings, Finding{
			Category:         "slow_query",
			Severity:         severity,
			ObjectType:       "query",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Slow query (%.1fms mean, %.1fx threshold)",
				q.MeanExecTime, ratio,
			),
			Detail: map[string]any{
				"queryid":        q.QueryID,
				"query":          q.Query,
				"mean_exec_ms":   q.MeanExecTime,
				"calls":          q.Calls,
				"total_exec_ms":  q.TotalExecTime,
				"threshold_ms":   threshold,
			},
			Recommendation: "Review query plan with EXPLAIN ANALYZE and optimize.",
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleHighPlanTime flags queries where planning time dominates execution.
func ruleHighPlanTime(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	var findings []Finding
	for _, q := range current.Queries {
		if q.MeanPlanTime <= 0 || q.Calls < 100 {
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
				"queryid":        q.QueryID,
				"query":          q.Query,
				"mean_plan_ms":   q.MeanPlanTime,
				"mean_exec_ms":   q.MeanExecTime,
				"ratio":          ratio,
				"calls":          q.Calls,
			},
			Recommendation: "Consider using prepared statements or simplifying the query.",
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleQueryRegression flags queries whose current mean execution time
// has regressed significantly compared to historical averages.
// historicalAvg maps queryid to average mean_exec_time over the lookback
// window. previousCalls maps queryid to calls from previous snapshot for
// reset detection. Skipped on first cycle when historicalAvg is empty.
func ruleQueryRegression(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	historicalAvg map[int64]float64,
	cfg *config.Config,
) []Finding {
	if len(historicalAvg) == 0 {
		return nil
	}

	// Build previous calls map for reset detection.
	prevCalls := make(map[int64]int64)
	if previous != nil {
		for _, q := range previous.Queries {
			prevCalls[q.QueryID] = q.Calls
		}
	}

	multiplier := 1.0 + float64(cfg.Analyzer.RegressionThresholdPct)/100.0
	var findings []Finding

	for _, q := range current.Queries {
		// FIX-4: Skip if calls dropped >90% (pg_stat_statements reset).
		if prev, ok := prevCalls[q.QueryID]; ok && prev > 0 {
			if float64(q.Calls) < float64(prev)*0.1 {
				continue
			}
		}

		avg, ok := historicalAvg[q.QueryID]
		if !ok || avg <= 0 {
			continue
		}
		if q.MeanExecTime <= avg*multiplier {
			continue
		}

		pctIncrease := ((q.MeanExecTime - avg) / avg) * 100.0
		severity := "warning"
		if pctIncrease > 200 {
			severity = "critical"
		}

		ident := fmt.Sprintf("queryid:%d", q.QueryID)
		findings = append(findings, Finding{
			Category:         "query_regression",
			Severity:         severity,
			ObjectType:       "query",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Query regression: %.1fms now vs %.1fms avg (+%.0f%%)",
				q.MeanExecTime, avg, pctIncrease,
			),
			Detail: map[string]any{
				"queryid":        q.QueryID,
				"query":          q.Query,
				"current_ms":     q.MeanExecTime,
				"historical_avg": avg,
				"pct_increase":   pctIncrease,
			},
			Recommendation: "Investigate cause of performance regression.",
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleSeqScanWatchdog flags tables with excessive sequential scans relative
// to index scans, when the table is large enough to matter.
// skipTables is a set of schema.table identifiers already flagged by
// missing FK index rules (to avoid noisy double-flagging).
func ruleSeqScanWatchdog(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	skipTables map[string]bool,
) []Finding {
	minRows := int64(cfg.Analyzer.SeqScanMinRows)
	var findings []Finding

	for _, t := range current.Tables {
		ident := t.SchemaName + "." + t.RelName
		if skipTables[ident] {
			continue
		}
		if t.SeqScan <= 100 || t.NLiveTup < minRows {
			continue
		}
		if t.IdxScan > 0 && t.SeqScan <= t.IdxScan*10 {
			continue
		}

		rec := "Consider adding indexes for frequent query patterns."
		detail := map[string]any{
			"seq_scan":       t.SeqScan,
			"idx_scan":       t.IdxScan,
			"n_live_tup":     t.NLiveTup,
			"relpersistence": t.Relpersistence,
		}
		if t.IsUnlogged() {
			detail["unlogged"] = true
			rec += " Note: unlogged table — indexes are also " +
				"unlogged and lost on crash."
		}
		findings = append(findings, Finding{
			Category:         "seq_scan_heavy",
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Table %s: %d seq scans vs %d idx scans (%d rows)",
				ident, t.SeqScan, t.IdxScan, t.NLiveTup,
			),
			Detail:         detail,
			Recommendation: rec,
			ActionRisk:     "safe",
		})
	}
	return findings
}
