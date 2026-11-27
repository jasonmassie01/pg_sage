package tuner

import (
	"context"
	"strings"
)

// hints returns the current pg_hint_plan facts (nil when never detected).
func (t *Tuner) hints() *HintPlanAvailability {
	t.hintMu.RLock()
	defer t.hintMu.RUnlock()
	return t.hintPlan
}

// refreshHintPlan re-reads the application-effect facts at the start of a
// cycle, so turning enable_hint_table on (or granting the privilege) takes
// effect without a restart, and turning it off stops executable hints.
// Facts a caller supplied (CheckedAt zero) are kept. SessionLoad comes from
// the startup detection: the re-read never runs LOAD. A failed read leaves
// hints not ready.
func (t *Tuner) refreshHintPlan(ctx context.Context) {
	cur := t.hints()
	if t.hintSource == nil || cur == nil || cur.CheckedAt.IsZero() {
		return
	}
	next, err := ProbeHintReadiness(ctx, t.hintSource)
	if err != nil {
		t.logFn("WARN", "tuner: pg_hint_plan readiness re-read failed, hints stay "+
			"advisory: %v", err)
	}
	withSessionLoad(next, cur.SessionLoad)
	if next.ApplicationEffect != cur.ApplicationEffect ||
		strings.Join(next.Reasons, ";") != strings.Join(cur.Reasons, ";") {
		t.logFn("INFO", "tuner: pg_hint_plan application effect %t (was %t): %s",
			next.ApplicationEffect, cur.ApplicationEffect, readinessNote(next))
	}
	t.hintMu.Lock()
	t.hintPlan = next
	t.hintMu.Unlock()
}

// readinessNote is the log text for the facts: "ready" or the reasons.
func readinessNote(a *HintPlanAvailability) string {
	if r := a.Readiness(); r != "" {
		return r
	}
	return "ready"
}
