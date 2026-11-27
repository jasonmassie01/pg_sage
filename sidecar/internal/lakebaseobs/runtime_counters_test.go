package lakebaseobs

import (
	"context"
	"sync"
	"testing"
	"time"
)

// scriptedReader answers each ReadWindow call from script in order (the
// last entry repeats). Each call sleeps delay first, so read time is real.
type scriptedReader struct {
	mu     sync.Mutex
	hash   string
	delay  time.Duration
	script []scriptStep
	calls  int
}

type scriptStep struct {
	err    error
	events int
	bytes  int
}

func (f *scriptedReader) ReadWindow(_ context.Context, s Stream, _ Scope, w Window,
) (Batch, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	step := f.script[min(f.calls, len(f.script)-1)]
	f.calls++
	if step.err != nil {
		return Batch{}, step.err
	}
	b := Batch{Bytes: step.bytes}
	for i := range step.events {
		b.Events = append(b.Events, Event{ID: string(s) + "-" + w.From.String() + "-" +
			string(rune('a'+i)), Stream: s, ScopeHash: f.hash, ComputeID: "compute-1",
			OccurredAt: w.From, ReceivedAt: w.From, Version: SchemaVersion})
	}
	return b, nil
}

func countersRuntime(t *testing.T, f *scriptedReader, queries int) *Runtime {
	t.Helper()
	rt, err := NewRuntime(f, &Store{}, testScope(), Options{WarehouseID: "test",
		Streams: []Stream{Plans}, QueriesPerPoll: queries})
	if err != nil {
		t.Fatal(err)
	}
	f.hash = rt.scope.Hash()
	return rt
}

func planStatus(t *testing.T, rt *Runtime) StreamStatus {
	t.Helper()
	for _, st := range rt.Status() {
		if st.Stream == Plans {
			return st
		}
	}
	t.Fatal("plans stream missing from Status")
	return StreamStatus{}
}

// Nil/empty: a runtime that has not polled reports zero counters and the
// time counting started.
func TestStreamCountersStartAtZeroWithAStartTime(t *testing.T) {
	before := time.Now().UTC()
	rt := countersRuntime(t, &scriptedReader{script: []scriptStep{{}}}, 3)
	st := planStatus(t, rt)
	if st.Statements != 0 || st.Rows != 0 || st.Bytes != 0 || st.ReadMillis != 0 {
		t.Fatalf("fresh counters = %+v; want zero", st)
	}
	if st.CountersSince.Before(before) || st.CountersSince.After(time.Now().UTC()) {
		t.Fatalf("counters_since = %v; want the runtime's start", st.CountersSince)
	}
}

// Boundary: one window halved twice is three statements, and the rows and
// bytes come only from the read that succeeded.
func TestStreamCountersCountEveryHalvingRetry(t *testing.T) {
	f := &scriptedReader{delay: 3 * time.Millisecond, script: []scriptStep{
		{err: ErrBudget}, {err: ErrBudget}, {events: 2, bytes: 100},
	}}
	rt := countersRuntime(t, f, 3)
	errs := rt.collect(context.Background(), newMemCursors(), scheduleEpoch)
	if len(errs) != 0 {
		t.Fatalf("collect errors = %v", errs)
	}
	st := planStatus(t, rt)
	if f.calls != 3 || st.Statements != 3 {
		t.Fatalf("reader calls=%d statements=%d; want 3 and 3", f.calls, st.Statements)
	}
	if st.Rows != 2 || st.Bytes != 100 {
		t.Fatalf("rows=%d bytes=%d; want 2 and 100", st.Rows, st.Bytes)
	}
	if st.ReadMillis < 9 {
		t.Fatalf("read_millis = %d; want at least three 3 ms reads", st.ReadMillis)
	}
}

// Boundary: a read that ends on ErrBudget still counts every attempt it
// spent, and the counters accumulate across polls.
func TestStreamCountersKeepAttemptsOfABudgetExit(t *testing.T) {
	f := &scriptedReader{script: []scriptStep{{err: ErrBudget}}}
	rt := countersRuntime(t, f, 4)
	mem := newMemCursors()
	errs := rt.collect(context.Background(), mem, scheduleEpoch)
	if len(errs) != 1 {
		t.Fatalf("collect errors = %v; want one budget error", errs)
	}
	st := planStatus(t, rt)
	if st.Statements != 4 || st.Rows != 0 || st.Bytes != 0 {
		t.Fatalf("after a budget exit = %+v; want 4 statements and no rows", st)
	}
	if st.LastError != ErrBudget.Error() {
		t.Fatalf("last_error = %q; want the budget error", st.LastError)
	}
	rt.collect(context.Background(), mem, scheduleEpoch.Add(time.Minute))
	if st = planStatus(t, rt); st.Statements != 8 || f.calls != 8 {
		t.Fatalf("after two polls statements=%d calls=%d; want 8", st.Statements, f.calls)
	}
}

// Error propagation: a failed (non-budget) read is one statement with no
// rows, and the stream stops reading for this poll.
func TestStreamCountersCountAFailedRead(t *testing.T) {
	f := &scriptedReader{script: []scriptStep{{err: ErrSchema}}}
	rt := countersRuntime(t, f, 6)
	rt.collect(context.Background(), newMemCursors(), scheduleEpoch)
	st := planStatus(t, rt)
	if st.Statements != 1 || st.Rows != 0 || st.LastError != ErrSchema.Error() {
		t.Fatalf("after a schema failure = %+v; want one statement and the error", st)
	}
}

// Concurrent access: Status readers while polls run see counters that never
// go backwards. Run with -race where cgo is available.
func TestStreamCountersStatusWhileCollecting(t *testing.T) {
	f := &scriptedReader{script: []scriptStep{{events: 1, bytes: 10}}}
	rt := countersRuntime(t, f, 3)
	mem := newMemCursors()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last int64
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, st := range rt.Status() {
					if st.Statements < last {
						t.Errorf("statements went back from %d to %d", last, st.Statements)
					}
					last = st.Statements
				}
			}
		}()
	}
	now := scheduleEpoch
	for range 30 {
		rt.collect(context.Background(), mem, now)
		now = now.Add(time.Minute)
	}
	close(done)
	wg.Wait()
	st := planStatus(t, rt)
	if st.Statements != int64(f.calls) || st.Rows < 30 || st.Bytes != 10*st.Rows {
		t.Fatalf("final counters = %+v with %d reader calls", st, f.calls)
	}
}
