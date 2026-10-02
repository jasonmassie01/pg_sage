package sre

import (
	"testing"
	"time"
)

// A model call slower than the lease TTL keeps the lease alive (the
// worker heartbeats during the call), so the review is stored and the
// investigation concludes under the same worker. This one stays on the
// wall clock because the heartbeats are what it tests: the call lasts
// two TTLs, so an unrenewed lease always expires, while the 3 s TTL
// leaves 2 s of slack per heartbeat (every TTL/3) on a loaded host.
func TestModelTurn_SlowCallKeepsTheLease(t *testing.T) {
	limits := budgetLimits()
	limits.LeaseTTL = 3 * time.Second
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, slowReply(2*limits.LeaseTTL, toolReply(validIdleReview(t))))
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
