package executor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// metricJudgement is a class metric's verdict (a config change's temp
// spills, dead tuples or HOT share; VACUUM/ANALYZE counters). Terminal
// marks an insufficient verdict more time cannot change (nothing spilled
// before, statistics reset, counters missing).
type metricJudgement struct {
	Verdict     string
	ObservedPct *float64
	Reason      string
	Metric      string
	Before      float64
	After       float64
	Terminal    bool
}

// judgement is one check's outcome and whether more time can add
// evidence.
type judgement struct {
	outcome  verify.Outcome
	accruing bool
}

// judgeMonitored judges an action at now.
func judgeMonitored(
	ctx context.Context, pool *pgxpool.Pool, p monitorPlan, cfg RollbackMonitorConfig,
	now time.Time,
) verify.Outcome {
	return p.judge(ctx, pool, cfg, now).outcome
}

func (p monitorPlan) judge(
	ctx context.Context, pool *pgxpool.Pool, cfg RollbackMonitorConfig, now time.Time,
) judgement {
	start, end := p.executedAt, now
	o := verify.Outcome{ActionLogID: p.actionID, Class: p.class, Predicted: p.prediction,
		WindowStart: &start, WindowEnd: &end, Evidence: map[string]any{}}
	queries := p.judgeQueries(ctx, pool, cfg, now, o.Evidence)
	metric := p.judgeMetric(ctx, pool, now, o.Evidence)
	if miss := p.softDropMiss(ctx, pool, queries); miss != "" {
		o.Verdict, o.Reason = verify.OutcomeRegressed, softDropReason(miss, queries)
		o.Evidence["soft_drop"] = map[string]any{"trigger": miss,
			"definition": p.rollbackSQL, "first_miss_at": now.UTC()}
	} else {
		o.Verdict, o.Reason = combineVerdict(p.class, p.prediction, queries, metric)
	}
	o.Observed = observedFrom(queries, metric)
	accruing := p.softDrop || queries != nil && queries.Verdict == verify.OutcomeInsufficient ||
		metric != nil && metric.Verdict == verify.OutcomeInsufficient && !metric.Terminal
	return judgement{outcome: o, accruing: accruing}
}

