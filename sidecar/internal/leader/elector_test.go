package leader

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory lease table with a controllable clock: the same
// conditional-acquire semantics as the Postgres store.
type memStore struct {
	mu      sync.Mutex
	now     func() time.Time
	lease   map[string]Lease
	failErr error
	calls   int
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{now: now, lease: map[string]Lease{}}
}

func (m *memStore) Acquire(_ context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.failErr != nil {
		return Lease{}, false, m.failErr
	}
	cur, ok := m.lease[scope]
	now := m.now()
	if ok && cur.Holder != holder && now.Before(cur.ExpiresAt) {
		return cur, false, nil
	}
	epoch := cur.Epoch
	if !ok || cur.Holder != holder {
		epoch++
	}
	l := Lease{Holder: holder, Epoch: epoch, ExpiresAt: now.Add(ttl)}
	m.lease[scope] = l
	return l, true, nil
}

func (m *memStore) Release(_ context.Context, scope, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.lease[scope]; ok && cur.Holder == holder {
		cur.ExpiresAt = m.now()
		m.lease[scope] = cur
	}
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestElector_FirstTickAcquires(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	e := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	if e.IsLeader() {
		t.Fatal("leader before any tick")
	}
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !e.IsLeader() {
		t.Fatal("not leader after acquiring a free lease")
	}
	holder, epoch, ok := e.Fence()
	if !ok || holder != "A" || epoch != 1 {
		t.Fatalf("fence = %q,%d,%v want A,1,true", holder, epoch, ok)
	}
}

func TestElector_OnlyOneLeaderAtATime(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	_ = b.Tick(context.Background())
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("a=%v b=%v, want only A", a.IsLeader(), b.IsLeader())
	}
	if s := b.Status(); s.Holder != "A" || s.Leader {
		t.Fatalf("follower status = %+v, want holder A", s)
	}
	// A keeps renewing: B never takes over.
	for i := 0; i < 5; i++ {
		c.Advance(10 * time.Second)
		_ = a.Tick(context.Background())
		_ = b.Tick(context.Background())
		if !a.IsLeader() || b.IsLeader() {
			t.Fatalf("round %d: a=%v b=%v", i, a.IsLeader(), b.IsLeader())
		}
	}
}

func TestElector_FailoverAfterExpiryWithNewEpoch(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	// A stops renewing (crashed or partitioned).
	c.Advance(29 * time.Second)
	_ = b.Tick(context.Background())
	if b.IsLeader() {
		t.Fatal("B took over before the lease expired")
	}
	c.Advance(2 * time.Second)
	_ = b.Tick(context.Background())
	if !b.IsLeader() {
		t.Fatal("B did not take over an expired lease")
	}
	if _, epoch, _ := b.Fence(); epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", epoch)
	}
	if a.IsLeader() {
		t.Fatal("split brain: A still believes it leads after its lease expired")
	}
}

func TestElector_LocalDeadlineEndsLeadershipBeforeTheLease(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	ttl := 30 * time.Second
	a := NewElector(st, "fleet", "A", ttl, WithClock(c.Now))
	_ = a.Tick(context.Background())
	c.Advance(ttl - ttl/SafetyDivisor - time.Millisecond)
	if !a.IsLeader() {
		t.Fatal("leadership ended before the local deadline")
	}
	c.Advance(2 * time.Millisecond)
	if a.IsLeader() {
		t.Fatal("leadership must end at the local deadline (TTL minus margin), " +
			"before any successor can acquire")
	}
}

func TestElector_DeadlineCountsFromBeforeTheAcquireCall(t *testing.T) {
	c := newClock()
	slow := &slowStore{memStore: newMemStore(c.Now), clk: c, delay: 10 * time.Second}
	ttl := 30 * time.Second
	a := NewElector(slow, "fleet", "A", ttl, WithClock(c.Now))
	_ = a.Tick(context.Background())
	// The store call took 10 s: the lease was granted at its start, so the
	// local deadline is start + ttl - margin, i.e. 10 s closer.
	c.Advance(ttl - ttl/SafetyDivisor - 10*time.Second + time.Millisecond)
	if a.IsLeader() {
		t.Fatal("call latency must be charged against the lease")
	}
}

