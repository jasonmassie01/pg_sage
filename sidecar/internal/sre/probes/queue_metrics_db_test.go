package probes

import (
	"context"
	"sync"
	"testing"
	"time"
)

// sre-06 integration: twenty background runners holding slow reads on a
// real server fill the four sidecar slots. An investigation probe is
// refused and counted; once the holders finish, the same probe (the
// coordinator's one retry) reads a usable result.
func TestQueueMetrics_TwentyRunnersAndASlowProbe(t *testing.T) {
	pool, ctx := livePool(t)
	global := NewLimiter(MaxSidecarConcurrency)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		r := NewRunner(pool, Catalog(), global)
		r.SetDatabaseName("int_background")
		r.queueWait = 200 * time.Millisecond
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.WithSlot(WithCaller(ctx, CallerRunway), func(c context.Context) error {
				_, err := pool.Exec(c, "SELECT pg_catalog.pg_sleep(0.05)")
				<-release
				return err
			})
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(global.slots) < MaxSidecarConcurrency && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	inv := NewRunner(pool, Catalog(), global)
	inv.SetDatabaseName("int_investigation")
	inv.queueWait = 100 * time.Millisecond
	ictx := WithCaller(ctx, CallerInvestigation)
	if res := inv.Run(ictx, LockGraph, Args{}); !QueueRefused(res) {
		t.Fatalf("first read = %+v, want a queue refusal while 4 holders run", res)
	}
	time.Sleep(300 * time.Millisecond) // the waiting holders give up
	close(release)
	wg.Wait()
	res := inv.Run(ictx, LockGraph, Args{})
	if !res.Status.Usable() {
		t.Fatalf("retried read = %+v, want a usable result", res)
	}
	got := statFor("int_investigation", CallerInvestigation, LimiterSidecar)
	if got.Refusals != 1 || got.Waits != 2 || got.WaitSeconds <= 0 {
		t.Fatalf("investigation stat = %+v, want 1 refusal of 2 waits", got)
	}
	bg := statFor("int_background", CallerRunway, LimiterSidecar)
	if bg.Refusals != 20-MaxSidecarConcurrency || bg.Waits != 20 {
		t.Fatalf("background stat = %+v, want %d refusals of 20 waits", bg,
			20-MaxSidecarConcurrency)
	}
	if len(global.slots) != 0 {
		t.Fatalf("sidecar slots held after the run: %d", len(global.slots))
	}
}