// combineVerdict decides an action from its targeted queries and its
// class metric. A regression of either is a regression, prediction or
// not; without a prediction nothing is credited; a drop whose reads held
// delivered its predicted effect (improved); otherwise the class metric,
// then the queries, decide.
func combineVerdict(
	class string, p verify.Prediction, queries *verify.Comparison, metric *metricJudgement,
) (string, string) {
	if queries != nil && queries.Verdict == verify.OutcomeRegressed {
		return verify.OutcomeRegressed, nonEmpty(queries.Reason, "targeted queries regressed")
	}
	if metric != nil && metric.Verdict == verify.OutcomeRegressed {
		return verify.OutcomeRegressed, nonEmpty(metric.Reason, "targeted metric regressed")
	}
	if !p.Predicts() {
		return verify.OutcomeUnverifiable, "no prediction (" +
			nonEmpty(p.Note, "none recorded") + "): not credited"
	}
	if class == verify.ClassIndexDrop {
		if queries == nil {
			return verify.OutcomeInsufficient, "no targeted query was measured; reads " +
				"after the drop are unproven"
		}
		if queries.Verdict == verify.OutcomeImproved || queries.Verdict == verify.OutcomeNeutral {
			return verify.OutcomeImproved, "reads held after the drop: " + queries.Reason
		}
		return queries.Verdict, queries.Reason
	}
	if metric != nil {
		return metric.Verdict, nonEmpty(metric.Reason, "targeted metric: "+metric.Verdict)
	}
	if queries != nil {
		return queries.Verdict, nonEmpty(queries.Reason, "targeted queries: "+queries.Verdict)
	}
	return verify.OutcomeUnverifiable, "nothing measurable for this action: not credited"
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// judgeQueries compares the targeted queries' call-weighted means after
// the action with the frozen baseline (or, for actions recorded without
// one, the same-length window before the action).
func (p monitorPlan) judgeQueries(
	ctx context.Context, pool *pgxpool.Pool, cfg RollbackMonitorConfig, now time.Time,
	evidence map[string]any,
) *verify.Comparison {
	if len(p.targets) == 0 {
		return nil
	}
	observe := cfg.observer(pool)
	before := p.baseline
	if before == nil {
		length := max(now.Sub(p.executedAt), time.Hour)
		measured, err := observe(ctx, p.targets, p.executedAt.Add(-length), p.executedAt)
		if err != nil {
			return unavailable("pre-action samples unavailable: " + err.Error())
		}
		before = measured
	}
	after, err := observe(ctx, p.targets, p.executedAt, now)
	if err != nil {
		return unavailable("post-action samples unavailable: " + err.Error())
	}
	th := verify.Thresholds{MinSamples: cfg.MinCalls, GainPct: cfg.GainPct,
		RegressPct: float64(cfg.ThresholdPct)}
	pooled, per := verify.DecideTargets(before, after, p.targets, th)
	evidence["comparison"] = pooled.Evidence()
	evidence["targets"] = verify.TargetsEvidence(per)
	return &pooled
}

func unavailable(reason string) *verify.Comparison {
	return &verify.Comparison{Verdict: verify.OutcomeInsufficient, Reason: reason}
}

// judgeMetric judges a config change's targeted metric against the
// counters recorded when it was applied.
func (p monitorPlan) judgeMetric(
	ctx context.Context, pool *pgxpool.Pool, now time.Time, evidence map[string]any,
) *metricJudgement {
	if p.config == nil {
		return nil
	}
	current, err := readOutcomeCounters(ctx, pool, p.config.Metric, p.config.Table)
	if err != nil {
		return &metricJudgement{Verdict: verify.OutcomeUnverifiable, Terminal: true,
			Metric: p.config.Metric, Reason: "outcome metric unavailable: " + err.Error()}
	}
	j := judgeOutcome(*p.config, current, now)
	evidence["metric"] = map[string]any{"metric": j.Metric, "verdict": j.Verdict,
		"reason": j.Reason, "before": j.Before, "after": j.After, "terminal": j.Terminal}
	return &j
}

// softDropMiss is why a dropped index must come back now ("" when it need
// not): a targeted query regressed, or an active pg_sage hint names the
// index (its plan directive now fails silently).
func (p monitorPlan) softDropMiss(
	ctx context.Context, pool *pgxpool.Pool, queries *verify.Comparison,
) string {
	if !p.softDrop {
		return ""
	}
	if queries != nil && queries.Verdict == verify.OutcomeRegressed {
		return "query_regression"
	}
	if p.index == "" {
		return ""
	}
	var named bool
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.query_hints
		WHERE status = 'active' AND strpos(hint_text, $1) > 0)`, p.index).Scan(&named)
	if err == nil && named {
		return "hint_reference"
	}
	return ""
}

func softDropReason(miss string, queries *verify.Comparison) string {
	if miss == "query_regression" && queries != nil {
		return "soft drop missed: " + queries.Reason
	}
	return "soft drop missed: an active pg_sage hint names the dropped index"
}

// observedFrom is what the verdict observed: the class metric when there
// is one, else the targets' call-weighted mean.
func observedFrom(queries *verify.Comparison, metric *metricJudgement) verify.Observed {
	if metric != nil {
		return verify.Observed{Metric: metric.Metric, Before: metric.Before,
			After: metric.After, ChangePct: metric.ObservedPct}
	}
	if queries == nil {
		return verify.Observed{}
	}
	o := verify.Observed{Metric: verify.MetricMeanExecTime,
		Before: msOf(queries.Before.AverageLatency), After: msOf(queries.After.AverageLatency)}
	if queries.Before.Samples > 0 && queries.After.Samples > 0 && o.Before > 0 {
		delta := (o.After - o.Before) * 100 / o.Before
		o.ChangePct = &delta
	}
	return o
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
