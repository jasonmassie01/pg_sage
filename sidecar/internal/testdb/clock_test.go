package testdb

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ci-14: the ClockWatch arithmetic, driven by a fake sampler so the
// steps are exact. No state-transition tests: the helpers hold no state
// machine (a watch keeps only its start sample).

var clockBase = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// fakeClocks returns its samples in order: each is (host offset, database
// offset) from clockBase. A sample past the end, or one at errAt, fails.
type fakeClocks struct {
	mu      sync.Mutex
	samples [][2]time.Duration
	next    int
	errAt   int
	err     error
}

func (f *fakeClocks) sample(context.Context) (clockSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.next
	f.next++
	if f.err != nil && i == f.errAt {
		return clockSample{}, f.err
	}
	if i >= len(f.samples) {
		return clockSample{}, fmt.Errorf("fake clocks: no sample %d", i)
	}
	s := f.samples[i]
	return clockSample{host: clockBase.Add(s[0]), db: clockBase.Add(s[1])}, nil
}

func watchOn(t *testing.T, f *fakeClocks) *ClockWatch {
	t.Helper()
	w, err := startClockWatch(context.Background(), f.sample)
	if err != nil {
		t.Fatalf("start watch: %v", err)
	}
	return w
}

// Boundary: a steady clock leaves the bound as it is; a step either way
// widens it by exactly the step.
func TestWidenBound(t *testing.T) {
	const bound = 300 * time.Millisecond
	for _, tc := range []struct {
		step, want time.Duration
	}{
		{0, bound},
		{900 * time.Millisecond, 1200 * time.Millisecond},
		{-900 * time.Millisecond, 1200 * time.Millisecond},
		{time.Nanosecond, bound + time.Nanosecond},
		{-time.Nanosecond, bound + time.Nanosecond},
	} {
		if got := WidenBound(bound, tc.step); got != tc.want {
			t.Errorf("WidenBound(%v, %v) = %v, want %v", bound, tc.step, got, tc.want)
		}
	}
	if got := WidenBound(0, -2*time.Second); got != 2*time.Second {
		t.Errorf("WidenBound(0, -2s) = %v, want 2s", got)
	}
}

func TestClockWatchStepIsTheDatabaseMoveAgainstTheHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host, db time.Duration
		want     time.Duration
	}{
		{"steady", 2 * time.Second, 2 * time.Second, 0},
		{"database ahead", 2 * time.Second, 2900 * time.Millisecond, 900 * time.Millisecond},
		{"database behind", 2 * time.Second, 1100 * time.Millisecond, -900 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The start sample carries a fixed 2 s offset: only the move counts.
			f := &fakeClocks{samples: [][2]time.Duration{{0, 2 * time.Second},
				{tc.host, 2*time.Second + tc.db}}}
			step, err := watchOn(t, f).Step(context.Background())
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if step != tc.want {
				t.Fatalf("step = %v, want %v", step, tc.want)
			}
		})
	}
}

// Nil/empty: Step called twice returns the step since start both times,
// not the step since the previous call.
func TestClockWatchStepIsSinceStartEveryTime(t *testing.T) {
	f := &fakeClocks{samples: [][2]time.Duration{{0, 0},
		{time.Second, 1500 * time.Millisecond}, {2 * time.Second, 2500 * time.Millisecond}}}
	w := watchOn(t, f)
	for i := 0; i < 2; i++ {
		step, err := w.Step(context.Background())
		if err != nil {
			t.Fatalf("Step %d: %v", i+1, err)
		}
		if step != 500*time.Millisecond {
			t.Fatalf("Step %d = %v, want 500ms since start", i+1, step)
		}
	}
}

func TestStartClockWatchNilQuerier(t *testing.T) {
	if w, err := StartClockWatch(context.Background(), nil); err == nil || w != nil {
		t.Fatalf("nil querier: watch=%v err=%v, want an error and no watch", w, err)
	}
	var typedNil *nilQuerier
	w, err := StartClockWatch(context.Background(), typedNil)
	if err == nil || w != nil || !strings.Contains(err.Error(), "no querier") {
		t.Fatalf("typed nil querier: watch=%v err=%v, want 'no querier'", w, err)
	}
	if _, err := DBNow(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "clock_timestamp") {
		t.Fatalf("DBNow(nil) err = %v, want an error naming clock_timestamp", err)
	}
	var nilWatch *ClockWatch
	if step, err := nilWatch.Step(context.Background()); err == nil || step != 0 {
		t.Fatalf("nil watch Step = %v, %v, want an error", step, err)
	}
}

