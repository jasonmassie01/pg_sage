package verify

import (
	"context"
	"testing"
	"time"
)

// Regression tests for Codex C15 (a revert verdict must stay retryable until
// the revert effect succeeds) and G4-B28 (a zero write baseline is
// insufficient evidence, not a regression).

func regressingSource() *fakeObservationSource {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 300 * time.Millisecond, Buckets: 12}
	return source
}

func TestRevertVerdictRemainsRetryableUntilEffectCompletes(t *testing.T) {
	store := newMemoryStateStore()
	source := regressingSource()
	engine := newTestEngine(t, source, store)

	verdict, err := engine.Watch(context.Background(), successfulWatchRequest("revert-1"))

	if err != nil || !verdict.Revert {
		t.Fatalf("Watch = %#v, %v; want revert verdict", verdict, err)
	}
	state, _ := store.Get(context.Background(), "revert-1")
	if state.Completed {
		t.Fatal("revert verdict persisted as completed before the revert ran")
	}
	if !state.NextEvaluationAt.After(testVerificationNow()) {
		t.Fatalf("retry time %s is not in the future", state.NextEvaluationAt)
	}
}

func TestResumedRevertReturnsStoredVerdictWithoutReobserving(t *testing.T) {
	store := newMemoryStateStore()
	source := regressingSource()
	engine := newTestEngine(t, source, store)
	if _, err := engine.Watch(context.Background(), successfulWatchRequest("revert-2")); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	source.queryErr = errObservation
	options := DefaultOptions()
	options.Now = func() time.Time { return testVerificationNow().Add(time.Hour) }
	later, err := NewEngine(source, store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	results, err := later.ResumeDueResults(context.Background())

	if err != nil || len(results) != 1 {
		t.Fatalf("ResumeDueResults = %#v, %v", results, err)
	}
	got := results[0].Verdict
	if !got.Revert || got.Reason != "query_regression" || results[0].ActionID != 101 {
		t.Fatalf("resumed verdict = %#v, want stored query_regression revert", got)
	}
}

func TestZeroWriteBaselineIsNotWriteRegression(t *testing.T) {
	source := newFakeObservationSource()
	source.writeBefore = Measurement{Samples: 60}
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 5 * time.Millisecond, Buckets: 12}
	engine := newTestEngine(t, source, newMemoryStateStore())

	verdict, err := engine.Watch(context.Background(), successfulWatchRequest("write-0"))

	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Revert || verdict.Reason == "write_impact" {
		t.Fatalf("verdict = %#v, zero write baseline must not trigger revert", verdict)
	}
}
