package analyzer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// workload_anomaly (market-02) scores each cycle's workload against the
// same hour of the week in earlier weeks (internal/baseline): a weekly
// peak is usual, the same value at a quiet hour is not. Detection is
// deterministic, uses no LLM and reads only collected data; the fixed
// threshold rules stay the floor.

// CategoryWorkloadAnomaly is the finding category.
const CategoryWorkloadAnomaly = "workload_anomaly"

// BaselineStore keeps the hour-of-week buckets (baseline.Store).
type BaselineStore interface {
	Accumulate(ctx context.Context, database string, w baseline.Window, at time.Time,
		samples []baseline.Sample) error
	History(ctx context.Context, database string, w baseline.Window, at time.Time,
		keys []baseline.Key) (map[baseline.Key][]float64, error)
}

// Absolute floors of the non-statement metrics: the rise over the bucket
// median a value needs before it can be an anomaly, so a quiet bucket
// does not alarm on a few sessions or a little WAL.
const (
	anomalyBackendFloor = 5
	anomalyWALFloor     = 1 << 20 // bytes per second
	anomalyWaitFloor    = 3
	// anomalyCriticalShare is the share of the interval's DB time a
	// statement needs for a critical anomaly.
	anomalyCriticalShare = 0.10
)

// newBaselineStore is the pool's store; none without a database.
func newBaselineStore(pool *pgxpool.Pool) BaselineStore {
	if pool == nil {
		return nil
	}
	return baseline.NewStore(pool)
}

// WithBaselineStore replaces the baseline store; nil leaves
// workload_anomaly unevaluated.
func (a *Analyzer) WithBaselineStore(s BaselineStore) { a.baselines = s }

// runWorkloadAnomaly scores the cycle, then folds it into the baselines.
// Disabled, the category is evaluated empty (its findings resolve).
// Without a store, a previous snapshot or a readable baseline it is not
// evaluated, so open findings stay open.
func (a *Analyzer) runWorkloadAnomaly(ctx context.Context,
	cur, prev *collector.Snapshot) []Finding {
	a.seasonal = nil
	cfg := a.cfg.Baseline
	if !cfg.Enabled {
		a.anomalies = anomalyTracker{}
		a.eval.evaluated(CategoryWorkloadAnomaly)
		return nil
	}
	if a.baselines == nil || cur == nil || prev == nil {
		a.eval.fail(CategoryWorkloadAnomaly)
		return nil
	}
	cs := workloadSamples(cur, prev, slowQueryMinCalls(a.cfg), cfg.MaxQueryKeys)
	w := baseline.Window{Loc: cfg.Location(), Weeks: cfg.Weeks}
	hist, err := a.baselines.History(ctx, a.databaseName, w, cur.CollectedAt,
		sampleKeys(cs.samples))
	if err != nil {
		a.eval.fail(CategoryWorkloadAnomaly)
		a.logFn("ERROR", "analyzer: workload_anomaly baseline read for %d series: %v",
			len(cs.samples), err)
		return nil
	}
	findings, kept := a.scoreAnomalies(cs, hist, cur.CollectedAt)
	a.eval.evaluated(CategoryWorkloadAnomaly)
	a.eval.keep(CategoryWorkloadAnomaly, kept...)
	if err := a.baselines.Accumulate(ctx, a.databaseName, w, cur.CollectedAt,
		cs.samples); err != nil {
		a.logFn("WARN", "analyzer: workload_anomaly could not fold %d samples into "+
			"the baseline (this cycle is missing from it): %v", len(cs.samples), err)
	}
	return findings
}