type nilQuerier struct{ Querier }

// Error propagation: a failed start sample is the caller's error.
func TestStartClockWatchPropagatesTheSampleError(t *testing.T) {
	boom := errors.New("connection refused")
	f := &fakeClocks{err: boom, errAt: 0}
	w, err := startClockWatch(context.Background(), f.sample)
	if !errors.Is(err, boom) || w != nil {
		t.Fatalf("start with a failing sample: watch=%v err=%v, want %v", w, err, boom)
	}
}

// Error propagation: a failed sample inside Step is an error, never a
// zero step.
func TestClockWatchStepPropagatesTheSampleError(t *testing.T) {
	boom := errors.New("connection refused")
	f := &fakeClocks{samples: [][2]time.Duration{{0, 0}}, err: boom, errAt: 1}
	step, err := watchOn(t, f).Step(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "step") {
		t.Fatalf("Step err = %v, want it to wrap %v and name the step", err, boom)
	}
	if step != 0 {
		t.Fatalf("Step returned %v with its error", step)
	}
}

// fatalRecorder is a testing.TB whose Fatal stops only its goroutine, so a
// test can see that Widen fails its caller.
type fatalRecorder struct {
	testing.TB
	mu     sync.Mutex
	fatals []string
	logs   []string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *fatalRecorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// runRecorded runs fn with a recorder in its own goroutine and returns
// the widened bound (zero when fn stopped) and the recorder.
func runRecorded(t *testing.T, fn func(testing.TB) time.Duration) (time.Duration,
	*fatalRecorder) {
	t.Helper()
	rec := &fatalRecorder{TB: t}
	var got time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = fn(rec)
	}()
	<-done
	return got, rec
}

func TestClockWatchWidenFailsTheCallerOnAFailedSample(t *testing.T) {
	boom := errors.New("server closed the connection")
	f := &fakeClocks{samples: [][2]time.Duration{{0, 0}}, err: boom, errAt: 1}
	w := watchOn(t, f)
	got, rec := runRecorded(t, func(tb testing.TB) time.Duration {
		return w.Widen(tb, 300*time.Millisecond)
	})
	if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], boom.Error()) {
		t.Fatalf("fatals = %q, want one naming %q", rec.fatals, boom)
	}
	if got != 0 {
		t.Fatalf("Widen returned %v after a failed sample", got)
	}
}

func TestClockWatchWidenAddsAndLogsTheMeasuredStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   time.Duration
		want time.Duration
	}{
		{"steady", 3 * time.Second, 300 * time.Millisecond},
		{"ahead", 3900 * time.Millisecond, 1200 * time.Millisecond},
		{"behind", 2100 * time.Millisecond, 1200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeClocks{samples: [][2]time.Duration{{0, 0}, {3 * time.Second, tc.db}}}
			w := watchOn(t, f)
			got, rec := runRecorded(t, func(tb testing.TB) time.Duration {
				return w.Widen(tb, 300*time.Millisecond)
			})
			if len(rec.fatals) != 0 || got != tc.want {
				t.Fatalf("Widen = %v (fatals %q), want %v", got, rec.fatals, tc.want)
			}
			if len(rec.logs) != 1 || !strings.Contains(rec.logs[0], tc.want.String()) {
				t.Fatalf("logs = %q, want one line naming the widened bound %v",
					rec.logs, tc.want)
			}
		})
	}
}

// Concurrency: two goroutines share one watch. The race detector run is
// OWNER ACTION on this host (-race needs cgo); here every step must still
// be the exact step since start.
func TestClockWatchSharedByTwoGoroutines(t *testing.T) {
	const per = 50
	samples := [][2]time.Duration{{0, 0}}
	for i := 1; i <= 2*per; i++ {
		d := time.Duration(i) * time.Second
		samples = append(samples, [2]time.Duration{d, d + 250*time.Millisecond})
	}
	w := watchOn(t, &fakeClocks{samples: samples})
	var wg sync.WaitGroup
	errs := make(chan error, 2*per)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				step, err := w.Step(context.Background())
				if err == nil && step != 250*time.Millisecond {
					err = fmt.Errorf("step %v, want 250ms", step)
				}
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
