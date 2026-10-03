package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// prediction is a retention batch's predicted effect: it deletes the
// candidate rows the reviewed dry run counted, up to its bound.
func (r *retentionRun) prediction() verify.Prediction {
	candidates := r.request.Candidates
	p := verify.Prediction{Class: verify.ClassRetention, Method: verify.MethodRule,
		Metric: verify.MetricRowsDeleted, Source: "retention",
		Note: "the batch deletes the reviewed candidate rows, up to its bound"}
	if candidates <= 0 {
		return verify.NoPrediction(verify.ClassRetention, "the dry run counted no candidates")
	}
	base := float64(candidates)
	expected := -float64(min(candidates, r.maxRows())) * 100 / base
	p.Baseline, p.ExpectedChangePct = &base, &expected
	return p
}

// failedVerdict judges a batch whose verification failed: rows outside
// the predicate, the relation or the bound are a regression; a record
// that could not be confirmed is unverifiable.
func (r *retentionRun) failedVerdict(err error) metricJudgement {
	o := r.outcome
	if o.OutsidePredicate != 0 || o.OutsideRelation != 0 || o.Deleted < 0 ||
		o.Deleted > r.maxRows() {
		return retentionVerdict(o.Deleted, r.request.Candidates, err)
	}
	return metricJudgement{Verdict: verify.OutcomeUnverifiable, Reason: err.Error(),
		Metric: verify.MetricRowsDeleted}
}

// recordOutcome writes the batch's predicted-vs-observed verdict.
func (r *retentionRun) recordOutcome(ctx context.Context, actionID int64, j metricJudgement) {
	now := time.Now()
	recordVerdict(ctx, r.executor.pool, verify.Outcome{ActionLogID: actionID,
		Class: verify.ClassRetention, Verdict: j.Verdict, Reason: j.Reason, WindowEnd: &now,
		Observed: verify.Observed{Metric: j.Metric, Before: j.Before, After: j.After,
			ChangePct: j.ObservedPct},
		Evidence: map[string]any{"retention_run_id": r.outcome.RunID,
			"max_rows": r.maxRows()}}, r.executor.logFn)
}
