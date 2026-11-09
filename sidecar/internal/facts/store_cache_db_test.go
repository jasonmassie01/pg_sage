package facts

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// facts-11: a decision committed while Confirmed is reading must not be
// hidden by the older rows that read returns.

func proposeSlot(t *testing.T, s *Store, slot string) Fact {
	t.Helper()
	f, _, err := s.Propose(t.Context(), Proposal{Type: TypeSlotConsumer, Kind: KindSlot,
		Subject: slot, Source: SourceDetector, Value: map[string]string{"consumer": "debezium"},
		Evidence: []Citation{citation("slot:" + slot)}})
	if err != nil {
		t.Fatalf("propose %s: %v", slot, err)
	}
	return f
}

func confirmedIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	got, err := s.Confirmed(t.Context())
	if err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	var ids []int64
	for _, f := range got {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestConfirmedDropsAReadOutdatedByADecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := proposeSlot(t, s, "cdc_orders")
	other := NewStore(pool) // a chat card's store: same pool, same cache
	var once sync.Once
	s.afterConfirmedRead = func() {
		once.Do(func() {
			if _, err := other.Decide(ctx, f.ID, Decision{Confirm: true,
				Actor: "op"}); err != nil {
				t.Errorf("decide while Confirmed reads: %v", err)
			}
		})
	}
	// The read predates the decision, so it returns the rows it saw.
	if ids := confirmedIDs(t, s); len(ids) != 0 {
		t.Fatalf("the first read saw %v before the decision committed", ids)
	}
	s.afterConfirmedRead = nil
	if ids := confirmedIDs(t, s); len(ids) != 1 || ids[0] != f.ID {
		t.Fatalf("after the decision Confirmed returned %v, want [%d] (stale cache)",
			ids, f.ID)
	}
	if ids := confirmedIDs(t, other); len(ids) != 1 || ids[0] != f.ID {
		t.Fatalf("the deciding store sees %v", ids)
	}
}

// Expiry (a state transition away from confirmed) racing a read is not
// hidden either: the expired fact stops binding at once.
func TestConfirmedDropsAReadOutdatedByAnExpiry(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := proposeSlot(t, s, "cdc_orders")
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	s.afterConfirmedRead = func() {
		once.Do(func() {
			if _, err := NewStore(pool).Expire(ctx, f.ID, "ops", "slot dropped"); err != nil {
				t.Errorf("expire while Confirmed reads: %v", err)
			}
		})
	}
	if ids := confirmedIDs(t, s); len(ids) != 1 {
		t.Fatalf("the first read predates the expiry: %v", ids)
	}
	s.afterConfirmedRead = nil
	if ids := confirmedIDs(t, s); len(ids) != 0 {
		t.Fatalf("an expired fact still binds from the cache: %v", ids)
	}
}

// Readers hammering Confirmed while decisions land: once a Decide has
// returned, the next Confirmed always includes it.
func TestConfirmedConcurrentReadersSeeEveryReturnedDecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	const n = 5
	var facts []Fact
	for i := 0; i < n; i++ {
		facts = append(facts, proposeSlot(t, s, uniqueName("cdc_")+string(rune('a'+i))))
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader := NewStore(pool)
			for !stop.Load() {
				if _, err := reader.Confirmed(ctx); err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
			}
		}()
	}
	defer func() { stop.Store(true); wg.Wait() }()
	decider := NewStore(pool)
	for i, f := range facts {
		if _, err := decider.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
			t.Fatal(err)
		}
		if ids := confirmedIDs(t, s); len(ids) != i+1 || ids[i] != f.ID {
			t.Fatalf("after confirming %d facts Confirmed returned %v", i+1, ids)
		}
	}
}

// The TTL is unchanged: a write that bypasses the store (another process)
// is seen once confirmedTTL has passed, not before.
func TestConfirmedCacheHonoursTheTTL(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	if ids := confirmedIDs(t, s); len(ids) != 0 {
		t.Fatalf("empty store: %v", ids)
	}
	execAll(t, ctx, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject, value,
		source, status, decided_by, decided_at) VALUES ('slot_consumer', 'slot', 'cdc_x',
		'{"consumer":"debezium"}', 'operator', 'confirmed', 'op', now())`)
	clock = clock.Add(confirmedTTL - time.Millisecond)
	if ids := confirmedIDs(t, s); len(ids) != 0 {
		t.Fatalf("inside the TTL the cache serves: %v", ids)
	}
	clock = clock.Add(time.Millisecond)
	if ids := confirmedIDs(t, s); len(ids) != 1 {
		t.Fatalf("at the TTL the cache is re-read: %v", ids)
	}
}

// A failed read is reported with context and leaves no cache behind.
func TestConfirmedReadErrorIsNotCached(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := proposeSlot(t, s, "cdc_orders")
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := ctxCancelled(t)
	defer cancel()
	if _, err := s.Confirmed(cctx); err == nil ||
		!strings.Contains(err.Error(), "confirmed facts") {
		t.Fatalf("a cancelled read must fail with context, got %v", err)
	}
	if ids := confirmedIDs(t, s); len(ids) != 1 || ids[0] != f.ID {
		t.Fatalf("after a failed read Confirmed returned %v", ids)
	}
}

// ctxCancelled is an already-cancelled context for error-path reads.
func ctxCancelled(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx, cancel
}
