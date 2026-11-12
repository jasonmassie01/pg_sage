package analyzer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// query_regression (rules-05) compares a statement's interval mean with
// the median of its hourly interval means over the regression lookback,
// read from sage.query_store. Only the statements whose interval mean is
// already above their cumulative mean by the threshold are read, at most
// maxRegressionCandidates of them, so a steady cycle reads nothing.

// maxRegressionCandidates caps the statements one cycle reads a baseline
// for, the largest interval total time first.
const maxRegressionCandidates = 50

// minBaselineBuckets is the fewest usable hourly buckets a baseline needs.
const minBaselineBuckets = 6

// regressionBaselineBucket is the baseline's bucket width.
const regressionBaselineBucket = time.Hour

// defaultRegressionLookbackDays applies when the configured lookback is
// zero or negative.
const defaultRegressionLookbackDays = 7

// regressionCandidate is a statement whose interval mean regressed against
// its own cumulative mean.
type regressionCandidate struct {
	q        collector.QueryStats
	interval queryInterval
}

// regressionBaseline is the baseline mean, how many buckets it is the
// median of and how it was built (empty: regressionMethodFlat).
type regressionBaseline struct {
	MeanMs  float64
	Buckets int
	Method  string
}

// Baseline methods: the flat median of the hourly interval means over the
// lookback, or the median of the same hour of the week in earlier weeks
// when that bucket is active (market-02, baseline.weeks).
const (
	regressionMethodFlat       = "median_hourly_interval"
	regressionMethodHourOfWeek = "hour_of_week"
)

// seasonalBaselines is flat with each active hour-of-week bucket in
// seasonal taking the place of its statement's flat baseline: a weekly
// peak is judged against earlier weeks' peaks, not the week's median.
func seasonalBaselines(flat map[int64]regressionBaseline,
	seasonal map[int64]baseline.Score) map[int64]regressionBaseline {
	out := make(map[int64]regressionBaseline, len(flat)+len(seasonal))
	for id, b := range flat {
		out[id] = b
	}
	for id, s := range seasonal {
		if !s.Learning() && s.Median > 0 {
			out[id] = regressionBaseline{MeanMs: s.Median, Buckets: s.Weeks,
				Method: regressionMethodHourOfWeek}
		}
	}
	return out
}

// regressionMultiplier is 1 + regression_threshold_pct / 100.
func regressionMultiplier(cfg *config.Config) float64 {
	return 1.0 + float64(cfg.Analyzer.RegressionThresholdPct)/100.0
}

// regressionCandidates returns the statements whose interval mean, over at
// least the call floor, exceeds their cumulative mean by the regression
// threshold: at most maxRegressionCandidates, largest interval total
// time first. None when the snapshots do not measure an interval.
func regressionCandidates(current, previous *collector.Snapshot,
	cfg *config.Config) []regressionCandidate {
	if !hasQueryInterval(current, previous) {
		return nil
	}
	minCalls, multiplier := slowQueryMinCalls(cfg), regressionMultiplier(cfg)
	before := queriesByID(previous)
	var out []regressionCandidate
	for _, q := range adviceQueries(current) {
		iv, ok := measuredInterval(q, before, minCalls)
		if !ok || iv.MeanMs <= q.MeanExecTime*multiplier {
			continue
		}
		out = append(out, regressionCandidate{q: q, interval: iv})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].interval.TotalMs > out[j].interval.TotalMs
	})
	if len(out) > maxRegressionCandidates {
		out = out[:maxRegressionCandidates]
	}
	return out
}

// baselineFromIntervals is the median interval mean of the buckets with at
// least minCalls calls, no epoch break and no negative time; none with
// fewer than minBaselineBuckets such buckets.
func baselineFromIntervals(ivs []querystore.Interval, minCalls int64) (regressionBaseline,
	bool) {
	means := make([]float64, 0, len(ivs))
	for _, iv := range ivs {
		if iv.EpochBreak || iv.Calls < minCalls || iv.Calls <= 0 || iv.TotalMs < 0 {
			continue
		}
		means = append(means, iv.TotalMs/float64(iv.Calls))
	}
	if len(means) < minBaselineBuckets {
		return regressionBaseline{}, false
	}
	sort.Float64s(means)
	mid := len(means) / 2
	median := means[mid]
	if len(means)%2 == 0 {
		median = (means[mid-1] + means[mid]) / 2
	}
	return regressionBaseline{MeanMs: median, Buckets: len(means)}, true
}

// ruleQueryRegression flags the candidates whose interval mean exceeds
// their baseline by the regression threshold. A candidate without a
// baseline is not judged.
func ruleQueryRegression(cands []regressionCandidate,
	baselines map[int64]regressionBaseline, cfg *config.Config) []Finding {
	multiplier := regressionMultiplier(cfg)
	var findings []Finding
	for _, c := range cands {
		b, ok := baselines[c.q.QueryID]
		if !ok || b.MeanMs <= 0 || c.interval.MeanMs <= b.MeanMs*multiplier {
			continue
		}
		findings = append(findings, regressionFinding(c, b))
	}
	return findings
}