type slowStore struct {
	*memStore
	clk   *clock
	delay time.Duration
}

func (s *slowStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	l, ok, err := s.memStore.Acquire(ctx, scope, holder, ttl)
	s.clk.Advance(s.delay)
	return l, ok, err
}

func TestElector_StoreErrorDropsLeadership(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	st.failErr = errors.New("connection refused")
	c.Advance(5 * time.Second)
	err := a.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "lease") {
		t.Fatalf("tick error = %v, want a lease error naming the cause", err)
	}
	if !a.IsLeader() {
		t.Fatal("one failed renewal must not end leadership before the deadline")
	}
	c.Advance(30 * time.Second)
	if a.IsLeader() {
		t.Fatal("leadership must lapse when renewals keep failing")
	}
	if s := a.Status(); !strings.Contains(s.LastError, "connection refused") {
		t.Fatalf("status = %+v, want the last error", s)
	}
}

func TestElector_ReleaseHandsOverImmediately(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if a.IsLeader() {
		t.Fatal("still leader after release")
	}
	_ = b.Tick(context.Background())
	if !b.IsLeader() {
		t.Fatal("B could not acquire a released lease")
	}
}

func TestElector_ScopesAreIndependent(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet-1", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet-2", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	_ = b.Tick(context.Background())
	if !a.IsLeader() || !b.IsLeader() {
		t.Fatal("leaders of different scopes must not exclude each other")
	}
}

func TestElector_RunTicksAndReleasesOnCancel(t *testing.T) {
	st := newMemStore(time.Now)
	e := NewElector(st, "fleet", "A", 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !e.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !e.IsLeader() {
		t.Fatal("Run never acquired")
	}
	time.Sleep(400 * time.Millisecond)
	if !e.IsLeader() {
		t.Fatal("Run did not renew before the lease ran out")
	}
	cancel()
	<-done
	if e.IsLeader() {
		t.Fatal("still leader after Run returned")
	}
	st.mu.Lock()
	expired := !time.Now().Before(st.lease["fleet"].ExpiresAt)
	st.mu.Unlock()
	if !expired {
		t.Fatal("Run must release the lease on shutdown")
	}
}

func TestElector_ConcurrentReadersAndTicks(t *testing.T) {
	st := newMemStore(time.Now)
	e := NewElector(st, "fleet", "A", time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = e.Tick(context.Background()) }()
		go func() { defer wg.Done(); _ = e.IsLeader(); _ = e.Status() }()
	}
	wg.Wait()
	if !e.IsLeader() {
		t.Fatal("not leader after concurrent ticks")
	}
}

func TestNewElector_InvalidArguments(t *testing.T) {
	st := newMemStore(time.Now)
	e := NewElector(st, "fleet", "A", 0)
	if e.TTL() != DefaultTTL {
		t.Fatalf("zero ttl = %v, want default %v", e.TTL(), DefaultTTL)
	}
	if err := NewElector(nil, "fleet", "A", time.Second).Tick(context.Background()); err == nil {
		t.Fatal("nil store must fail")
	}
	if err := NewElector(st, "", "A", time.Second).Tick(context.Background()); err == nil {
		t.Fatal("empty scope must fail")
	}
	if err := NewElector(st, "fleet", "", time.Second).Tick(context.Background()); err == nil {
		t.Fatal("empty holder must fail")
	}
}

func TestHolderID_UniquePerCall(t *testing.T) {
	a, b := HolderID(), HolderID()
	if a == "" || a == b {
		t.Fatalf("holder ids %q %q must be non-empty and unique", a, b)
	}
}

func TestNilElectorIsAlwaysLeader(t *testing.T) {
	var e *Elector
	if !e.IsLeader() {
		t.Fatal("a nil elector (election disabled) must allow every job")
	}
	if _, _, ok := e.Fence(); ok {
		t.Fatal("a nil elector has no fence")
	}
	if s := e.Status(); s.Enabled {
		t.Fatalf("nil status = %+v", s)
	}
}
