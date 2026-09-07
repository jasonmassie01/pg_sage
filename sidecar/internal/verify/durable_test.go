package verify

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWatchPersistsPendingThenTerminalState(t *testing.T) {
	store := newMemoryStateStore()
	request := successfulWatchRequest("durable-success")
	verdict, err := newTestEngine(t, newFakeObservationSource(), store).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	state, err := store.Get(context.Background(), request.ID)
	if err != nil {
		t.Fatalf("Get state: %v", err)
	}
	if store.created != 1 || store.updated != 1 {
		t.Fatalf("create/update counts = %d/%d, want 1/1", store.created, store.updated)
	}
	if !state.Completed || state.Status != verdict.Status || state.ActionID != request.ActionID {
		t.Fatalf("stored terminal state = %#v, verdict = %#v", state, verdict)
	}
	if state.Criterion.TargetIDs[0] != 42 || state.ExecutedAt != request.ExecutedAt {
		t.Fatalf("stored state lost restart inputs: %#v", state)
	}
}

func TestExtendedWatchPersistsNextEvaluationAndWindow(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 1, AverageLatency: time.Millisecond}
	store := newMemoryStateStore()
	request := successfulWatchRequest("durable-extension")
	verdict, err := newTestEngine(t, source, store).Watch(context.Background(), request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	state, err := store.Get(context.Background(), request.ID)
	if err != nil {
		t.Fatalf("Get state: %v", err)
	}
	if state.Completed || state.Status != "extended" {
		t.Fatalf("extended state = %#v", state)
	}
	if state.Window != verdict.Window || state.NextEvaluationAt != verdict.NextEvaluationAt {
		t.Fatalf("state/verdict scheduling mismatch: %#v %#v", state, verdict)
	}
}

func TestStoreCreateFailurePreventsObservation(t *testing.T) {
	source := newFakeObservationSource()
	source.queryStarted = make(chan struct{}, 1)
	store := newMemoryStateStore()
	store.err = errors.New("durable store down")
	verdict, err := newTestEngine(t, source, store).Watch(
		context.Background(), successfulWatchRequest("create-error"),
	)
	if err == nil || !verdict.Revert || verdict.Reason != "state_persist_failed" {
		t.Fatalf("Watch = %#v, %v; want fail-closed store error", verdict, err)
	}
	select {
	case <-source.queryStarted:
		t.Fatal("metrics read before durable state was created")
	default:
	}
}

func TestStoreUpdateFailureReturnsFailClosedVerdict(t *testing.T) {
	source := newFakeObservationSource()
	store := &failUpdateStore{memoryStateStore: newMemoryStateStore()}
	verdict, err := newTestEngine(t, source, store).Watch(
		context.Background(), successfulWatchRequest("update-error"),
	)
	if err == nil || !verdict.Revert || verdict.Status != "unverifiable" {
		t.Fatalf("Watch = %#v, %v; want fail-closed update error", verdict, err)
	}
}

type failUpdateStore struct{ *memoryStateStore }

func (s *failUpdateStore) Update(context.Context, WatchState) error {
	return errors.New("update failed")
}

func TestResumeDueEvaluatesPersistedWatchAfterRestart(t *testing.T) {
	source := newFakeObservationSource()
	store := newMemoryStateStore()
	request := successfulWatchRequest("resume-due")
	state := WatchStateFromRequest(request)
	state.Status = "extended"
	state.Window = 4 * time.Hour
	state.NextEvaluationAt = testVerificationNow().Add(-time.Minute)
	if err := store.Create(context.Background(), state); err != nil {
		t.Fatalf("seed durable state: %v", err)
	}
	engine := newTestEngine(t, source, store)

	verdicts, err := engine.ResumeDue(context.Background())
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(verdicts) != 1 || !verdicts[0].Retain {
		t.Fatalf("resumed verdicts = %#v, want one retained", verdicts)
	}
	got, err := store.Get(context.Background(), request.ID)
	if err != nil || !got.Completed {
		t.Fatalf("resumed state = %#v, %v; want completed", got, err)
	}
}

func TestResumeDueIgnoresFutureAndCompletedWatches(t *testing.T) {
	store := newMemoryStateStore()
	future := WatchStateFromRequest(successfulWatchRequest("future"))
	future.NextEvaluationAt = testVerificationNow().Add(time.Hour)
	completed := WatchStateFromRequest(successfulWatchRequest("completed"))
	completed.NextEvaluationAt = testVerificationNow().Add(-time.Hour)
	completed.Completed = true
	for _, state := range []WatchState{future, completed} {
		if err := store.Create(context.Background(), state); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	verdicts, err := newTestEngine(
		t, newFakeObservationSource(), store,
	).ResumeDue(context.Background())
	if err != nil {
		t.Fatalf("ResumeDue: %v", err)
	}
	if len(verdicts) != 0 {
		t.Fatalf("ResumeDue returned non-due watches: %#v", verdicts)
	}
}

func TestResumeDueStoreErrorFailsClosed(t *testing.T) {
	store := newMemoryStateStore()
	store.err = errors.New("list due failed")
	verdicts, err := newTestEngine(
		t, newFakeObservationSource(), store,
	).ResumeDue(context.Background())
	if err == nil || verdicts != nil {
		t.Fatalf("ResumeDue = %#v, %v; want store error", verdicts, err)
	}
}