// scoreAnomalies judges every sample against its bucket and returns the
// open findings and the identities kept open without a verdict.
func (a *Analyzer) scoreAnomalies(cs cycleSamples, hist map[baseline.Key][]float64,
	at time.Time) ([]Finding, []string) {
	cfg := a.cfg.Baseline
	a.seasonal = map[int64]baseline.Score{}
	seen := make(map[string]bool, len(cs.samples))
	var findings []Finding
	var kept []string
	for _, s := range cs.samples {
		ident := s.Key.Metric + ":" + s.Key.Key
		seen[ident] = true
		floor := anomalyFloor(s.Key.Metric, cfg)
		score := baseline.Judge(hist[s.Key], s.Value, floor)
		if score.Learning() {
			if a.anomalies.unjudged(ident) {
				kept = append(kept, ident)
			}
			continue
		}
		q, isQuery := cs.queries[s.Key]
		if isQuery {
			a.seasonal[q.QueryID] = score
		}
		level := anomalyLevel(score, floor, cs.share[s.Key], isQuery, cfg)
		emit, keep, episode := a.anomalies.judged(ident, level != "", at)
		if keep {
			kept = append(kept, ident)
		}
		if emit {
			findings = append(findings, anomalyFinding(s, score, level, episode, cs))
		}
	}
	return findings, append(kept, a.unseenAnomalies(cs, seen)...)
}

// unseenAnomalies ends the tracked series without a sample this cycle and
// returns those kept open. A statement gone from a snapshot whose
// statements were read is never judged again, so its finding resolves;
// any other series (a category not read, a statement past the key cap or
// idle) is kept open.
func (a *Analyzer) unseenAnomalies(cs cycleSamples, seen map[string]bool) []string {
	var kept []string
	for ident := range a.anomalies {
		if seen[ident] {
			continue
		}
		key, isQuery := strings.CutPrefix(ident, MetricQueryMean+":")
		if isQuery && cs.queriesRead && !cs.present[key] {
			delete(a.anomalies, ident)
			continue
		}
		if a.anomalies.unjudged(ident) {
			kept = append(kept, ident)
		}
	}
	return kept
}

// anomalyFloor is the metric's absolute delta floor.
func anomalyFloor(metric string, cfg config.BaselineConfig) float64 {
	switch metric {
	case MetricQueryMean:
		return cfg.MinDeltaMs
	case MetricWALRate:
		return anomalyWALFloor
	case MetricWaitSessions:
		return anomalyWaitFloor
	}
	return anomalyBackendFloor
}

// anomalyLevel is the severity a score earns: none below z_warning or the
// delta floor; critical at z_critical, for a statement only with at least
// 10% of the interval's DB time; warning otherwise.
func anomalyLevel(s baseline.Score, floor, share float64, isQuery bool,
	cfg config.BaselineConfig) string {
	warn, crit := cfg.ZWarning, cfg.ZCritical
	if warn <= 0 {
		warn = config.DefaultBaselineZWarning
	}
	if crit < warn {
		crit = max(warn, config.DefaultBaselineZCritical)
	}
	if !s.Exceeds(warn, floor) {
		return ""
	}
	if s.Z >= crit && (!isQuery || share >= anomalyCriticalShare) {
		return "critical"
	}
	return "warning"
}

// anomalyFinding is the workload_anomaly finding of sample s.
func anomalyFinding(s baseline.Sample, score baseline.Score, level string,
	episode time.Time, cs cycleSamples) Finding {
	detail := map[string]any{
		"metric": s.Key.Metric, "key": s.Key.Key, "how": s.How,
		"observed": score.Observed, "baseline_median": score.Median, "mad": score.MAD,
		"z": score.Z, "weeks": score.Weeks, "method": baseline.Method,
		"episode_start": episode.UTC().Format(time.RFC3339),
	}
	if q, ok := cs.queries[s.Key]; ok {
		detail["queryid"], detail["query"] = q.QueryID, q.Query
		detail["db_time_share"] = cs.share[s.Key]
	}
	return Finding{
		Category:         CategoryWorkloadAnomaly,
		Severity:         level,
		ObjectType:       "workload",
		ObjectIdentifier: s.Key.Metric + ":" + s.Key.Key,
		Title: fmt.Sprintf("Workload anomaly: %s of %s is %.1f, usually %.1f at this "+
			"hour of the week (z %.1f over %d weeks)", s.Key.Metric, s.Key.Key,
			score.Observed, score.Median, score.Z, score.Weeks),
		Detail: detail,
		Recommendation: "Compare with the same hour in earlier weeks: look for a new " +
			"plan, a data or traffic change, or a deploy since the last normal cycle.",
		ActionRisk: "safe",
	}
}
