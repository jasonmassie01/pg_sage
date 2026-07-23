package verify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errObservation = errors.New("observation unavailable")

type fakeObservationSource struct {
	mu           sync.Mutex
	executedAt   time.Time
	before       map[int64]Measurement
	after        map[int64]Measurement
	writeBefore  Measurement
	writeAfter   Measurement
	indexValid   bool
	load         LoadSample
	queryErr     error
	writeErr     error
	indexErr     error
	loadErr      error
	queryStarted chan struct{}
	queryGate    chan struct{}
}

func newFakeObservationSource() *fakeObservationSource {
	return &fakeObservationSource{
		executedAt: testVerificationNow().Add(-2 * time.Hour),
		before: map[int64]Measurement{
			42: {Samples: 60, AverageLatency: 100 * time.Millisecond},
		},
		after: map[int64]Measurement{
			42: {Samples: 60, AverageLatency: 70 * time.Millisecond},
		},
		writeBefore: Measurement{Samples: 60, AverageLatency: 10 * time.Millisecond},
		writeAfter:  Measurement{Samples: 60, AverageLatency: 11 * time.Millisecond},
		indexValid:  true,
		load:        LoadSample{CPUPct: 20, DataIOPct: 20, LogIOPct: 20},
	}
}

func (s *fakeObservationSource) QueryMeasurements(
	ctx context.Context, ids []int64, from, to time.Time,
) (map[int64]Measurement, error) {
	if s.queryStarted != nil {
		select {
		case s.queryStarted <- struct{}{}:
		default:
		}
	}
	if s.queryGate != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.queryGate:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	if !to.After(s.executedAt) {
		return copyMeasurements(s.before, ids), nil
	}
	return copyMeasurements(s.after, ids), nil
}

func (s *fakeObservationSource) WriteMeasurements(
	_ context.Context, _ string, from, to time.Time,
) (Measurement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return Measurement{}, s.writeErr
	}
	if !to.After(s.executedAt) {
		return s.writeBefore, nil
	}
	return s.writeAfter, nil
}

func (s *fakeObservationSource) IndexValid(
	_ context.Context, _ string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexValid, s.indexErr
}

func (s *fakeObservationSource) CurrentLoad(context.Context) (LoadSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load, s.loadErr
}

func copyMeasurements(
	source map[int64]Measurement, ids []int64,
) map[int64]Measurement {
	result := make(map[int64]Measurement, len(ids))
	for _, id := range ids {
		if measurement, ok := source[id]; ok {
			result[id] = measurement
		}
	}
	return result
}

type memoryStateStore struct {
	mu      sync.Mutex
	states  map[string]WatchState
	created int
	updated int
	err     error
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{states: make(map[string]WatchState)}
}

func (s *memoryStateStore) Create(_ context.Context, state WatchState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, exists := s.states[state.ID]; exists {
		return errors.New("watch already exists")
	}
	s.states[state.ID] = state
	s.created++
	return nil
}

func (s *memoryStateStore) Update(_ context.Context, state WatchState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.states[state.ID] = state
	s.updated++
	return nil
}

func (s *memoryStateStore) Get(_ context.Context, id string) (WatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[id]
	if !ok {
		return WatchState{}, errors.New("watch not found")
	}
	return state, nil
}

func (s *memoryStateStore) ListDue(
	_ context.Context, now time.Time,
) ([]WatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var due []WatchState
	for _, state := range s.states {
		if !state.NextEvaluationAt.After(now) && !state.Completed {
			due = append(due, state)
		}
	}
	return due, nil
}

func testVerificationNow() time.Time {
	return time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
}

func successfulWatchRequest(id string) WatchRequest {
	return WatchRequest{
		ID:         id,
		ActionID:   101,
		ExecutedAt: testVerificationNow().Add(-2 * time.Hour),
		Table:      "public.orders",
		IndexName:  "public.orders_customer_idx",
		Criterion: Criterion{
			Kind:           "per_query_latency",
			TargetIDs:      []int64{42},
			MinGainPct:     20,
			RegressPct:     15,
			WriteImpactPct: 20,
			Window:         2 * time.Hour,
			HardMax:        72 * time.Hour,
		},
	}
}

func newTestEngine(
	t *testing.T, source ObservationSource, store StateStore,
) *Engine {
	t.Helper()
	options := DefaultOptions()
	options.Now = testVerificationNow
	engine, err := NewEngine(source, store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}
