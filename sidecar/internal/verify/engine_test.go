package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchRetainsWhenAnyTargetMeetsGainAndNoneRegress(t *testing.T) {
	source := newFakeObservationSource()
	source.before[43] = Measurement{Samples: 50, AverageLatency: 200 * time.Millisecond}
	source.after[43] = Measurement{Samples: 50, AverageLatency: 190 * time.Millisecond}
	request := successfulWatchRequest("useful")
	request.Criterion.TargetIDs = []int64{42, 43}

	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Retain || verdict.Revert || verdict.Status != "success" {
		t.Fatalf("verdict = %#v, want retained success", verdict)
	}
	if verdict.Samples != 100 {
		t.Fatalf("samples = %d, want 100", verdict.Samples)
	}
}

func TestWatchRevertsWhenAnyTargetRegresses(t *testing.T) {
	source := newFakeObservationSource()
	source.before[43] = Measurement{Samples: 40, AverageLatency: 100 * time.Millisecond}
	source.after[43] = Measurement{Samples: 40, AverageLatency: 116 * time.Millisecond}
	request := successfulWatchRequest("regression")
	request.Criterion.TargetIDs = []int64{42, 43}

	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Reason != "query_regression" {
		t.Fatalf("verdict = %#v, want query-regression revert", verdict)
	}
}

func TestWatchRegressionBoundaryIsStrict(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 115 * time.Millisecond}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("boundary"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Reason == "query_regression" {
		t.Fatalf("exact 15%% boundary counted as >15%% regression: %#v", verdict)
	}
}

func TestWatchRevertsWhenNoTargetMeetsMinimumGain(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 81 * time.Millisecond}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("no-gain"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Reason != "no_gain" {
		t.Fatalf("verdict = %#v, want no-gain revert", verdict)
	}
}

func TestWatchMinimumGainBoundaryCountsAsSuccess(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 80 * time.Millisecond}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("gain-boundary"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Retain || verdict.Reason == "no_gain" {
		t.Fatalf("exact 20%% gain was not retained: %#v", verdict)
	}
}

func TestWatchRevertsOnWriteImpact(t *testing.T) {
	source := newFakeObservationSource()
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 13 * time.Millisecond}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("write-impact"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Reason != "write_impact" {
		t.Fatalf("verdict = %#v, want write-impact revert", verdict)
	}
}

func TestWatchWriteImpactBoundaryIsAllowed(t *testing.T) {
	source := newFakeObservationSource()
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 12 * time.Millisecond}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("write-boundary"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Retain {
		t.Fatalf("exact 20%% write boundary rejected: %#v", verdict)
	}
}

func TestWatchDropsInvalidIndexThroughRevertVerdict(t *testing.T) {
	source := newFakeObservationSource()
	source.indexValid = false
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("invalid-index"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Reason != "invalid_index" {
		t.Fatalf("verdict = %#v, want invalid-index revert", verdict)
	}
}

func TestWatchExtendsWhenSamplesBelowThirty(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 29, AverageLatency: 60 * time.Millisecond}
	request := successfulWatchRequest("low-samples")
	store := newMemoryStateStore()
	verdict, err := newTestEngine(t, source, store).Watch(context.Background(), request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Status != "extended" || verdict.Retain || verdict.Revert {
		t.Fatalf("low-sample verdict = %#v, want extension", verdict)
	}
	if verdict.NextEvaluationAt != testVerificationNow().Add(2*time.Hour) {
		t.Fatalf("next evaluation = %s, want +2h", verdict.NextEvaluationAt)
	}
}

func TestWatchExtendsAdaptivelyAndCapsAtSeventyTwoHours(t *testing.T) {
	tests := []struct {
		name       string
		window     time.Duration
		wantWindow time.Duration
	}{
		{name: "double initial", window: 2 * time.Hour, wantWindow: 4 * time.Hour},
		{name: "double middle", window: 24 * time.Hour, wantWindow: 48 * time.Hour},
		{name: "cap hard max", window: 48 * time.Hour, wantWindow: 72 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.after[42] = Measurement{Samples: 2, AverageLatency: time.Millisecond}
			request := successfulWatchRequest(test.name)
			request.Criterion.Window = test.window
			request.ExecutedAt = testVerificationNow().Add(-test.window)
			source.executedAt = request.ExecutedAt
			verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
				context.Background(), request,
			)
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			if verdict.Window != test.wantWindow {
				t.Fatalf("extended window = %s, want %s", verdict.Window, test.wantWindow)
			}
		})
	}
}

