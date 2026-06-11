package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleStaleStatistics recommends ANALYZE for large tables whose planner
// statistics are missing or stale, which causes bad row estimates and
// plan disasters. ANALYZE is idempotent and SAFE (no rollback needed).
// This is the deterministic core of A4; LLM-driven CREATE STATISTICS for
// correlated columns layers on top using plan estimate-vs-actual skew.
func ruleStaleStatistics(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	return ruleStaleStatisticsAt(current, cfg, time.Now())
}

func ruleStaleStatisticsAt(
	current *collector.Snapshot,
	cfg *config.Config,
	now time.Time,
) []Finding {
	if current == nil {
		return nil
	}
	minRows := int64(cfg.Analyzer.AnalyzeStaleMinRows)
	if minRows <= 0 {
		minRows = 10000
	}
	staleDays := cfg.Analyzer.AnalyzeStaleDays
	if staleDays <= 0 {
		staleDays = 7
	}
	cutoff := now.AddDate(0, 0, -staleDays)

	var findings []Finding
	for _, t := range current.Tables {
		if t.NLiveTup < minRows {
			continue
		}
		writes := t.NTupIns + t.NTupUpd + t.NTupDel
		if writes == 0 {
			continue // static table — stats don't drift
		}
		last := mostRecentAnalyze(t)
		neverAnalyzed := last.IsZero()
		stale := !neverAnalyzed && last.Before(cutoff)
		if !neverAnalyzed && !stale {
			continue
		}

		ident := t.SchemaName + "." + t.RelName
		q := sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)
		reason := "has never been analyzed"
		if stale {
			reason = fmt.Sprintf(
				"was last analyzed over %d days ago", staleDays)
		}
		findings = append(findings, Finding{
			Category:         "stale_statistics",
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Table %s %s; the planner may use bad row estimates",
				ident, reason),
			Detail: map[string]any{
				"n_live_tup":       t.NLiveTup,
				"writes":           writes,
				"never_analyzed":   neverAnalyzed,
				"last_analyze":     t.LastAnalyze,
				"last_autoanalyze": t.LastAutoanalyze,
			},
			Recommendation: fmt.Sprintf(
				"Run ANALYZE on %s to refresh planner statistics. "+
					"ANALYZE is safe and idempotent.", ident),
			RecommendedSQL: fmt.Sprintf("ANALYZE %s;", q),
			ActionRisk:     "safe",
		})
	}
	return findings
}

// mostRecentAnalyze returns the later of LastAnalyze and LastAutoanalyze,
// or the zero time if the table has never been analyzed.
func mostRecentAnalyze(t collector.TableStats) time.Time {
	var last time.Time
	if t.LastAnalyze != nil && t.LastAnalyze.After(last) {
		last = *t.LastAnalyze
	}
	if t.LastAutoanalyze != nil && t.LastAutoanalyze.After(last) {
		last = *t.LastAutoanalyze
	}
	return last
}
