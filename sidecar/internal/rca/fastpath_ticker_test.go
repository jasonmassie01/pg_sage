package rca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// fakeClock drives LockChainTicker.Run deterministically: Advance emits
// one tick each time elapsed time crosses a multiple of the period.
type fakeClock struct {
	mu      sync.Mutex
	period  time.Duration
	elapsed time.Duration
	ch      chan time.Time
	ready   chan struct{}
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{ready: make(chan struct{})}
}

func (f *fakeClock) newTicker(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.period = d
	f.ch = make(chan time.Time)
	close(f.ready)
	return f.ch, func() {
		f.mu.Lock()
		f.stopped = true
		f.mu.Unlock()
	}
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	before := f.elapsed / f.period
	f.elapsed += d
	after := f.elapsed / f.period
	ch := f.ch
	f.mu.Unlock()
	for i := before; i < after; i++ {
		ch <- time.Now()
	}
}

func (f *fakeClock) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func waitTick(t *testing.T, ticks <-chan struct{}, want bool) {
	t.Helper()
	select {
	case <-ticks:
		if !want {
			t.Fatal("tick ran before the interval elapsed")
		}
	case <-time.After(100 * time.Millisecond):
		if want {
			t.Fatal("tick did not run when the interval elapsed")
		}
	}
}

func startFakeTicker(
	t *testing.T, interval time.Duration, tickErr error,
) (*fakeClock, <-chan struct{}, context.CancelFunc, <-chan struct{}, *logSink) {
	t.Helper()
	clock := newFakeClock()
	ticks := make(chan struct{}, 8)
	sink := &logSink{}
	lt := NewLockChainTicker(testEngine(), nil, nil, interval, sink.log)
	lt.newTicker = clock.newTicker
	lt.tick = func(context.Context) error {
		ticks <- struct{}{}
		return tickErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		lt.Run(ctx)
		close(done)
	}()
	select {
	case <-clock.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not start its ticker")
	}
	return clock, ticks, cancel, done, sink
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *logSink) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// Boundary: with a 60 s interval nothing runs at 59 s, one tick runs at
// 60 s and the second at 120 s.
func TestLockChainTicker_FiresOnIntervalBoundary(t *testing.T) {
	clock, ticks, cancel, done, _ := startFakeTicker(t, 60*time.Second, nil)
	defer cancel()
	if clock.period != 60*time.Second {
		t.Fatalf("ticker period = %s, want 60s", clock.period)
	}
	clock.Advance(59 * time.Second)
	waitTick(t, ticks, false)
	clock.Advance(time.Second)
	waitTick(t, ticks, true)
	clock.Advance(59 * time.Second)
	waitTick(t, ticks, false)
	clock.Advance(time.Second)
	waitTick(t, ticks, true)
	cancel()
	<-done
	if !clock.isStopped() {
		t.Fatal("ticker not stopped after Run returned")
	}
}

// A failing tick is logged with context and does not stop the loop.
func TestLockChainTicker_TickErrorLoggedLoopContinues(t *testing.T) {
	clock, ticks, cancel, done, sink := startFakeTicker(
		t, 60*time.Second, errors.New("probe: connection refused"))
	defer cancel()
	clock.Advance(60 * time.Second)
	waitTick(t, ticks, true)
	clock.Advance(60 * time.Second)
	waitTick(t, ticks, true)
	cancel()
	<-done
	if !sink.contains("lock-chain fast path") ||
		!sink.contains("connection refused") {
		t.Fatalf("tick error not logged with context: %v", sink.lines)
	}
}

func TestLockChainTicker_DisabledIntervalReturnsImmediately(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		lt := NewLockChainTicker(testEngine(), nil, nil, d, noopTestLog)
		lt.newTicker = func(time.Duration) (<-chan time.Time, func()) {
			t.Fatalf("interval %s must not start a ticker", d)
			return nil, nil
		}
		done := make(chan struct{})
		go func() {
			lt.Run(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("Run with interval %s did not return", d)
		}
	}
}

func noopTestLog(string, string, ...any) {}

func TestLockChainTicker_TickRequiresStore(t *testing.T) {
	probeCalls := 0
	lt := NewLockChainTicker(testEngine(), nil,
		func(context.Context) ([]analyzer.Finding, error) {
			probeCalls++
			return nil, nil
		}, time.Minute, noopTestLog)
	err := lt.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hydrate") {
		t.Fatalf("Tick without a pool: want hydrate error, got %v", err)
	}
	if probeCalls != 0 {
		t.Fatal("probe ran although incident state could not load")
	}
}

func TestLockChainTicker_NilProbeRejected(t *testing.T) {
	lt := NewLockChainTicker(testEngine(), nil, nil, time.Minute, noopTestLog)
	lt.hydrate = func(context.Context) error { return nil }
	if err := lt.Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "probe") {
		t.Fatalf("nil probe: want probe error, got %v", err)
	}
}