func TestWatchRevertsToSafeAtHardMaxWithoutSamples(t *testing.T) {
	source := newFakeObservationSource()
	source.after = map[int64]Measurement{}
	request := successfulWatchRequest("hard-max")
	request.Criterion.Window = 72 * time.Hour
	request.ExecutedAt = testVerificationNow().Add(-72 * time.Hour)
	source.executedAt = request.ExecutedAt

	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Status != "unverifiable" ||
		verdict.Reason != "insufficient_samples" {
		t.Fatalf("hard-max verdict = %#v, want safe revert", verdict)
	}
}

func TestWatchObservationErrorsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeObservationSource)
	}{
		{name: "query", mutate: func(s *fakeObservationSource) { s.queryErr = errObservation }},
		{name: "write", mutate: func(s *fakeObservationSource) { s.writeErr = errObservation }},
		{name: "index", mutate: func(s *fakeObservationSource) { s.indexErr = errObservation }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeObservationSource()
			test.mutate(source)
			verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
				context.Background(), successfulWatchRequest("error-"+test.name),
			)
			if !errors.Is(err, errObservation) {
				t.Fatalf("Watch error = %v, want observation error", err)
			}
			if !verdict.Revert || verdict.Status != "unverifiable" {
				t.Fatalf("error verdict = %#v, want fail-closed revert", verdict)
			}
		})
	}
}

func TestWatchCancellationWhileReadingMetricsFailsClosed(t *testing.T) {
	source := newFakeObservationSource()
	source.queryStarted = make(chan struct{}, 1)
	source.queryGate = make(chan struct{})
	engine := newTestEngine(t, source, newMemoryStateStore())
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		verdict Verdict
		err     error
	}
	done := make(chan result, 1)
	go func() {
		verdict, err := engine.Watch(ctx, successfulWatchRequest("cancel-read"))
		done <- result{verdict: verdict, err: err}
	}()
	<-source.queryStarted
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Watch error = %v, want canceled", got.err)
	}
	if !got.verdict.Revert || got.verdict.Reason != "context_canceled" {
		t.Fatalf("canceled verdict = %#v", got.verdict)
	}
}

func TestEngineSupportsConcurrentIndependentWatches(t *testing.T) {
	source := newFakeObservationSource()
	store := newMemoryStateStore()
	engine := newTestEngine(t, source, store)
	const count = 16
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := successfulWatchRequest(fmt.Sprintf("watch-%d", i))
			verdict, err := engine.Watch(context.Background(), request)
			if err == nil && !verdict.Retain {
				err = fmt.Errorf("watch %d not retained: %#v", i, verdict)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Watch: %v", err)
		}
	}
	if store.created != count || store.updated != count {
		t.Fatalf("store creates/updates = %d/%d, want %d/%d",
			store.created, store.updated, count, count)
	}
}

func TestWatchRejectsUnknownCriterion(t *testing.T) {
	request := successfulWatchRequest("unknown")
	request.Criterion.Kind = "decorative_rationale"
	verdict, err := newTestEngine(
		t, newFakeObservationSource(), newMemoryStateStore(),
	).Watch(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "criterion") {
		t.Fatalf("Watch error = %v, want criterion error", err)
	}
	if !verdict.Revert || verdict.Status != "unverifiable" {
		t.Fatalf("unknown criterion verdict = %#v", verdict)
	}
}
