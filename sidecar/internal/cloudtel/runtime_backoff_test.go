package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTimer is one timer Run armed through RuntimeOptions.After.
type fakeTimer struct {
	d  time.Duration
	ch chan time.Time
}

// fakeTimers hands each armed timer to the test, which advances the
// clock and fires it: polls happen only when the test says so.
type fakeTimers struct {
	reqs chan fakeTimer
	stop chan struct{}
}

func newFakeTimers(t *testing.T) *fakeTimers {
	f := &fakeTimers{reqs: make(chan fakeTimer), stop: make(chan struct{})}
	t.Cleanup(func() { close(f.stop) })
	return f
}

func (f *fakeTimers) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	select {
	case f.reqs <- fakeTimer{d: d, ch: ch}:
	case <-f.stop:
	}
	return ch
}

// runHarness runs Runtime.Run on a fake clock and counts its callbacks.
type runHarness struct {
	t          *testing.T
	r          *Runtime
	src        *stubSource
	c          *clock
	timers     *fakeTimers
	reports    atomic.Int32
	recoveries atomic.Int32
	mu         sync.Mutex
	reported   []error
	cancel     context.CancelFunc
	done       chan struct{}
}

func startRun(t *testing.T, src *stubSource, interval time.Duration) *runHarness {
	t.Helper()
	h := &runHarness{t: t, src: src, c: &clock{t: guardNow}, timers: newFakeTimers(t),
		done: make(chan struct{})}
	r, err := NewRuntime(src, RuntimeOptions{Database: "orders", Interval: interval,
		Limits: DefaultLimits(), Now: h.c.now, After: h.timers.after})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	h.r = r
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		r.Run(ctx, func(err error) {
			h.mu.Lock()
			h.reported = append(h.reported, err)
			h.mu.Unlock()
			h.reports.Add(1)
		}, func() { h.recoveries.Add(1) })
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *runHarness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not stop on cancel")
	}
}

// next waits until Run has polled and armed its next timer.
func (h *runHarness) next() fakeTimer {
	h.t.Helper()
	select {
	case tm := <-h.timers.reqs:
		return tm
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not arm its next timer")
		return fakeTimer{}
	}
}

// fire advances the clock by the timer's delay and fires it.
func (h *runHarness) fire(tm fakeTimer) {
	h.c.advance(tm.d)
	tm.ch <- h.c.now()
}

func (h *runHarness) calls() int {
	h.src.mu.Lock()
	defer h.src.mu.Unlock()
	return h.src.calls
}

func noCreds() error {
	return fmt.Errorf("%w: AWS default chain found nothing", ErrNoCredentials)
}

// State transition: permanent failures double the delay, report once;
// a success reports one recovery and returns to the base interval.
func TestRuntimeRunBacksOffPermanentErrors(t *testing.T) {
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: []error{noCreds(), noCreds(), noCreds(), nil}}
	h := startRun(t, src, time.Minute)
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		tm := h.next()
		if tm.d != want {
			t.Fatalf("delay after failure %d = %v, want %v", i+1, tm.d, want)
		}
		st := h.r.Status()
		if st.ConsecutiveFailures != i+1 || st.NextPollAt == nil ||
			!st.NextPollAt.Equal(h.c.now().Add(want)) {
			t.Fatalf("status after failure %d = failures %d next %v (now %v)", i+1,
				st.ConsecutiveFailures, st.NextPollAt, h.c.now())
		}
		if n := h.reports.Load(); n != 1 {
			t.Fatalf("reports after failure %d = %d, want exactly 1", i+1, n)
		}
		h.fire(tm)
	}
	tm := h.next()
	if tm.d != time.Minute {
		t.Fatalf("delay after recovery = %v, want the base interval", tm.d)
	}
	if h.reports.Load() != 1 || h.recoveries.Load() != 1 {
		t.Fatalf("reports %d recoveries %d, want 1 and 1", h.reports.Load(),
			h.recoveries.Load())
	}
	st := h.r.Status()
	if !st.Available || st.ConsecutiveFailures != 0 || h.calls() != 4 {
		t.Fatalf("after recovery: available %v failures %d calls %d", st.Available,
			st.ConsecutiveFailures, h.calls())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !errors.Is(h.reported[0], ErrNoCredentials) {
		t.Fatalf("reported %v, want the no-credentials error", h.reported[0])
	}
}

