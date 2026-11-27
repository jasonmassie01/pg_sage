package leader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockStore wraps memStore; while blocked, Acquire waits for its context
// to end, like a renewal on a half-open connection or an exhausted pool.
type blockStore struct {
	*memStore
	mu      sync.Mutex
	blocked bool
	calls   int
	lastDL  time.Duration
	hasDL   bool
}

func (s *blockStore) setBlocked(b bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = b
}

func (s *blockStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *blockStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	s.mu.Lock()
	s.calls++
	dl, ok := ctx.Deadline()
	s.hasDL = ok
	if ok {
		s.lastDL = time.Until(dl)
	}
	blocked := s.blocked
	s.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return Lease{}, false, fmt.Errorf("acquire lease %s: %w", scope, ctx.Err())
	}
	return s.memStore.Acquire(ctx, scope, holder, ttl)
}

// logSink records every elector log line.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logSink) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// tickWithin runs Tick and fails the test, instead of hanging, when it
// does not return within limit.
func tickWithin(t *testing.T, e *Elector, ctx context.Context,
	limit time.Duration) (error, time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- e.Tick(ctx) }()
	select {
	case err := <-done:
		return err, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("Tick still blocked after %v: the renewal has no per-tick bound", limit)
		return nil, 0
	}
}

func TestElector_TickBoundsBlockedRenewal(t *testing.T) {
	st := &blockStore{memStore: newMemStore(time.Now), blocked: true}
	ttl := 300 * time.Millisecond
	e := NewElector(st, "fleet", "A", ttl)
	err, took := tickWithin(t, e, context.Background(), 2*time.Second)
	if took > ttl/3+50*time.Millisecond {
		t.Fatalf("Tick took %v, want at most ttl/3 + 50ms = %v", took,
			ttl/3+50*time.Millisecond)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "renew lease of fleet") {
		t.Fatalf("Tick error = %q, want it to name the scope", err)
	}
	if e.IsLeader() {
		t.Fatal("a renewal that never answered must not grant leadership")
	}
	if s := e.Status(); !strings.Contains(s.LastError, "deadline exceeded") {
		t.Fatalf("status last error = %q, want the timeout", s.LastError)
	}
}

