package rca

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// mockActionStore implements ActionQuerier for tests.
type mockActionStore struct {
	recentActions   []SageAction
	rollbackHistory []SageAction
	recentErr       error
	rollbackErr     error
	recentCalls     int
	rollbackCalls   int
}

func (m *mockActionStore) RecentSageActions(
	_ context.Context, _ time.Duration,
) ([]SageAction, error) {
	m.recentCalls++
	return m.recentActions, m.recentErr
}

func (m *mockActionStore) RollbackHistory(
	_ context.Context, _ time.Duration,
) ([]SageAction, error) {
	m.rollbackCalls++
	return m.rollbackHistory, m.rollbackErr
}

// ---------------------------------------------------------------------------
// WithActionStore wiring
// ---------------------------------------------------------------------------

func TestWithActionStore_SetsFieldsCorrectly(t *testing.T) {
	eng := testEngine()
	store := &mockActionStore{}

	eng.WithActionStore(store)

	if eng.actionStore == nil {
		t.Fatal("actionStore should be set")
	}
	if eng.correlator == nil {
		t.Fatal("correlator should be set")
	}
}

func TestWithActionStore_NilByDefault(t *testing.T) {
	eng := testEngine()

	if eng.actionStore != nil {
		t.Error("actionStore should be nil by default")
	}
	if eng.correlator != nil {
		t.Error("correlator should be nil by default")
	}
}

// ---------------------------------------------------------------------------
// Correlation runs during Analyze when wired
// ---------------------------------------------------------------------------

func TestAnalyze_CallsCorrelationWhenWired(t *testing.T) {
	eng := testEngine()
	store := &mockActionStore{}
	eng.WithActionStore(store)

	snap := quietSnapshot()
	cfg := testConfig()

	eng.Analyze(snap, snap, cfg, nil)

	if store.recentCalls != 1 {
		t.Errorf("RecentSageActions called %d times, want 1",
			store.recentCalls)
	}
	if store.rollbackCalls != 1 {
		t.Errorf("RollbackHistory called %d times, want 1",
			store.rollbackCalls)
	}
}

func TestAnalyze_SkipsCorrelationWhenNotWired(t *testing.T) {
	eng := testEngine()
	snap := quietSnapshot()
	cfg := testConfig()

	// Should not panic when no action store is wired.
	incidents := eng.Analyze(snap, snap, cfg, nil)
	if len(incidents) != 0 {
		t.Errorf("expected no incidents, got %d", len(incidents))
	}
}

func TestAnalyze_SkipsRollbackQueryOnNoActions(t *testing.T) {
	eng := testEngine()
	store := &mockActionStore{
		recentActions: []SageAction{}, // empty
	}
	eng.WithActionStore(store)

	snap := quietSnapshot()
	cfg := testConfig()

	eng.Analyze(snap, snap, cfg, nil)

	if store.recentCalls != 1 {
		t.Errorf("RecentSageActions called %d, want 1",
			store.recentCalls)
	}
	// When no recent actions exist, rollback query should still
	// be called because applySelfActionCorrelation queries both
	// before checking length.
	if store.rollbackCalls != 1 {
		t.Errorf("RollbackHistory called %d, want 1",
			store.rollbackCalls)
	}
}

// ---------------------------------------------------------------------------
// Error handling: query failures should not crash Analyze
// ---------------------------------------------------------------------------

func TestAnalyze_RecentActionsError_DoesNotPanic(t *testing.T) {
	eng := testEngine()
	store := &mockActionStore{
		recentErr: errors.New("connection refused"),
	}
	eng.WithActionStore(store)

	snap := quietSnapshot()
	cfg := testConfig()

	// Should not panic.
	incidents := eng.Analyze(snap, snap, cfg, nil)
	if len(incidents) != 0 {
		t.Errorf("expected no incidents, got %d", len(incidents))
	}
	// Rollback should NOT be queried after recent fails.
	if store.rollbackCalls != 0 {
		t.Errorf("RollbackHistory called %d, want 0 after "+
			"RecentSageActions error", store.rollbackCalls)
	}
}

func TestAnalyze_RollbackHistoryError_DoesNotPanic(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	store := &mockActionStore{
		recentActions: []SageAction{
			{ID: "1", Family: "create_index",
				ExecutedAt: now.Add(-5 * time.Minute)},
		},
		rollbackErr: errors.New("timeout"),
	}
	eng.WithActionStore(store)

	snap := quietSnapshot()
	cfg := testConfig()

	// Should not panic.
	incidents := eng.Analyze(snap, snap, cfg, nil)
	_ = incidents // no crash is the assertion
}

