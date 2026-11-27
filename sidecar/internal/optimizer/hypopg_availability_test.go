package optimizer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// HypoPG availability (indexes-06): a found extension is cached, an absent
// one is re-probed after hypopgRecheck (an operator may install it without
// restarting pg_sage), and a failed probe is never cached.

// scriptedProbe answers availability probes in order and counts them.
type scriptedProbe struct {
	mu      sync.Mutex
	answers []probeAnswer
	calls   atomic.Int32
}

type probeAnswer struct {
	installed bool
	err       error
}

func (p *scriptedProbe) probe(context.Context) (bool, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.answers) == 0 {
		return false, errors.New("probe script exhausted")
	}
	a := p.answers[0]
	if len(p.answers) > 1 {
		p.answers = p.answers[1:]
	}
	return a.installed, a.err
}

// testClock is an injectable, advanceable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func scriptedHypoPG(answers ...probeAnswer) (*HypoPG, *scriptedProbe, *testClock) {
	p := &scriptedProbe{answers: answers}
	clock := &testClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	h := NewHypoPG(nil, noopLog2)
	h.probe, h.now = p.probe, clock.Now
	return h, p, clock
}

func TestHypoPGAvailability_AbsentIsReprobedAfterRecheck(t *testing.T) {
	h, p, clock := scriptedHypoPG(probeAnswer{installed: false}, probeAnswer{installed: true})
	if h.IsAvailable(t.Context()) {
		t.Fatal("first probe found HypoPG absent but IsAvailable reported true")
	}
	clock.advance(9 * time.Minute)
	if h.IsAvailable(t.Context()) || p.calls.Load() != 1 {
		t.Fatalf("within the recheck window: probes = %d, want 1 (cached false)",
			p.calls.Load())
	}
	clock.advance(time.Minute) // exactly hypopgRecheck after the probe
	if !h.IsAvailable(t.Context()) || p.calls.Load() != 2 {
		t.Fatalf("at the recheck boundary: probes = %d, want 2 and HypoPG available",
			p.calls.Load())
	}
}

func TestHypoPGAvailability_TrueIsCachedForever(t *testing.T) {
	h, p, clock := scriptedHypoPG(probeAnswer{installed: true}, probeAnswer{installed: false})
	if !h.IsAvailable(t.Context()) {
		t.Fatal("installed HypoPG reported unavailable")
	}
	clock.advance(24 * time.Hour)
	if !h.IsAvailable(t.Context()) || p.calls.Load() != 1 {
		t.Fatalf("a found extension must stay cached: probes = %d", p.calls.Load())
	}
}

func TestHypoPGAvailability_ErrorsAreNotCached(t *testing.T) {
	logs := &logRecorder{}
	h, p, _ := scriptedHypoPG(probeAnswer{err: errors.New("canceling statement due to " +
		"statement timeout")}, probeAnswer{installed: true})
	h.logFn = logs.log
	if h.IsAvailable(t.Context()) {
		t.Fatal("a failed probe reported HypoPG available")
	}
	// No clock advance: an error must not start the recheck window.
	if !h.IsAvailable(t.Context()) || p.calls.Load() != 2 {
		t.Fatalf("a failed probe was cached: probes = %d, want 2", p.calls.Load())
	}
	if got := logs.matching("HypoPG availability", "statement timeout"); len(got) != 1 {
		t.Fatalf("the failed probe was not logged with its cause: %q", logs.lines)
	}
}

func TestHypoPGAvailability_ErrorAfterExpiredFalseKeepsReprobing(t *testing.T) {
	h, p, clock := scriptedHypoPG(probeAnswer{installed: false},
		probeAnswer{err: errors.New("connection refused")}, probeAnswer{installed: true})
	_ = h.IsAvailable(t.Context())
	clock.advance(hypopgRecheck)
	if h.IsAvailable(t.Context()) {
		t.Fatal("a failed re-probe reported HypoPG available")
	}
	if !h.IsAvailable(t.Context()) || p.calls.Load() != 3 {
		t.Fatalf("after a failed re-probe the next check must probe again: probes = %d",
			p.calls.Load())
	}
}

func TestHypoPGAvailability_NilPoolNeverProbes(t *testing.T) {
	h := NewHypoPG(nil, noopLog2)
	for range 2 {
		if h.IsAvailable(t.Context()) {
			t.Fatal("nil pool reported HypoPG available")
		}
	}
	if h.available != nil {
		t.Fatal("a nil pool cached a verdict it never probed")
	}
}

func TestHypoPGAvailability_ParallelCallersProbeOnce(t *testing.T) {
	h, p, _ := scriptedHypoPG(probeAnswer{installed: true})
	var wg sync.WaitGroup
	var unavailable atomic.Int32
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !h.IsAvailable(context.Background()) {
				unavailable.Add(1)
			}
		}()
	}
	wg.Wait()
	if unavailable.Load() != 0 || p.calls.Load() != 1 {
		t.Fatalf("parallel callers: %d saw unavailable, %d probes (want 0, 1)",
			unavailable.Load(), p.calls.Load())
	}
}