func TestElector_RunKeepsTickingAndRecoversAfterStall(t *testing.T) {
	st := &blockStore{memStore: newMemStore(time.Now), blocked: true}
	e := NewElector(st, "fleet", "A", 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(450 * time.Millisecond)
	if e.IsLeader() {
		t.Fatal("leader while every renewal is blocked")
	}
	if n := st.callCount(); n < 3 {
		t.Fatalf("Run made %d renewal attempts in 450ms, want >= 3 (one per ttl/3)", n)
	}
	st.setBlocked(false)
	deadline := time.Now().Add(2 * time.Second)
	for !e.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.IsLeader() {
		t.Fatal("Run did not become leader again once the store answered")
	}
}

func TestElector_LapseIsCountedAndWarnedOnce(t *testing.T) {
	c := newClock()
	st := &blockStore{memStore: newMemStore(c.Now)}
	sink := &logSink{}
	ttl := 300 * time.Millisecond
	e := NewElector(st, "fleet", "A", ttl, WithClock(c.Now), WithLogger(sink.log))
	if err := e.Tick(context.Background()); err != nil || !e.IsLeader() {
		t.Fatalf("first tick err=%v leader=%v, want leader", err, e.IsLeader())
	}
	base := e.Status().Transitions
	st.setBlocked(true)
	// A blocked renewal before the local deadline keeps leadership and
	// logs nothing: one slow renewal is not a lapse.
	c.Advance(100 * time.Millisecond)
	if err, _ := tickWithin(t, e, context.Background(), 2*time.Second); err == nil {
		t.Fatal("blocked tick returned no error")
	}
	if !e.IsLeader() || e.Status().Transitions != base || sink.count("WARN") != 0 {
		t.Fatalf("early stall: leader=%v transitions=%d warns=%d, want leader, %d, 0",
			e.IsLeader(), e.Status().Transitions, sink.count("WARN"), base)
	}
	// Past the deadline (ttl - ttl/5 = 240ms after the grant): a lapse.
	c.Advance(150 * time.Millisecond)
	_, _ = tickWithin(t, e, context.Background(), 2*time.Second)
	if e.IsLeader() {
		t.Fatal("still leader after the local deadline passed")
	}
	if got := e.Status().Transitions; got != base+1 {
		t.Fatalf("transitions = %d, want %d after one lapse", got, base+1)
	}
	if n := sink.count("WARN leader: lease of fleet lapsed, renewal overdue by 10ms: "); n != 1 {
		t.Fatalf("lapse WARN lines = %d (log %q), want exactly 1", n, sink.lines)
	}
	// More failed renewals while a follower: no new transition or WARN.
	c.Advance(100 * time.Millisecond)
	_, _ = tickWithin(t, e, context.Background(), 2*time.Second)
	if e.Status().Transitions != base+1 || sink.count("WARN") != 1 {
		t.Fatalf("repeat stall: transitions=%d warns=%d, want %d and 1",
			e.Status().Transitions, sink.count("WARN"), base+1)
	}
	st.setBlocked(false)
	if err := e.Tick(context.Background()); err != nil || !e.IsLeader() {
		t.Fatalf("recovery tick err=%v leader=%v, want leader again", err, e.IsLeader())
	}
	if got := e.Status().Transitions; got != base+2 {
		t.Fatalf("transitions = %d after re-acquiring, want %d", got, base+2)
	}
}

func TestElector_FollowerAfterSilentLapseIsCounted(t *testing.T) {
	// Leadership lapsed by time alone (no failed tick in between), then the
	// next renewal finds another holder: still one leader -> follower move.
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	c.Advance(31 * time.Second)
	_ = b.Tick(context.Background())
	_ = a.Tick(context.Background())
	if a.IsLeader() || !b.IsLeader() {
		t.Fatalf("a=%v b=%v, want B leading", a.IsLeader(), b.IsLeader())
	}
	if got := a.Status().Transitions; got != 2 {
		t.Fatalf("A transitions = %d, want 2 (became leader, then follower)", got)
	}
}

func TestElector_PerTickTimeoutIsThirdOfTTL(t *testing.T) {
	for _, ttl := range []time.Duration{10 * time.Second, 600 * time.Second} {
		st := &blockStore{memStore: newMemStore(time.Now)}
		e := NewElector(st, "fleet", "A", ttl)
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("ttl %v: tick: %v", ttl, err)
		}
		if !st.hasDL {
			t.Fatalf("ttl %v: the store call had no deadline", ttl)
		}
		if st.lastDL > ttl/3 || st.lastDL < ttl/3-time.Second {
			t.Fatalf("ttl %v: store deadline in %v, want ttl/3 = %v", ttl, st.lastDL, ttl/3)
		}
	}
}