// Boundary: the ceilings and the reset, computed directly.
func TestRuntimeNextDelayCeilings(t *testing.T) {
	r := &Runtime{interval: time.Minute}
	auth := fmt.Errorf("%w: AccessDenied", ErrAuth)
	cases := []struct {
		err      error
		failures int
		want     time.Duration
	}{
		{nil, 0, time.Minute},
		{nil, 7, time.Minute}, // a success always resets
		{noCreds(), 1, time.Minute},
		{noCreds(), 5, 16 * time.Minute},
		{noCreds(), 6, 30 * time.Minute},
		{auth, 10, 30 * time.Minute},
		{ErrIdentity, 10, 30 * time.Minute},
		{noCreds(), 1000, 30 * time.Minute}, // no overflow
	}
	for _, tc := range cases {
		if got := r.nextDelay(tc.err, tc.failures); got != tc.want {
			t.Errorf("nextDelay(%v, %d) = %v, want %v", tc.err, tc.failures, got, tc.want)
		}
	}
	lo, hi := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < 500; i++ {
		d := r.nextDelay(fmt.Errorf("%w: Rate exceeded", ErrThrottled), 10)
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo < 8*time.Minute || hi > 12*time.Minute || lo == hi {
		t.Fatalf("transient delay after 10 failures in [%v, %v], want jittered in [8m, 12m]",
			lo, hi)
	}
	for _, err := range []error{ErrProvider, ErrMalformed, ErrClockSkew,
		errors.New("dial tcp: connection refused"), context.DeadlineExceeded} {
		if d := r.nextDelay(err, 1); d < 48*time.Second || d > 72*time.Second {
			t.Errorf("first transient delay for %v = %v, want 1m +/-20%%", err, d)
		}
	}
}

// Boundary: an interval above the permanent ceiling is never shortened.
func TestRuntimeNextDelayNeverBelowInterval(t *testing.T) {
	r := &Runtime{interval: time.Hour}
	if d := r.nextDelay(noCreds(), 1); d != time.Hour {
		t.Fatalf("permanent delay with a 1h interval = %v, want 1h", d)
	}
	if d := r.nextDelay(noCreds(), 20); d != time.Hour {
		t.Fatalf("permanent delay with a 1h interval after 20 failures = %v, want 1h", d)
	}
}

// Boundary through Run: ten permanent failures reach the 30-minute
// ceiling and one success resets it.
func TestRuntimeRunCeilingThenReset(t *testing.T) {
	errs := make([]error, 0, 11)
	for i := 0; i < 10; i++ {
		errs = append(errs, noCreds())
	}
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: append(errs, nil)}
	h := startRun(t, src, time.Minute)
	var tm fakeTimer
	for i := 0; i < 10; i++ {
		tm = h.next()
		if i < 9 {
			h.fire(tm)
		}
	}
	if tm.d != 30*time.Minute || h.r.Status().ConsecutiveFailures != 10 {
		t.Fatalf("after 10 permanent failures delay %v failures %d", tm.d,
			h.r.Status().ConsecutiveFailures)
	}
	h.fire(tm)
	if tm = h.next(); tm.d != time.Minute {
		t.Fatalf("delay after the reset = %v, want 1m", tm.d)
	}
}

// Error propagation: throttling backs off with jitter, the status names
// it, and the runtime is not marked permanently unavailable.
func TestRuntimeRunThrottledBacksOff(t *testing.T) {
	throttled := fmt.Errorf("%w: CloudWatch Rate exceeded", ErrThrottled)
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: []error{throttled, throttled, nil}}
	h := startRun(t, src, time.Minute)
	tm := h.next()
	if tm.d < 48*time.Second || tm.d > 72*time.Second {
		t.Fatalf("first throttled delay = %v", tm.d)
	}
	st := h.r.Status()
	if st.Available || !strings.Contains(st.Reason, "throttled") ||
		h.r.reason != "" {
		t.Fatalf("throttled status = %+v (permanent reason %q)", st, h.r.reason)
	}
	h.fire(tm)
	if tm = h.next(); tm.d < 96*time.Second || tm.d > 144*time.Second {
		t.Fatalf("second throttled delay = %v", tm.d)
	}
	h.fire(tm)
	h.next()
	if st := h.r.Status(); !st.Available || st.Reason != "" ||
		h.reports.Load() != 1 || h.recoveries.Load() != 1 {
		t.Fatalf("after throttling clears: %+v reports %d recoveries %d", st,
			h.reports.Load(), h.recoveries.Load())
	}
}

