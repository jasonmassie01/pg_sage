package executor

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// engineOutcome is the outcome ledger entry of a durable index-create
// verdict: the engine's call-weighted comparison of the targets, with
// the verdict it reached (an unknown one is unverifiable).
func (e *Executor) engineOutcome(
	ctx context.Context, actionID int64, v verify.Verdict,
) verify.Outcome {
	verdict := v.Outcome
	switch {
	case verify.DecidedVerdict(verdict):
	case v.Retain && !v.Revert:
		verdict = verify.OutcomeImproved // the engine retains only an improvement
	default:
		verdict = verify.OutcomeUnverifiable
	}
	comparison, _ := v.Evidence["comparison"].(map[string]any)
	reason, _ := comparison["reason"].(string)
	if v.Reason != "" {
		reason = strings.TrimSuffix(v.Reason+": "+reason, ": ")
	}
	now := time.Now()
	o := verify.Outcome{ActionLogID: actionID, Class: verify.ClassIndexCreate,
		Verdict: verdict, Reason: nonEmpty(reason, v.Status), Evidence: v.Evidence,
		WindowEnd: &now, Observed: verify.Observed{Metric: verify.MetricMeanExecTime,
			Before: evidenceMean(comparison, "before"), After: evidenceMean(comparison, "after"),
			ChangePct: v.ObservedPct}}
	if o.Evidence == nil {
		o.Evidence = map[string]any{}
	}
	var executedAt time.Time
	var predicted []byte
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT executed_at,
		before_state->'predicted_effect' FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&executedAt, &predicted)
	if err != nil {
		e.logFn("verify", "read action %d for its outcome: %v", actionID, err)
		return o
	}
	o.WindowStart = &executedAt
	if len(predicted) > 0 && json.Unmarshal(predicted, &o.Predicted) != nil {
		o.Predicted = verify.NoPrediction(verify.ClassIndexCreate, "unreadable prediction")
	}
	return o
}

func evidenceMean(comparison map[string]any, side string) float64 {
	m, _ := comparison[side].(map[string]any)
	mean, _ := m["mean_ms"].(float64)
	return mean
}