func TestElector_PerTickTimeoutKeepsShorterParentDeadline(t *testing.T) {
	st := &blockStore{memStore: newMemStore(time.Now)}
	e := NewElector(st, "fleet", "A", 600*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !st.hasDL || st.lastDL > time.Second {
		t.Fatalf("store deadline in %v (set=%v), want the parent's 1s", st.lastDL, st.hasDL)
	}
}

func TestElector_LastRenewTracksSuccessfulRenewals(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	e := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	if s := e.Status(); !s.LastRenew.IsZero() {
		t.Fatalf("last renew before any tick = %v, want zero", s.LastRenew)
	}
	c.Advance(4 * time.Second)
	if got := e.LastRenewAge(); got != 4*time.Second {
		t.Fatalf("age before any renewal = %v, want time since start (4s)", got)
	}
	_ = e.Tick(context.Background())
	renewed := c.Now()
	if s := e.Status(); !s.LastRenew.Equal(renewed) {
		t.Fatalf("last renew = %v, want %v", s.LastRenew, renewed)
	}
	st.failErr = errors.New("connection refused")
	c.Advance(7 * time.Second)
	_ = e.Tick(context.Background())
	if s := e.Status(); !s.LastRenew.Equal(renewed) {
		t.Fatalf("a failed renewal moved last renew to %v", s.LastRenew)
	}
	if got := e.LastRenewAge(); got != 7*time.Second {
		t.Fatalf("age = %v, want 7s since the last good renewal", got)
	}
}

func TestElector_FollowerRenewalCountsAsRenew(t *testing.T) {
	// A follower's successful round trip proves the store answers; its
	// renew age must not grow while another sidecar leads.
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	c.Advance(5 * time.Second)
	_ = b.Tick(context.Background())
	if b.IsLeader() || b.LastRenewAge() != 0 {
		t.Fatalf("follower leader=%v age=%v, want follower with age 0",
			b.IsLeader(), b.LastRenewAge())
	}
}

func TestNilElector_LastRenewAgeIsZero(t *testing.T) {
	var e *Elector
	if got := e.LastRenewAge(); got != 0 {
		t.Fatalf("nil elector age = %v, want 0", got)
	}
	if s := e.Status(); !s.LastRenew.IsZero() {
		t.Fatalf("nil elector last renew = %v, want zero", s.LastRenew)
	}
}

func TestElector_ConcurrentStatusDuringBlockedTicks(t *testing.T) {
	st := &blockStore{memStore: newMemStore(time.Now), blocked: true}
	e := NewElector(st, "fleet", "A", 150*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = e.Tick(context.Background()) }()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = e.Status(), e.LastRenewAge()
			}
		}()
	}
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked ticks never returned: the renewal has no per-tick bound")
	}
	if s := e.Status(); s.Leader || !strings.Contains(s.LastError, "deadline") {
		t.Fatalf("status after blocked ticks = %+v, want follower with a timeout", s)
	}
}

func TestElector_LateRenewalThatReacquiresCountsLapse(t *testing.T) {
	// The renewal answers, still as holder, but only after the local
	// deadline: leadership was off for a while, so the lapse is counted and
	// warned before the sidecar becomes leader again.
	c := newClock()
	st := newMemStore(c.Now)
	sink := &logSink{}
	e := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now),
		WithLogger(sink.log))
	_ = e.Tick(context.Background())
	base := e.Status().Transitions
	c.Advance(25 * time.Second) // deadline is 24s, the lease 30s: still A's row
	if err := e.Tick(context.Background()); err != nil || !e.IsLeader() {
		t.Fatalf("late renewal err=%v leader=%v, want leader again", err, e.IsLeader())
	}
	if got := e.Status().Transitions; got != base+2 {
		t.Fatalf("transitions = %d, want %d (lapse, then leader again)", got, base+2)
	}
	want := "WARN leader: lease of fleet lapsed, renewal overdue by 1s: " +
		"renewal answered after the local deadline"
	if n := sink.count(want); n != 1 || sink.count("WARN") != 1 {
		t.Fatalf("log %q, want exactly one %q", sink.lines, want)
	}
	if n := sink.count("INFO leader: A became leader"); n != 2 {
		t.Fatalf("became-leader lines = %d (log %q), want 2", n, sink.lines)
	}
}

func TestElector_SilentLapseToFollowerWarnsNotInfo(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	sink := &logSink{}
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now),
		WithLogger(sink.log))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	c.Advance(31 * time.Second)
	_ = b.Tick(context.Background())
	_ = a.Tick(context.Background())
	if n := sink.count("WARN leader: lease of fleet lapsed"); n != 1 {
		t.Fatalf("lapse WARN lines = %d (log %q), want 1", n, sink.lines)
	}
	if n := sink.count("INFO leader: A is a follower"); n != 0 {
		t.Fatalf("a lapse already counted was logged again as a move: %q", sink.lines)
	}
}
