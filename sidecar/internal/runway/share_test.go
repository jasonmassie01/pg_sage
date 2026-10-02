package runway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Fleet WAL-runway dedupe (Sage SRE follow-ups B): every fleet runtime on
// one cluster used to sum every database's size on every pass (quadratic
// in the fleet). The size is a cluster-level measurement: one runtime
// measures it per cluster per pass and the others reuse it. A failed
// measurement is never shared; an unknown cluster is never shared.

type sizeCounter struct {
	calls atomic.Int64
	value float64
	err   error
	gate  chan struct{} // when set, measurements wait for it
}

func (c *sizeCounter) measure(ctx context.Context) (ClusterSize, error) {
	c.calls.Add(1)
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ClusterSize{}, ctx.Err()
		}
	}
	if c.err != nil {
		return ClusterSize{}, c.err
	}
	return ClusterSize{DatabaseBytes: c.value}, nil
}

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestShare() (*SizeShare, *stepClock) {
	clock := &stepClock{now: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	s := NewSizeShare()
	s.now = clock.Now
	return s, clock
}

func TestSizeShare_OneMeasurementPerClusterPerPass(t *testing.T) {
	s, clock := newTestShare()
	c := &sizeCounter{value: 4e9}
	ctx := context.Background()
	measuredAt := clock.Now()
	for i := 0; i < 10; i++ {
		got, shared, err := s.Measure(ctx, "sys-1/start/sage", time.Minute, c.measure)
		if err != nil || got.DatabaseBytes != 4e9 {
			t.Fatalf("measure %d = %+v (%v)", i, got, err)
		}
		if shared != (i > 0) {
			t.Fatalf("measure %d shared = %v", i, shared)
		}
		// A shared reading keeps the time it was measured at.
		if !got.MeasuredAt.Equal(measuredAt) {
			t.Fatalf("measured at %v, want %v", got.MeasuredAt, measuredAt)
		}
		clock.advance(time.Second)
	}
	if n := c.calls.Load(); n != 1 {
		t.Fatalf("%d measurements for one cluster in one pass, want 1", n)
	}
}

// A reading is reused for less than a pass: exactly one pass old it is
// measured again.
func TestSizeShare_PassBoundary(t *testing.T) {
	s, clock := newTestShare()
	c := &sizeCounter{value: 1}
	ctx := context.Background()
	_, _, _ = s.Measure(ctx, "k", time.Minute, c.measure)
	clock.advance(time.Minute - time.Nanosecond)
	if _, shared, _ := s.Measure(ctx, "k", time.Minute, c.measure); !shared {
		t.Fatal("a reading younger than a pass was measured again")
	}
	clock.advance(time.Nanosecond)
	if _, shared, _ := s.Measure(ctx, "k", time.Minute, c.measure); shared {
		t.Fatal("a reading one pass old was reused")
	}
	if n := c.calls.Load(); n != 2 {
		t.Fatalf("%d measurements, want 2", n)
	}
}

func TestSizeShare_ClustersAreSeparate(t *testing.T) {
	s, _ := newTestShare()
	a, b := &sizeCounter{value: 1}, &sizeCounter{value: 2}
	ctx := context.Background()
	ga, _, _ := s.Measure(ctx, "sys-a", time.Minute, a.measure)
	gb, _, _ := s.Measure(ctx, "sys-b", time.Minute, b.measure)
	if ga.DatabaseBytes != 1 || gb.DatabaseBytes != 2 || a.calls.Load() != 1 ||
		b.calls.Load() != 1 {
		t.Fatalf("a=%+v b=%+v", ga, gb)
	}
}

// An unknown cluster (no key), no share at all or no pass length: each
// caller measures for itself, as before the dedupe.
func TestSizeShare_UnknownClusterIsNeverShared(t *testing.T) {
	s, _ := newTestShare()
	c := &sizeCounter{value: 1}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, shared, err := s.Measure(ctx, "", time.Minute, c.measure); shared || err != nil {
			t.Fatalf("an unknown cluster was shared (%v)", err)
		}
		if _, shared, _ := s.Measure(ctx, "k", 0, c.measure); shared {
			t.Fatal("a zero pass shared a reading")
		}
	}
	var none *SizeShare
	if _, shared, err := none.Measure(ctx, "k", time.Minute, c.measure); shared || err != nil {
		t.Fatalf("a nil share shared (%v)", err)
	}
	if n := c.calls.Load(); n != 7 {
		t.Fatalf("%d measurements, want 7 (none shared)", n)
	}
}

