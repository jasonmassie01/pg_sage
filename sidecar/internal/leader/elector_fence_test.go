package leader

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The token names the scope the lease row is stored under, whatever form
// the scope takes (meta, YAML fleet hash, standalone, governance).
func TestElector_FenceTokenCarriesTheLeaseScope(t *testing.T) {
	for _, scope := range []string{"meta", "fleet:0123456789abcdef",
		"standalone:app", "agents:fleet:0123456789abcdef"} {
		c := newClock()
		st := newMemStore(c.Now)
		e := NewElector(st, scope, "A", 30*time.Second, WithClock(c.Now))
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("%s: tick: %v", scope, err)
		}
		tok, ok := e.Fence()
		if !ok {
			t.Fatalf("%s: leader has no fence", scope)
		}
		want := Token{Scope: scope, Holder: "A", Epoch: 1}
		if tok != want {
			t.Fatalf("%s: token = %+v, want %+v", scope, tok, want)
		}
		row, found := st.lease[tok.Scope]
		if !found || row.Holder != tok.Holder || row.Epoch != tok.Epoch {
			t.Fatalf("%s: no lease row matches token %+v (rows %+v)", scope, tok, st.lease)
		}
	}
}

// A follower and a nil (disabled) elector hand out the zero token.
func TestElector_FollowerAndNilFenceAreZero(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	_ = b.Tick(context.Background())
	if tok, ok := b.Fence(); ok || tok != (Token{}) {
		t.Fatalf("follower fence = %+v,%v, want zero,false", tok, ok)
	}
	var nilE *Elector
	if tok, ok := nilE.Fence(); ok || tok != (Token{}) {
		t.Fatalf("nil elector fence = %+v,%v, want zero,false", tok, ok)
	}
	if tok, ok := NewElector(st, "fleet", "C", time.Second).Fence(); ok ||
		tok != (Token{}) {
		t.Fatalf("never-ticked elector fence = %+v,%v, want zero,false", tok, ok)
	}
}

// The fence ends at the local deadline (ttl - ttl/SafetyDivisor after the
// acquire started), not at the lease's own expiry.
func TestElector_FenceEndsAtTheLocalDeadline(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	e := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	_ = e.Tick(context.Background())
	c.Advance(24*time.Second - time.Nanosecond)
	if tok, ok := e.Fence(); !ok || tok.Epoch != 1 {
		t.Fatalf("just before the deadline: %+v,%v, want epoch 1", tok, ok)
	}
	c.Advance(time.Nanosecond)
	if tok, ok := e.Fence(); ok || tok != (Token{}) {
		t.Fatalf("at the deadline: %+v,%v, want zero,false", tok, ok)
	}
}

// Takeover: the new holder's token carries the bumped epoch; after
// Release the old holder's fence is gone at once.
func TestElector_FenceTransitions(t *testing.T) {
	c := newClock()
	st := newMemStore(c.Now)
	a := NewElector(st, "fleet", "A", 30*time.Second, WithClock(c.Now))
	b := NewElector(st, "fleet", "B", 30*time.Second, WithClock(c.Now))
	_ = a.Tick(context.Background())
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if tok, ok := a.Fence(); ok || tok != (Token{}) {
		t.Fatalf("released fence = %+v,%v, want zero,false", tok, ok)
	}
	_ = b.Tick(context.Background())
	want := Token{Scope: "fleet", Holder: "B", Epoch: 2}
	if tok, ok := b.Fence(); !ok || tok != want {
		t.Fatalf("successor fence = %+v,%v, want %+v", tok, ok, want)
	}
}

// Fence read while Tick renews never returns a torn token.
func TestElector_FenceConcurrentWithTick(t *testing.T) {
	st := newMemStore(time.Now)
	e := NewElector(st, "fleet:x", "A", time.Minute)
	_ = e.Tick(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = e.Tick(context.Background())
		}
	}()
	for i := 0; i < 500; i++ {
		tok, ok := e.Fence()
		if !ok || tok != (Token{Scope: "fleet:x", Holder: "A", Epoch: 1}) {
			t.Fatalf("read %d: fence = %+v,%v", i, tok, ok)
		}
	}
	wg.Wait()
}
