package probes

import (
	"context"
	"testing"
	"time"
)

// sre-06 review: a caller that gives up while its probe waits (a lost
// lease, a shutdown) was not refused a slot. Its wait counts; it is not a
// refusal, so a sidecar restart does not read as investigations losing
// their slots. A caller deadline shorter than the queue wait is still a
// probe that found no slot in the time it had: a refusal.
// No concurrency test here: the concurrent counting is pinned in
// queue_metrics_test.go.

func TestQueueCancelledCallerIsAWaitNotARefusal(t *testing.T) {
	const db = "cancelled_caller"
	global := NewLimiter(MaxSidecarConcurrency)
	defer fill(global, MaxSidecarConcurrency)()
	r := NewRunner(lazyPool(t, db), Catalog(), global)
	r.queueWait = 5 * time.Second
	ctx, cancel := context.WithCancel(WithCaller(context.Background(),
		CallerInvestigation))
	time.AfterFunc(50*time.Millisecond, cancel)
	began := time.Now()
	res := r.Run(ctx, LockGraph, Args{})
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("Run waited %s after the caller gave up", waited)
	}
	if res.Phase != PhaseQueue || res.Status.Usable() {
		t.Fatalf("result = %+v, want a queue-phase failure", res)
	}
	got := statFor(db, CallerInvestigation, LimiterSidecar)
	if got.Waits != 1 || got.Refusals != 0 || got.WaitSeconds < 0.04 {
		t.Fatalf("sidecar stat = %+v, want one ~50 ms wait and no refusal", got)
	}
}

func TestQueueCallerDeadlineUnderQueueWaitIsARefusal(t *testing.T) {
	const db = "short_deadline_caller"
	global := NewLimiter(1)
	defer fill(global, 1)()
	r := NewRunner(lazyPool(t, db), Catalog(), global)
	ctx, cancel := context.WithTimeout(WithCaller(context.Background(), CallerNarration),
		60*time.Millisecond)
	defer cancel()
	res := r.Run(ctx, LockGraph, Args{})
	if !QueueRefused(res) {
		t.Fatalf("result = %+v, want a queue refusal", res)
	}
	got := statFor(db, CallerNarration, LimiterSidecar)
	if got.Waits != 1 || got.Refusals != 1 {
		t.Fatalf("sidecar stat = %+v, want one wait and one refusal", got)
	}
}
