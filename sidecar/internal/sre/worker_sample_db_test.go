package sre

import (
	"context"
	"testing"
	"time"
)

// The wait between compared samples (connection and WAL investigations)
// may be as long as the lease TTL: sre.sample_interval_seconds allows
// 30 s and the TTL is 30 s. The worker must keep its lease through the
// wait, or the investigation is orphaned at every attempt and never
// concludes. The ratio here is the configured maximum (wait == TTL); a
// wait that long expires an unrenewed lease regardless of load. The 6 s
// TTL keeps the heartbeats (every TTL/3) safe from a multi-second
// scheduling stall on a loaded host.
func TestCollect_SampleWaitAsLongAsTheLeaseKeepsTheLease(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 6 * time.Second
	st, _, ctx := liveStore(t, limits)
	c, _ := testCoordinator(t, ctx, st, leakRunner(), nil)
	c.cfg.SampleInterval = limits.LeaseTTL
	c.sleep = sleepCtx
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:conn-wait",
		Kind: TriggerConnections, Subject: "incident 11", IdempotencyKey: "incident:11"})
	if inv.State != StateConcluded || inv.Summary.Root != "connection_leak" {
		t.Fatalf("investigation after a TTL-long sample wait = %+v, want concluded "+
			"connection_leak", inv)
	}
	if claims := payloads(t, st, inv, EventClaimed); len(claims) != 1 {
		t.Fatalf("claimed %d times, want one uninterrupted lease", len(claims))
	}
}

// A lease lost during the sample wait (an operator stopped the
// investigation) ends the wait at the next heartbeat instead of holding
// the worker for the whole interval, and nothing more is committed.
func TestCollect_LeaseLostDuringSampleWaitEndsTheWait(t *testing.T) {
	limits := DefaultLimits()
	limits.LeaseTTL = 3 * time.Second
	st, _, ctx := liveStore(t, limits)
	runner := leakRunner()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	c.cfg.SampleInterval = 25 * time.Second
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, Trigger{CaseID: "case:conn-stop",
		Kind: TriggerConnections, Subject: "incident 12", IdempotencyKey: "incident:12"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var waited time.Duration
	c.sleep = func(wctx context.Context, d time.Duration) error {
		cur, gerr := st.Get(ctx, scope, inv.ID)
		if gerr != nil {
			t.Errorf("get: %v", gerr)
		} else if _, serr := st.Stop(ctx, scope, inv.ID, cur.Version); serr != nil {
			t.Errorf("stop: %v", serr)
		}
		began := time.Now()
		defer func() { waited = time.Since(began) }()
		return sleepCtx(wctx, d)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate = %v, want a quiet exit after the stop", err)
	}
	if waited <= 0 || waited >= c.cfg.SampleInterval {
		t.Fatalf("the wait lasted %v, want it cut short of %v by the lost lease",
			waited, c.cfg.SampleInterval)
	}
	plan, _ := planFor(TriggerConnections, c.cfg.ActionWindow)
	first := len(plan[0].calls)
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateCancelled || got.ProbeCount != first || runner.total() != first {
		t.Fatalf("after a stop during the wait: %+v with %d probe calls, want "+
			"cancelled with the first step's %d probes only", got, runner.total(), first)
	}
}
