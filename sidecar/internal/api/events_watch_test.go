package api

import (
	"testing"
)

// Performance gate: the live-update poll reads whole history tables, so it
// runs only while a dashboard is subscribed. With nobody watching it must
// not touch the database (reviews/2026-10-03-perf-gate-report.md).
func TestEventBrokerPollsOnlyWhileWatched(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	mgr := phase2MgrWithPool(pool)
	b := NewEventBroker()
	state := map[string]map[EventType]lastSeen{}

	if b.pollIfWatched(ctx, mgr, state) {
		t.Fatal("polled with no subscriber")
	}
	if len(state) != 0 {
		t.Fatalf("unwatched poll recorded state %v", state)
	}

	ch, cancel := b.Subscribe()
	if !b.pollIfWatched(ctx, mgr, state) {
		t.Fatal("did not poll with a subscriber")
	}
	if len(state["testdb"]) != 3 {
		t.Fatalf("watched poll recorded %d resources, want 3", len(state["testdb"]))
	}
	assertNoEvent(t, ch) // the first observation is the baseline
	cancel()

	if b.pollIfWatched(ctx, mgr, state) {
		t.Fatal("polled after the last subscriber left")
	}
}

func TestEventBrokerPollIfWatchedNilManager(t *testing.T) {
	b := NewEventBroker()
	_, cancel := b.Subscribe()
	defer cancel()
	state := map[string]map[EventType]lastSeen{}
	if !b.pollIfWatched(t.Context(), nil, state) || len(state) != 0 {
		t.Fatalf("nil manager: state = %v", state)
	}
}
