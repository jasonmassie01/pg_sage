package leader

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// No integration category here: these use memStore; the Postgres store's
// identity and the pinned-database cases are in postgres_control_db_test.go.

// countLines counts sink lines at level that contain substr.
func countLines(s *logSink, level, substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.HasPrefix(l, level+" ") && strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func mustTick(t *testing.T, e *Elector) {
	t.Helper()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("tick %s: %v", e.holder, err)
	}
}

func refusingPair(t *testing.T) (*clock, *Elector, *Elector, *logSink) {
	t.Helper()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	st := newMemStore(clk.Now)
	sink := &logSink{}
	a := NewElector(st, "fleet:x", "A", 30*time.Second, WithClock(clk.Now))
	b := NewElector(st, "fleet:x", "B", 30*time.Second, WithClock(clk.Now),
		WithLogger(sink.log), WithForeignHolderRefusal("pin the control database"))
	return clk, a, b, sink
}

func TestElector_ForeignHolderRenewingRefusesLeadershipForTheRun(t *testing.T) {
	ctx := context.Background()
	clk, a, b, sink := refusingPair(t)
	mustTick(t, a)
	mustTick(t, b) // first sight of A: could be a dead predecessor
	if st := b.Status(); st.LeadershipRefused || st.Holder != "A" {
		t.Fatalf("after one sight = %+v, want follower of A, not refused", st)
	}
	clk.Advance(10 * time.Second)
	mustTick(t, a) // A renews: it is alive
	mustTick(t, b)
	st := b.Status()
	if !st.LeadershipRefused || st.Leader {
		t.Fatalf("status = %+v, want refused follower", st)
	}
	if !strings.Contains(st.RefusedReason, "sidecar A also holds the lease of fleet:x") ||
		!strings.Contains(st.RefusedReason, "pin the control database") {
		t.Fatalf("refused reason = %q, want the holder and the configured reason",
			st.RefusedReason)
	}
	if n := countLines(sink, "ERROR", "B refuses leadership of fleet:x"); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1", n)
	}
	// A goes away; B never takes the lease for the rest of the run.
	if err := a.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	clk.Advance(time.Minute)
	for i := 0; i < 3; i++ {
		mustTick(t, b)
	}
	if b.IsLeader() || b.Status().Holder != "A" {
		t.Fatalf("refused elector led or acquired: %+v", b.Status())
	}
	if _, ok := b.Fence(); ok {
		t.Fatal("refused elector returned a fence")
	}
	if n := countLines(sink, "ERROR", "refuses leadership"); n != 1 {
		t.Fatalf("ERROR lines after more ticks = %d, want still 1", n)
	}
}

func TestElector_DeadPredecessorLeaseIsTakenOverNotRefused(t *testing.T) {
	clk, a, b, sink := refusingPair(t)
	mustTick(t, a) // A crashes after this tick and never renews
	for i := 0; i < 3; i++ {
		mustTick(t, b)
		clk.Advance(10 * time.Second)
	}
	mustTick(t, b) // lease expired at +30s: B takes it
	st := b.Status()
	if st.LeadershipRefused || !st.Leader || st.Epoch != 2 {
		t.Fatalf("status = %+v, want B leading at epoch 2", st)
	}
	if n := countLines(sink, "ERROR", "refuses"); n != 0 {
		t.Fatalf("ERROR lines = %d, want 0", n)
	}
}

func TestElector_NewForeignHolderCountsAsLive(t *testing.T) {
	clk, a, b, _ := refusingPair(t)
	st := a.store.(*memStore)
	mustTick(t, a)
	mustTick(t, b)
	// A dies; C takes the expired lease before B looks again.
	clk.Advance(31 * time.Second)
	c := NewElector(st, "fleet:x", "C", 30*time.Second, WithClock(clk.Now))
	mustTick(t, c)
	mustTick(t, b)
	if s := b.Status(); !s.LeadershipRefused || !strings.Contains(s.RefusedReason, "C") {
		t.Fatalf("status = %+v, want refused on seeing C take the lease", s)
	}
}

func TestElector_WithoutRefusalOptionForeignHolderIsPlainFollower(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	st := newMemStore(clk.Now)
	a := NewElector(st, "s", "A", 30*time.Second, WithClock(clk.Now))
	b := NewElector(st, "s", "B", 30*time.Second, WithClock(clk.Now))
	for i := 0; i < 3; i++ {
		mustTick(t, a)
		mustTick(t, b)
		clk.Advance(10 * time.Second)
	}
	if s := b.Status(); s.LeadershipRefused || s.RefusedReason != "" || s.Holder != "A" {
		t.Fatalf("status = %+v, want an unrefused follower of A", s)
	}
}

func TestElector_RefuseStopsLeaderAndStoreCalls(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	st := newMemStore(clk.Now)
	sink := &logSink{}
	e := NewElector(st, "s", "A", 30*time.Second, WithClock(clk.Now),
		WithLogger(sink.log))
	mustTick(t, e)
	if !e.IsLeader() {
		t.Fatal("want leader before refusal")
	}
	e.Refuse("control database down")
	e.Refuse("second reason is ignored")
	calls := st.calls
	mustTick(t, e)
	if e.IsLeader() || st.calls != calls || !e.Refused() {
		t.Fatalf("leader=%v calls %d->%d refused=%v, want follower with no store call",
			e.IsLeader(), calls, st.calls, e.Refused())
	}
	if s := e.Status(); s.RefusedReason != "control database down" || s.Epoch != 0 {
		t.Fatalf("status = %+v, want first reason and no epoch", s)
	}
	if n := countLines(sink, "ERROR", "control database down"); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1", n)
	}
}

func TestElector_NilRefuseAndRefusedAreSafe(t *testing.T) {
	var e *Elector
	e.Refuse("x")
	if e.Refused() || e.Status().LeadershipRefused {
		t.Fatal("nil elector is election-off: never refused")
	}
}

// identityStore reports a control identity on every acquire.
type identityStore struct{ *memStore }

func (s identityStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	l, held, err := s.memStore.Acquire(ctx, scope, holder, ttl)
	l.Control = ControlIdentity{SystemIdentifier: "7301234567890123456", Datname: "db1"}
	return l, held, err
}

func TestElector_StatusReportsControlIdentity(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	e := NewElector(identityStore{newMemStore(clk.Now)}, "s", "A", 30*time.Second,
		WithClock(clk.Now))
	if s := e.Status(); s.ControlDatname != "" || s.ControlSystemIdentifier != "" {
		t.Fatalf("before any tick = %+v, want no identity", s)
	}
	mustTick(t, e)
	s := e.Status()
	if s.ControlDatname != "db1" || s.ControlSystemIdentifier != "7301234567890123456" {
		t.Fatalf("status = %+v, want db1 and its system identifier", s)
	}
}

func TestElector_ConcurrentRefuseAndTickNeverLeads(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	e := NewElector(newMemStore(clk.Now), "s", "A", 30*time.Second, WithClock(clk.Now))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = e.Tick(context.Background()) }()
		go func() { defer wg.Done(); e.Refuse("down") }()
	}
	wg.Wait()
	_ = e.Tick(context.Background())
	if e.IsLeader() || !e.Refused() {
		t.Fatalf("leader=%v refused=%v, want a refused follower", e.IsLeader(), e.Refused())
	}
}