// State transition: a changed class or reason is reported again; a
// repeat of the same error is not.
func TestRuntimeRunReportsChangedErrors(t *testing.T) {
	auth := fmt.Errorf("%w: AccessDenied on GetMetricData", ErrAuth)
	other := fmt.Errorf("%w: AccessDenied on DescribeDBInstances", ErrAuth)
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: []error{noCreds(), noCreds(), auth, auth, other}}
	h := startRun(t, src, time.Minute)
	for i := 0; i < 5; i++ {
		h.fire(h.next())
	}
	h.next()
	if n := h.reports.Load(); n != 3 {
		t.Fatalf("reports = %d, want 3 (no creds, auth, changed auth reason)", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !errors.Is(h.reported[1], ErrAuth) || h.reported[2].Error() != other.Error() {
		t.Fatalf("reported = %v", h.reported)
	}
}

// Nil/empty: an Unavailable runtime's Run has no source to call; it
// never polls, never reports and stops on cancel.
func TestUnavailableRuntimeRunNeverPolls(t *testing.T) {
	r := Unavailable("orders", "rds", "no AWS credentials in the default chain")
	var reports, recoveries atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, func(error) { reports.Add(1) }, func() { recoveries.Add(1) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Unavailable Run did not stop on cancel")
	}
	st := r.Status()
	if reports.Load() != 0 || recoveries.Load() != 0 || st.LastPollAt != nil ||
		st.NextPollAt != nil || st.ConsecutiveFailures != 0 {
		t.Fatalf("reports %d recoveries %d status %+v", reports.Load(), recoveries.Load(), st)
	}
}

// Concurrent access: Status readers during a backoff see one stable
// next poll time and failure count.
func TestRuntimeStatusStableDuringBackoff(t *testing.T) {
	src := &stubSource{provider: "rds", errs: []error{noCreds()}}
	h := startRun(t, src, time.Minute)
	h.fire(h.next())
	h.fire(h.next())
	tm := h.next() // three failures; Run now waits 4 minutes
	want := h.c.now().Add(tm.d)
	var bad atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				st := h.r.Status()
				if st.ConsecutiveFailures != 3 || st.NextPollAt == nil ||
					!st.NextPollAt.Equal(want) {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || tm.d != 4*time.Minute {
		t.Fatalf("%d unstable status reads (delay %v)", bad.Load(), tm.d)
	}
}

// Happy path: steady success polls once per interval, reports nothing.
func TestRuntimeRunSteadySuccess(t *testing.T) {
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)}}
	h := startRun(t, src, time.Minute)
	for i := 1; i <= 5; i++ {
		tm := h.next()
		if tm.d != time.Minute || h.calls() != i {
			t.Fatalf("poll %d: delay %v calls %d", i, tm.d, h.calls())
		}
		if st := h.r.Status(); !st.Available || st.ConsecutiveFailures != 0 {
			t.Fatalf("poll %d status = %+v", i, st)
		}
		h.fire(tm)
	}
	h.next()
	if h.reports.Load() != 0 || h.recoveries.Load() != 0 {
		t.Fatalf("steady success reported %d, recovered %d", h.reports.Load(),
			h.recoveries.Load())
	}
}

// CHECK-02: ten simulated hours without credentials cost at most 25
// provider calls and one report, and credentials added afterwards are
// still picked up.
func TestRuntimeRunTenHoursWithoutCredentials(t *testing.T) {
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(guardNow)},
		errs: []error{noCreds()}}
	h := startRun(t, src, time.Minute)
	var elapsed time.Duration
	tm := h.next()
	for elapsed+tm.d <= 10*time.Hour {
		elapsed += tm.d
		h.fire(tm)
		tm = h.next()
	}
	calls := h.calls()
	if calls > 25 || calls < 20 || h.reports.Load() != 1 {
		t.Fatalf("10h without credentials: %d calls, %d reports", calls, h.reports.Load())
	}
	src.mu.Lock()
	src.errs = []error{nil}
	src.mu.Unlock()
	h.fire(tm)
	if tm = h.next(); tm.d != time.Minute || !h.r.Status().Available ||
		h.recoveries.Load() != 1 {
		t.Fatalf("credentials added later: delay %v status %+v", tm.d, h.r.Status())
	}
}
