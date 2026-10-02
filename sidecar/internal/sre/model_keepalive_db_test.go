package sre

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// A model call slower than the lease TTL keeps the lease alive (the
// worker heartbeats during the call), so the review is stored and the
// investigation concludes under the same worker. This one stays on the
// wall clock because the heartbeats are what it tests: the call lasts
// 1.5 TTLs, so an unrenewed lease always expires, while the 6 s TTL
// only expires if the heartbeat goroutine (every TTL/3) is starved for
// more than 6 s; a 3 s TTL lost its lease to a 3.6 s stall under load.
// The client's HTTP timeout is raised above the 9 s call (the shared
// test client allows 5 s).
func TestModelTurn_SlowCallKeepsTheLease(t *testing.T) {
	limits := budgetLimits()
	limits.LeaseTTL = 6 * time.Second
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, slowReply(limits.LeaseTTL*3/2, toolReply(validIdleReview(t))))
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: m.srv.URL, APIKey: "k",
		Model: "m", TimeoutSeconds: 30, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), client)
	c.cfg.ModelTimeout = 20 * time.Second
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
