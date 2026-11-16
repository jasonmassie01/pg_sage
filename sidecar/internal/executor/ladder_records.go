package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// findingLadder is the ladder a finding carries (lab-03a): the tier-1
// record of its own evidence (the what-if of an index create or drop, a
// hint's plan comparison), then the records a producer attached in
// detail["ladder"] (a clone rehearsal). An unreadable value is a skipped
// tier with the error, never a pass.
func findingLadder(f analyzer.Finding, actionType string, now time.Time,
) []verify.LadderRecord {
	var out []verify.LadderRecord
	switch actionType {
	case "create_index_concurrently":
		out = append(out, whatIfLadder(f, verify.MethodLadderWhatIf, now,
			optimizer.HypoPGCannotModel(f.RecommendedSQL)))
	case "drop_unused_index":
		out = append(out, whatIfLadder(f, verify.MethodLadderHide, now, ""))
	case "apply_query_hint":
		if rec, ok := hintLadder(f, now); ok {
			out = append(out, rec)
		}
	}
	attached, err := verify.DecodeLadderValue(f.Detail["ladder"])
	if err != nil {
		return append(out, verify.LadderBuildError(verify.TierClone, "",
			fmt.Errorf("the finding's ladder is unreadable: %w", err), now))
	}
	return append(out, attached...)
}

// whatIfLadder is the tier-1 record of the finding's HypoPG what-if.
func whatIfLadder(f analyzer.Finding, method string, now time.Time,
	cannotModel string) verify.LadderRecord {
	raw, present := f.Detail["what_if_verdict"]
	if !present || raw == nil {
		return verify.LadderBuildError(verify.TierPlanOnly, method,
			errors.New("no HypoPG what-if was recorded for this change"), now)
	}
	verdict, ok := raw.(string)
	if !ok {
		return verify.LadderBuildError(verify.TierPlanOnly, method, fmt.Errorf(
			"what-if result unreadable: what_if_verdict is %T, want a string", raw), now)
	}
	e := verify.WhatIfEvidence{Verdict: verdict, CannotModel: cannotModel}
	e.Reason, _ = f.Detail["what_if_reason"].(string)
	if n, ok := detailFloat(f.Detail["what_if_measured"]); ok {
		e.Measured = int(n)
	}
	e.Unplanned = len(queryIDList(f.Detail["what_if_unplanned_queryids"]))
	if pct, ok := detailFloat(f.Detail["estimated_improvement_pct"]); ok &&
		verdict == optimizer.WhatIfVerified && method == verify.MethodLadderWhatIf {
		change := -pct
		e.ChangePct = &change
	}
	if method == verify.MethodLadderHide {
		return verify.FromDropMeasurement(e, now)
	}
	return verify.FromWhatIf(e, now)
}

func queryIDList(v any) []any {
	switch list := v.(type) {
	case []any:
		return list
	case []int64:
		out := make([]any, len(list))
		for i, id := range list {
			out[i] = id
		}
		return out
	}
	return nil
}

// hintLadder is the tier-1 record of a hint's plan comparison, if any.
func hintLadder(f analyzer.Finding, now time.Time) (verify.LadderRecord, bool) {
	raw, present := f.Detail["hint_evidence"]
	if !present || raw == nil {
		return verify.LadderRecord{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return verify.LadderBuildError(verify.TierPlanOnly, verify.MethodLadderHint,
			fmt.Errorf("hint_evidence is %T, want a JSON object", raw), now), true
	}
	h := verify.HintComparison{}
	h.Verdict, _ = m["verdict"].(string)
	h.Reason, _ = m["reason"].(string)
	h.GenericPlan, _ = m["generic_plan"].(bool)
	if pct, ok := detailFloat(m["cost_delta_pct"]); ok {
		h.CostDeltaPct = &pct
	}
	return verify.FromHintEvidence(h, now), true
}

type queueLadderKey struct{}

// withQueueLadder carries the ladder of the queue item being executed to
// the prediction that records it.
func withQueueLadder(ctx context.Context, recs []verify.LadderRecord) context.Context {
	return context.WithValue(ctx, queueLadderKey{}, recs)
}

func queueLadderFrom(ctx context.Context) []verify.LadderRecord {
	recs, _ := ctx.Value(queueLadderKey{}).([]verify.LadderRecord)
	return recs
}

// actionLadder is the ladder an executed action's prediction carries: its
// queue item's when one is executing, else the one its finding carries.
func actionLadder(ctx context.Context, detail map[string]any, sql string,
	now time.Time) []verify.LadderRecord {
	if recs := queueLadderFrom(ctx); len(recs) > 0 {
		return recs
	}
	f := analyzer.Finding{RecommendedSQL: sql, Detail: detail}
	return findingLadder(f, ActionTypeForSQL(sql), now)
}

// withQueueLadderOf reads a queue item's ladder into ctx; an unreadable
// one is logged and the finding's ladder applies.
func (e *Executor) withQueueLadderOf(ctx context.Context, queueID int) context.Context {
	if e.pool == nil {
		return ctx
	}
	raw, err := verify.ReadLadder(ctx, e.pool, queueID)
	if err == nil {
		var recs []verify.LadderRecord
		if recs, err = verify.ParseLadder(raw); err == nil {
			return withQueueLadder(ctx, recs)
		}
	}
	e.logFn("executor", "queue item %d: ladder unreadable, the finding's applies: %v",
		queueID, err)
	return ctx
}

// recordQueueLadder stores a newly queued finding's ladder on its queue
// item. A failure is logged: the gate already decided, and the card then
// says no pre-production test was recorded.
func (e *Executor) recordQueueLadder(ctx context.Context, queueID int, f analyzer.Finding) {
	if e.pool == nil || queueID <= 0 {
		return
	}
	recs := findingLadder(f, ActionTypeForSQL(f.RecommendedSQL), time.Now())
	if err := verify.AppendLadder(ctx, e.pool, queueID, recs...); err != nil {
		e.logFn("executor", "record the ladder of queue item %d: %v", queueID, err)
	}
}

// appendVerdictTier appends the production verdict (tier 3) to the
// ladder of the queue item that ran the action, if one did.
func appendVerdictTier(ctx context.Context, pool *pgxpool.Pool, o verify.Outcome,
	logFn func(string, string, ...any)) {
	if pool == nil || o.ActionLogID <= 0 {
		return
	}
	rec := verify.FromVerdict(o.Verdict, o.Observed.ChangePct, o.ActionLogID, time.Now())
	if _, err := verify.AppendLadderForAction(ctx, pool, o.ActionLogID, rec); err != nil {
		logFn("verify", "record the tier-3 verdict of action %d: %v", o.ActionLogID, err)
	}
}
