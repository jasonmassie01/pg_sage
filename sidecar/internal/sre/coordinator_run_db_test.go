package sre

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The coordinator loop: it polls committed triggers only when automatic
// start is on, resumes pending work either way, applies retention, and a
// metadata outage puts it in the explicit degraded state (CHECK-16).

type staticTriggers struct {
	mu    sync.Mutex
	list  []Trigger
	err   error
	polls int
}

func (s *staticTriggers) Triggers(context.Context) ([]Trigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	return append([]Trigger(nil), s.list...), s.err
}

func (s *staticTriggers) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

func runFor(c *Coordinator, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c.Run(ctx)
}

func waitState(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	want State) []Investigation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		page, err := st.List(ctx, scope, ListFilter{})
		if err == nil && len(page.Items) > 0 && page.Items[0].State == want {
			return page.Items
		}
		time.Sleep(50 * time.Millisecond)
	}
	page, _ := st.List(ctx, scope, ListFilter{})
	t.Fatalf("no investigation reached %s: %+v", want, page.Items)
	return nil
}

func TestCoordinatorRun_AutomaticStartInvestigatesTriggers(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	src := &staticTriggers{list: []Trigger{lockTrigger("inc-run"), lockTrigger("inc-run")}}
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), src)
	c.cfg.AutomaticStart = true
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	items := waitState(t, ctx, st, mustScope(t, c), StateConcluded)
	cancel()
	<-done
	if len(items) != 1 || items[0].IncidentID != "inc-run" {
		t.Fatalf("investigations = %+v, want one for the duplicated trigger", items)
	}
}

func TestCoordinatorRun_WithoutAutomaticStartOnlyResumes(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	src := &staticTriggers{list: []Trigger{lockTrigger("inc-manual")}}
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), src)
	queued, _, err := c.Start(ctx, lockTrigger("inc-queued"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	runFor(c, 1500*time.Millisecond)
	if src.count() != 0 {
		t.Fatalf("triggers polled %d times with automatic start off", src.count())
	}
	page, _ := st.List(ctx, mustScope(t, c), ListFilter{})
	if len(page.Items) != 1 || page.Items[0].ID != queued.ID ||
		page.Items[0].State != StateConcluded {
		t.Fatalf("investigations = %+v, want only the queued one, concluded", page.Items)
	}
}

// A failing trigger source is logged and retried; it never stops the loop.
func TestCoordinatorRun_TriggerSourceErrorsDoNotStopTheLoop(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	src := &staticTriggers{err: errors.New("incidents unreadable")}
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), src)
	c.cfg.AutomaticStart = true
	runFor(c, 500*time.Millisecond)
	if src.count() < 2 {
		t.Fatalf("trigger source polled %d times, want retries", src.count())
	}
}

func TestCoordinatorRun_AppliesRetention(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("inc-old"))
	backdate(t, ctx, pool, inv.ID, 120)
	runFor(c, 700*time.Millisecond)
	if _, err := st.Get(ctx, inv.Scope, inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 120-day-old investigation survived the loop: %v", err)
	}
	if tb, _ := st.Tombstones(ctx, inv.Scope, inv.ID); len(tb) != 1 {
		t.Fatalf("tombstones = %+v", tb)
	}
}

// CHECK-16: a metadata outage is an explicit degraded state; the loop
// keeps running and recovers when the store is back.
func TestCoordinator_MetadataOutageDegradesExplicitly(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	broken, err := pgxpool.New(ctx, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(broken.Close)
	down, _ := NewPostgresStore(broken, DefaultLimits())
	c.store = down
	if _, _, err := c.Start(ctx, lockTrigger("inc-down")); !errors.Is(err,
		ErrMetadataUnavailable) {
		t.Fatalf("start during outage = %v, want ErrMetadataUnavailable", err)
	}
	if !c.Durability().Status().Degraded || c.Durability().HandoffAllowed() {
		t.Fatalf("durability = %+v, want degraded with handoff blocked",
			c.Durability().Status())
	}
	c.store = st
	runFor(c, 300*time.Millisecond)
	if c.Durability().Status().Degraded {
		t.Fatal("the loop did not verify the store and clear the outage")
	}
}
