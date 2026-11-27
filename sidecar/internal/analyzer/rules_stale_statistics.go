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

// staleWindow is the rule's size floor and age cutoff.
type staleWindow struct {
	minRows   int64
	staleDays int
	cutoff    time.Time
}

func newStaleWindow(cfg *config.Config, now time.Time) staleWindow {
	w := staleWindow{minRows: int64(cfg.Analyzer.AnalyzeStaleMinRows),
		staleDays: cfg.Analyzer.AnalyzeStaleDays}
	if w.minRows <= 0 {
		w.minRows = 10000
	}
	if w.staleDays <= 0 {
		w.staleDays = 7
	}
	w.cutoff = now.AddDate(0, 0, -w.staleDays)
	return w
}

// ruleStaleStatisticsAt judges each table as autoanalyze does (rules-10):
// the rows modified since the last analyze against the autoanalyze
// threshold, never lifetime counters, so a table loaded once and analyzed
// is not re-flagged every week. A table is flagged when it was never
// analyzed and has changed since, or when it changed past its threshold
// and its last analyze is older than the cutoff. Partitioned parents,
// which autoanalyze never processes, are judged from their partitions.
func ruleStaleStatisticsAt(
	current *collector.Snapshot,
	cfg *config.Config,
	now time.Time,
) []Finding {
	if current == nil {
		return nil
	}
	w := newStaleWindow(cfg, now)
	var findings []Finding
	for _, t := range current.Tables {
		if t.Relkind == "p" || t.NLiveTup < w.minRows || t.ModSinceAnalyze <= 0 {
			continue
		}
		last := mostRecentAnalyze(t)
		neverAnalyzed := last.IsZero()
		stale := !neverAnalyzed && last.Before(w.cutoff) &&
			float64(t.ModSinceAnalyze) > t.AnalyzeThreshold
		if !neverAnalyzed && !stale {
			continue
		}
		findings = append(findings, staleTableFinding(t, neverAnalyzed, w.staleDays))
	}
	return append(findings, staleParentFindings(current, w)...)
}

func staleTableFinding(t collector.TableStats, neverAnalyzed bool, staleDays int) Finding {
	ident := t.SchemaName + "." + t.RelName
	reason := "has never been analyzed"
	if !neverAnalyzed {
		reason = fmt.Sprintf("was last analyzed over %d days ago and %d rows "+
			"changed since, past its autoanalyze threshold of %.0f",
			staleDays, t.ModSinceAnalyze, t.AnalyzeThreshold)
	}
	return Finding{
		Category:         "stale_statistics",
		Severity:         "warning",
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Table %s %s; the planner may use bad row estimates", ident, reason),
		Detail: map[string]any{
			"n_live_tup":        t.NLiveTup,
			"writes":            t.NTupIns + t.NTupUpd + t.NTupDel,
			"mod_since_analyze": t.ModSinceAnalyze,
			"analyze_threshold": t.AnalyzeThreshold,
			"never_analyzed":    neverAnalyzed,
			"last_analyze":      t.LastAnalyze,
			"last_autoanalyze":  t.LastAutoanalyze,
		},
		Recommendation: fmt.Sprintf(
			"Run ANALYZE on %s to refresh planner statistics. "+
				"ANALYZE is safe and idempotent.", ident),
		RecommendedSQL: fmt.Sprintf("ANALYZE %s;",
			sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)),
		ActionRisk: "safe",
	}
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
