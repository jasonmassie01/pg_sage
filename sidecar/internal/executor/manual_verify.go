package executor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// detailMap decodes an approved action's finding detail; nil when absent
// or unreadable (the action then has no producer prediction).
func detailMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var detail map[string]any
	if json.Unmarshal(raw, &detail) != nil {
		return nil
	}
	return detail
}

// coveredIndexOutcome is the verdict of an approved CREATE INDEX that ran
// nothing because an index already covers its columns: nothing changed,
// so nothing is credited.
func coveredIndexOutcome(actionID int64) verify.Outcome {
	now := time.Now()
	return verify.Outcome{ActionLogID: actionID, Class: verify.ClassIndexCreate,
		Verdict: verify.OutcomeUnverifiable, WindowStart: &now, WindowEnd: &now,
		Reason: "no change: an existing index already covers these columns"}
}

// recordUnverified closes the outcome of an action that ended without a
// measurable effect (a config change that did not take effect or needs a
// restart), so no prediction stays pending.
func (e *Executor) recordUnverified(ctx context.Context, actionID int64, reason string) {
	now := time.Now()
	recordVerdict(ctx, e.pool, verify.Outcome{ActionLogID: actionID,
		Verdict: verify.OutcomeUnverifiable, Reason: reason, WindowEnd: &now}, e.logFn)
}
