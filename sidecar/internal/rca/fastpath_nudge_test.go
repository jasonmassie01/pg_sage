package rca

import (
	"context"
	"sync"
	"testing"
	"time"
)

// migration-05: the migration detector nudges the fast path when a
// tracked DDL waits on a lock, so the next lock-chain observation comes
// forward instead of waiting a whole interval. Nudges are rate-limited.

type nudgeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nudgeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *nudgeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func startNudgeTicker(t *testing.T) (*LockChainTicker, *nudgeClock, <-chan struct{}) {
	t.Helper()
	clock := &nudgeClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	ticks := make(chan struct{}, 8)
	lt := NewLockChainTicker(testEngine(), nil, nil, time.Hour, (&logSink{}).log)
	lt.now = clock.Now
	ready := make(chan struct{})
	lt.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		close(ready)
		return make(chan time.Time), func() {}
	}
	lt.tick = func(context.Context) error {
		ticks <- struct{}{}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { lt.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not start")
	}
	return lt, clock, ticks
}

// Boundary: one nudge per 10 s; a second inside the window is dropped,
// one at exactly 10 s runs.
func TestLockChainTicker_NudgeRateLimited(t *testing.T) {
	lt, clock, ticks := startNudgeTicker(t)
	if !lt.Nudge() {
		t.Fatal("first nudge refused")
	}
	waitTick(t, ticks, true)
	clock.Add(9 * time.Second)
	if lt.Nudge() {
		t.Fatal("nudge 9s after the last was accepted, want rate-limited")
	}
	waitTick(t, ticks, false)
	clock.Add(time.Second)
	if !lt.Nudge() {
		t.Fatal("nudge 10s after the last was refused")
	}
	waitTick(t, ticks, true)
}

// Nil/disabled: a nil ticker or a disabled fast path ignores nudges.
func TestLockChainTicker_NudgeDisabled(t *testing.T) {
	var nilTicker *LockChainTicker
	if nilTicker.Nudge() {
		t.Fatal("nil ticker accepted a nudge")
	}
	off := NewLockChainTicker(testEngine(), nil, nil, 0, (&logSink{}).log)
	if off.Nudge() {
		t.Fatal("disabled fast path accepted a nudge")
	}
}

// Concurrent access: many detectors nudging at once queue one tick.
func TestLockChainTicker_ConcurrentNudgesQueueOne(t *testing.T) {
	lt := NewLockChainTicker(testEngine(), nil, nil, time.Hour, (&logSink{}).log)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lt.Nudge() {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 || len(lt.nudges) != 1 {
		t.Fatalf("accepted %d nudges with %d queued, want exactly 1", accepted,
			len(lt.nudges))
	}
}
