package cloudtel

import (
	"context"
	"testing"
	"time"
)

// clouds-08: a shared runtime cannot be rebuilt by one member's reload,
// so the reload asks it to poll at once instead.
func TestRuntimeSharePollNowWakesRun(t *testing.T) {
	src := &stubSource{provider: "rds", samples: []Sample{healthySample(time.Now())}}
	r, err := NewRuntime(src, RuntimeOptions{Database: "pollnow", Limits: DefaultLimits(),
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, nil, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitCalls(t, src, 1)
	r.PollNow()
	r.PollNow() // coalesces: one pending wake at most
	waitCalls(t, src, 2)
	time.Sleep(50 * time.Millisecond)
	if n := src.callCount(); n > 3 {
		t.Fatalf("PollNow polled %d times, want at most one per pending wake", n)
	}
}

// Nil/zero: an Unavailable runtime has nothing to poll and PollNow is a
// no-op that never blocks.
func TestRuntimeSharePollNowOnUnavailable(t *testing.T) {
	r := Unavailable("pollnow-u", "rds", "no credentials")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PollNow()
		r.PollNow()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PollNow blocked on an Unavailable runtime")
	}
}

func waitCalls(t *testing.T, src *stubSource, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for src.callCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("source polled %d times, want %d", src.callCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *stubSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