// regressionFinding is the query_regression finding for c against b.
func regressionFinding(c regressionCandidate, b regressionBaseline) Finding {
	pct := (c.interval.MeanMs - b.MeanMs) / b.MeanMs * 100.0
	method, against := b.Method, "hourly median"
	if method == regressionMethodHourOfWeek {
		against = "median at this hour of the week"
	} else {
		method = regressionMethodFlat
	}
	severity := "warning"
	if pct > 200 {
		severity = "critical"
	}
	return Finding{
		Category:         "query_regression",
		Severity:         severity,
		ObjectType:       "query",
		ObjectIdentifier: queryIdent(c.q.QueryID),
		Title: fmt.Sprintf("Query regression: %.1fms now vs %.1fms %s (+%.0f%%)",
			c.interval.MeanMs, b.MeanMs, against, pct),
		Detail: map[string]any{
			"queryid":            c.q.QueryID,
			"query":              c.q.Query,
			"interval_mean_ms":   c.interval.MeanMs,
			"interval_calls":     c.interval.Calls,
			"cumulative_mean_ms": c.q.MeanExecTime,
			"baseline_ms":        b.MeanMs,
			"baseline_method":    method,
			"baseline_buckets":   b.Buckets,
			"pct_increase":       pct,
		},
		Recommendation: "Investigate cause of performance regression.",
		ActionRisk:     "safe",
	}
}

// runQueryRegression runs query_regression for the cycle. Without a
// measurable interval, or when the baseline cannot be read, the category
// is marked not evaluated, so its open findings are not resolved.
func (a *Analyzer) runQueryRegression(ctx context.Context,
	current, previous *collector.Snapshot) []Finding {
	if !hasQueryInterval(current, previous) {
		a.evalFail("query_regression")
		return nil
	}
	cands := regressionCandidates(current, previous, a.cfg)
	baselines := map[int64]regressionBaseline{}
	if len(cands) > 0 {
		ids := make([]int64, len(cands))
		for i, c := range cands {
			ids[i] = c.q.QueryID
		}
		var err error
		baselines, err = a.regressionBaselines(ctx, ids, current.CollectedAt)
		if err != nil {
			a.evalFail("query_regression")
			a.logFn("ERROR", "analyzer: query_regression baseline read for %d queries: %v",
				len(ids), err)
			return nil
		}
	}
	baselines = seasonalBaselines(baselines, a.seasonal)
	a.eval.keep("query_regression", unjudgedRegressions(current, previous, a.cfg, cands,
		baselines)...)
	return ruleQueryRegression(cands, baselines, a.cfg)
}

// unjudgedRegressions are the identities of the current statements that
// query_regression gave no verdict this cycle: those without a measured
// interval (undecidedQueries), and the candidates without a baseline or
// past the candidate cap. Their open findings are kept. A statement whose
// measured interval mean is within the threshold of its cumulative mean,
// or a candidate compared with its baseline, is judged.
func unjudgedRegressions(current, previous *collector.Snapshot, cfg *config.Config,
	cands []regressionCandidate, baselines map[int64]regressionBaseline) []string {
	minCalls, multiplier := slowQueryMinCalls(cfg), regressionMultiplier(cfg)
	out := undecidedQueries(current, previous, minCalls)
	judged := make(map[int64]bool, len(cands))
	for _, c := range cands {
		if b, ok := baselines[c.q.QueryID]; ok && b.MeanMs > 0 {
			judged[c.q.QueryID] = true
		}
	}
	before := queriesByID(previous)
	for _, q := range adviceQueries(current) {
		iv, ok := measuredInterval(q, before, minCalls)
		if ok && iv.MeanMs > q.MeanExecTime*multiplier && !judged[q.QueryID] {
			out = append(out, queryIdent(q.QueryID))
		}
	}
	return out
}

// regressionBaselines reads the hourly intervals of ids over the
// regression lookback before now, excluding the last hour, and returns
// the baseline of every query that has one.
func (a *Analyzer) regressionBaselines(ctx context.Context, ids []int64,
	now time.Time) (map[int64]regressionBaseline, error) {
	days := a.cfg.Analyzer.RegressionLookbackDays
	if days <= 0 {
		days = defaultRegressionLookbackDays
	}
	if now.IsZero() {
		now = time.Now()
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	to := now.Add(-regressionBaselineBucket)
	ivs, err := querystore.QueryIntervalsMany(ctx, a.pool, ids, from, to,
		regressionBaselineBucket)
	if err != nil {
		return nil, err
	}
	minCalls := slowQueryMinCalls(a.cfg)
	out := make(map[int64]regressionBaseline, len(ivs))
	for qid, qivs := range ivs {
		if b, ok := baselineFromIntervals(qivs, minCalls); ok {
			out[qid] = b
		}
	}
	return out, nil
}
