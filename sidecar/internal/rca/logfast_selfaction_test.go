package rca

import (
	"context"
	"errors"
	"testing"
	"time"
)

// rca-06 review: the log fast path takes critical log signals before the
// analyzer cycle sees them, so it must run the cycle's self-action
// correlation itself, or "Index creation consumed remaining disk space"
// and the anti-oscillation manual review would never fire.

// No concurrent access tests here: the engine's e.mu handling is covered by
// TestLogFastPathAndCycleRaceNotifyOnce; these tests pin the correlation.

func selfActionEngine(store *mockActionStore) *Engine {
	eng := testEngine()
	eng.WithDatabaseName("app")
	eng.WithActionStore(store)
	return eng
}

func createIndexAction(id string) SageAction {
	return SageAction{ID: id, Family: "create_index",
		ExecutedAt:  time.Now().Add(-5 * time.Minute),
		Description: "CREATE INDEX CONCURRENTLY idx_orders_customer ON orders"}
}

func incidentsBySource(eng *Engine, source string) []Incident {
	var out []Incident
	for _, inc := range eng.ActiveIncidents() {
		if inc.Source == source {
			out = append(out, inc)
		}
	}
	return out
}

// Happy path: a disk-full line after pg_sage built an index opens the
// self-caused incident on the fast path, next to the plain one.
func TestObserveLogSignalsAttributesSelfCausedDiskFull(t *testing.T) {
	store := &mockActionStore{recentActions: []SageAction{createIndexAction("42")}}
	eng := selfActionEngine(store)
	touched := eng.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	if len(touched) != 1 || touched[0].Source != "log_deterministic" {
		t.Fatalf("touched = %+v, want the log_deterministic disk-full incident", touched)
	}
	self := incidentsBySource(eng, "self_action")
	if len(self) != 1 {
		t.Fatalf("self_action incidents = %d, want 1", len(self))
	}
	if self[0].Severity != "critical" || self[0].DatabaseName != "app" ||
		len(self[0].SignalIDs) != 1 || self[0].SignalIDs[0] != "log_disk_full" {
		t.Fatalf("self-caused incident = %+v", self[0])
	}
	if store.recentCalls != 1 || store.rollbackCalls != 1 {
		t.Fatalf("action store reads = %d/%d, want 1/1",
			store.recentCalls, store.rollbackCalls)
	}
}

// Boundary: a repeat sighting in the same analyzer-cycle window neither
// reads the action store again nor counts another self-caused occurrence;
// the first sighting after a cycle does both, as the cycle would.
func TestObserveLogSignalsCorrelatesOncePerCycleWindow(t *testing.T) {
	store := &mockActionStore{recentActions: []SageAction{createIndexAction("42")}}
	eng := selfActionEngine(store)
	ctx := context.Background()
	eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	if store.recentCalls != 1 {
		t.Fatalf("store read %d times in one window, want 1", store.recentCalls)
	}
	if self := incidentsBySource(eng, "self_action"); len(self) != 1 ||
		self[0].OccurrenceCount != 1 {
		t.Fatalf("self-caused after a repeat = %+v, want one with 1 occurrence", self)
	}
	eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	calls := store.recentCalls
	eng.ObserveLogSignals(ctx, []*Signal{diskFullSignal()})
	if store.recentCalls != calls+1 {
		t.Fatalf("first sighting after a cycle read the store %d times, want 1",
			store.recentCalls-calls)
	}
	if self := incidentsBySource(eng, "self_action"); len(self) != 1 ||
		self[0].OccurrenceCount != 2 {
		t.Fatalf("self-caused after a cycle = %+v, want 2 occurrences", self)
	}
}

// State transition: a family rolled back twice in 30 days turns the
// attribution into the anti-oscillation manual review.
func TestObserveLogSignalsOpensManualReviewAfterRollbacks(t *testing.T) {
	rolledBack := func(id string) SageAction {
		a := createIndexAction(id)
		a.RolledBack = true
		return a
	}
	store := &mockActionStore{
		recentActions:   []SageAction{createIndexAction("42")},
		rollbackHistory: []SageAction{rolledBack("7"), rolledBack("8")},
	}
	eng := selfActionEngine(store)
	eng.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	review := incidentsBySource(eng, "manual_review_required")
	if len(review) != 1 || review[0].Severity != "critical" ||
		review[0].RecommendedSQL != "" {
		t.Fatalf("manual review incidents = %+v, want one critical, no SQL", review)
	}
	if self := incidentsBySource(eng, "self_action"); len(self) != 0 {
		t.Fatalf("self_action incidents = %d, want 0 when review is required", len(self))
	}
}

// Error propagation: an action-store failure leaves the disk-full incident
// open and uncorrelated; it never blocks the page.
func TestObserveLogSignalsActionStoreErrorKeepsIncident(t *testing.T) {
	store := &mockActionStore{recentErr: errors.New("connection refused")}
	eng := selfActionEngine(store)
	touched := eng.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	if len(touched) != 1 || touched[0].SignalIDs[0] != "log_disk_full" {
		t.Fatalf("touched = %+v, want the disk-full incident", touched)
	}
	if self := incidentsBySource(eng, "self_action"); len(self) != 0 {
		t.Fatalf("self_action incidents = %d after a store error", len(self))
	}
	if store.rollbackCalls != 0 {
		t.Fatalf("rollback history read %d times after the recent read failed",
			store.rollbackCalls)
	}
}

// Nil/empty: no action store, or no matching action, opens only the plain
// incident.
func TestObserveLogSignalsNoActionStoreOrNoMatch(t *testing.T) {
	plain := testEngine()
	plain.WithDatabaseName("app")
	plain.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	if n := len(plain.ActiveIncidents()); n != 1 {
		t.Fatalf("incidents without an action store = %d, want 1", n)
	}
	other := SageAction{ID: "9", Family: "analyze", ExecutedAt: time.Now(),
		Description: "ANALYZE orders"}
	eng := selfActionEngine(&mockActionStore{recentActions: []SageAction{other}})
	eng.ObserveLogSignals(context.Background(), []*Signal{diskFullSignal()})
	if n := len(eng.ActiveIncidents()); n != 1 {
		t.Fatalf("incidents with an unrelated action = %d, want 1", n)
	}
}
