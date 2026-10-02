package sre

import (
	"testing"
	"time"
)

// A model call slower than the lease TTL keeps the lease alive (the
// worker heartbeats during the call), so the review is stored and the
// investigation concludes under the same worker.
func TestModelTurn_SlowCallKeepsTheLease(t *testing.T) {
	limits := budgetLimits()
	limits.LeaseTTL = time.Second
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, slowReply(2500*time.Millisecond, toolReply(validIdleReview(t))))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	c.cfg.ModelTimeout = 10 * time.Second
	inv := startAndRun(t, ctx, c, lockTrigger("m3-slow"))
	assertIdleRoot(t, st, inv)
	if inv.Summary.ModelRanking == nil || m.calls() != 1 {
		t.Fatalf("slow review not stored: %+v (calls %d)", inv.Summary, m.calls())
	}
	claims := payloads(t, st, inv, EventClaimed)
	if len(claims) != 1 {
		t.Fatalf("claimed %d times, want one uninterrupted lease", len(claims))
	}
}