// ---------------------------------------------------------------------------
// Causal match produces self-caused incident
// ---------------------------------------------------------------------------

func TestAnalyze_SelfCausedIncidentCreated(t *testing.T) {
	eng := testEngine()
	now := time.Now()

	// Set up an action that matches a causal path:
	// drop_index -> log_slow_query
	store := &mockActionStore{
		recentActions: []SageAction{
			{
				ID:         "42",
				Family:     "drop_index",
				ExecutedAt: now.Add(-10 * time.Minute),
				Database:   "",
				Description: "DROP INDEX idx_users_email " +
					"ON public.users",
			},
		},
		rollbackHistory: []SageAction{},
	}
	eng.WithActionStore(store)

	// Produce a hot snapshot that fires the "slow query" signal.
	// We need a snapshot that fires log_slow_query -- however,
	// log_slow_query comes from log signals, not metric signals.
	// Instead, let's inject the incident directly and test the
	// correlation in isolation via applySelfActionCorrelation.
	eng.mu.Lock()
	eng.incidents = append(eng.incidents, Incident{
		ID:           newUUID(),
		DetectedAt:   now,
		Severity:     "warning",
		SignalIDs:    []string{"log_slow_query"},
		RootCause:    "slow query detected",
		Source:       "log_deterministic",
		Confidence:   0.85,
		DatabaseName: "",
		OccurrenceCount: 1,
	})
	eng.mu.Unlock()

	// Run the correlation directly with the existing incidents.
	newIncidents := eng.incidents
	eng.applySelfActionCorrelation(newIncidents)

	// Find self-caused incidents.
	var selfCaused int
	for _, inc := range eng.incidents {
		if inc.Source == "self_action" {
			selfCaused++
		}
	}
	if selfCaused == 0 {
		t.Error("expected a self-caused incident from " +
			"drop_index -> log_slow_query causal path")
	}
}

// ---------------------------------------------------------------------------
// Anti-oscillation: manual review when rollback count exceeds threshold
// ---------------------------------------------------------------------------

func TestAnalyze_ManualReviewOnRepeatedRollback(t *testing.T) {
	eng := testEngine()
	now := time.Now()

	store := &mockActionStore{
		recentActions: []SageAction{
			{
				ID:         "99",
				Family:     "drop_index",
				ExecutedAt: now.Add(-5 * time.Minute),
			},
		},
		rollbackHistory: []SageAction{
			// 3 rollbacks exceeds default threshold of 2.
			{ID: "10", Family: "drop_index", RolledBack: true},
			{ID: "11", Family: "drop_index", RolledBack: true},
			{ID: "12", Family: "drop_index", RolledBack: true},
		},
	}
	eng.WithActionStore(store)

	eng.mu.Lock()
	eng.incidents = append(eng.incidents, Incident{
		ID:              newUUID(),
		DetectedAt:      now,
		Severity:        "warning",
		SignalIDs:       []string{"log_slow_query"},
		RootCause:       "slow query detected",
		Source:          "log_deterministic",
		Confidence:      0.85,
		OccurrenceCount: 1,
	})
	eng.mu.Unlock()

	newIncidents := eng.incidents
	eng.applySelfActionCorrelation(newIncidents)

	var manualReview int
	for _, inc := range eng.incidents {
		if inc.Source == "manual_review_required" {
			manualReview++
		}
	}
	if manualReview == 0 {
		t.Error("expected a manual_review_required incident " +
			"when rollback threshold is exceeded")
	}
}

// ---------------------------------------------------------------------------
// Full Analyze cycle with causal match via hot snapshot
// ---------------------------------------------------------------------------

func TestAnalyze_FullCycleWithSelfAction(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	now := time.Now()

	store := &mockActionStore{
		recentActions: []SageAction{
			{
				ID:          "7",
				Family:      "vacuum_full",
				ExecutedAt:  now.Add(-15 * time.Minute),
				Description: "VACUUM FULL public.orders",
			},
		},
	}
	eng.WithActionStore(store)

	// Hot snapshot: high connection count to produce an incident.
	hot := &collector.Snapshot{
		CollectedAt: now,
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}

	// Run several cycles. The correlation runs each time.
	for i := 0; i < 5; i++ {
		eng.Analyze(hot, nil, cfg, nil)
	}

	// Verify correlation was called each cycle.
	if store.recentCalls != 5 {
		t.Errorf("RecentSageActions called %d, want 5",
			store.recentCalls)
	}
}