// A failure is returned to everyone waiting on it and never reused: the
// next caller measures again.
func TestSizeShare_FailureIsNotShared(t *testing.T) {
	s, _ := newTestShare()
	c := &sizeCounter{err: errors.New("statement timeout")}
	ctx := context.Background()
	if _, _, err := s.Measure(ctx, "k", time.Minute, c.measure); err == nil ||
		err.Error() != "statement timeout" {
		t.Fatalf("err = %v", err)
	}
	c.err, c.value = nil, 9
	got, shared, err := s.Measure(ctx, "k", time.Minute, c.measure)
	if err != nil || shared || got.DatabaseBytes != 9 || c.calls.Load() != 2 {
		t.Fatalf("after a failure: %+v shared %v (%v), %d calls", got, shared, err,
			c.calls.Load())
	}
}

// Runtimes ticking at once wait for the one measurement in flight (run
// with -race).
func TestSizeShare_ConcurrentRuntimesMeasureOnce(t *testing.T) {
	s, _ := newTestShare()
	c := &sizeCounter{value: 5e9, gate: make(chan struct{})}
	const runtimes = 32
	var wg sync.WaitGroup
	var sharedN atomic.Int64
	errs := make(chan error, runtimes)
	for i := 0; i < runtimes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, shared, err := s.Measure(context.Background(), "k", time.Minute, c.measure)
			if err != nil || got.DatabaseBytes != 5e9 {
				errs <- errors.Join(err, errors.New("wrong size"))
			}
			if shared {
				sharedN.Add(1)
			}
		}()
	}
	for c.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the others queue on the flight
	close(c.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := c.calls.Load(); n != 1 || sharedN.Load() != runtimes-1 {
		t.Fatalf("%d measurements, %d shared; want 1 and %d", n, sharedN.Load(), runtimes-1)
	}
}

// A waiter whose context ends stops waiting; the flight still completes
// for the others.
func TestSizeShare_WaiterCancellation(t *testing.T) {
	s, _ := newTestShare()
	c := &sizeCounter{value: 3, gate: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Measure(context.Background(), "k", time.Minute, c.measure)
		done <- err
	}()
	for c.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Measure(ctx, "k", time.Minute, c.measure); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("canceled waiter err = %v", err)
	}
	close(c.gate)
	if err := <-done; err != nil {
		t.Fatalf("the flight failed for its owner: %v", err)
	}
	if got, shared, _ := s.Measure(context.Background(), "k", time.Minute,
		c.measure); !shared || got.DatabaseBytes != 3 {
		t.Fatalf("after the flight: %+v shared %v", got, shared)
	}
}

// The cluster key: the system identifier, plus the postmaster start and
// the role, so a standby, a restored clone or a differently privileged
// role never reuses another's reading. Without a system identifier there
// is no key.
func TestClusterKey(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	w := probes.WALRunway{SystemID: "7001", StartedAt: start, RoleName: "sage"}
	k := ClusterKey(w)
	if k == "" {
		t.Fatal("a known cluster has no key")
	}
	for name, other := range map[string]probes.WALRunway{
		"other cluster": {SystemID: "7002", StartedAt: start, RoleName: "sage"},
		"restarted":     {SystemID: "7001", StartedAt: start.Add(time.Hour), RoleName: "sage"},
		"other role":    {SystemID: "7001", StartedAt: start, RoleName: "reporting"},
	} {
		if ClusterKey(other) == k {
			t.Errorf("%s shares the key %q", name, k)
		}
	}
	if ClusterKey(probes.WALRunway{StartedAt: start, RoleName: "sage"}) != "" {
		t.Fatal("an unknown system identifier has a key")
	}
	if ClusterKey(w) != k {
		t.Fatal("the key is not stable")
	}
}
