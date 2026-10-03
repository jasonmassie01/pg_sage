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

// The gain is judged on the call-weighted mean of every target (Phase
// 1.3): 42 runs 60 calls 100->70 ms and 43 runs 50 calls 200->150 ms, so
// the pooled mean falls from 145.5 to 106.4 ms (-26.9%). The old rule kept
// an index when any one target gained, however few calls it had; this
// test replaces TestWatchRetainsWhenAnyTargetMeetsGainAndNoneRegress.
func TestWatchRetainsWhenCallWeightedTargetsGainAndNoneRegress(t *testing.T) {
	source := newFakeObservationSource()
	source.before[43] = Measurement{Samples: 50, AverageLatency: 200 * time.Millisecond,
		Buckets: 12}
	source.after[43] = Measurement{Samples: 50, AverageLatency: 150 * time.Millisecond,
		Buckets: 12}
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
	if verdict.Samples != 110 {
		t.Fatalf("samples = %d, want 110 pooled after-window calls", verdict.Samples)
	}
	if verdict.Outcome != OutcomeImproved || verdict.ObservedPct == nil ||
		*verdict.ObservedPct > -26 || *verdict.ObservedPct < -28 {
		t.Fatalf("outcome = %s observed = %v, want improved at about -26.9%%",
			verdict.Outcome, verdict.ObservedPct)
	}
	if verdict.Evidence == nil || verdict.Evidence["comparison"] == nil ||
		verdict.Evidence["targets"] == nil {
		t.Fatalf("verdict evidence = %#v, want comparison and per-target evidence",
			verdict.Evidence)
	}
}

// One target gaining is not enough when the call-weighted mean does not
// move: 42 gains 30% on 60 calls, 43 is flat on 2000 calls.
func TestWatchRevertsWhenOnlyARareTargetGains(t *testing.T) {
	source := newFakeObservationSource()
	source.before[43] = Measurement{Samples: 2000, AverageLatency: 100 * time.Millisecond,
		Buckets: 12}
	source.after[43] = Measurement{Samples: 2000, AverageLatency: 100 * time.Millisecond,
		Buckets: 12}
	request := successfulWatchRequest("rare-gain")
	request.Criterion.TargetIDs = []int64{42, 43}

	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Revert || verdict.Reason != "no_gain" || verdict.Outcome != OutcomeNeutral {
		t.Fatalf("verdict = %#v, want no-gain revert with a neutral outcome", verdict)
	}
}

// Noisy samples do not flip the verdict: an apparent +60% whose interval
// spans zero extends the window instead of reverting.
func TestWatchNoisySamplesExtendInsteadOfReverting(t *testing.T) {
	source := newFakeObservationSource()
	source.before[42] = Measurement{Samples: 400, AverageLatency: 100 * time.Millisecond,
		StdErr: 30 * time.Millisecond, Buckets: 6}
	source.after[42] = Measurement{Samples: 400, AverageLatency: 160 * time.Millisecond,
		StdErr: 60 * time.Millisecond, Buckets: 6}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("noisy"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Revert || verdict.Retain || verdict.Status != "extended" {
		t.Fatalf("noisy verdict = %#v, want an extended window", verdict)
	}
}

func TestWatchRegressionCarriesRegressedOutcome(t *testing.T) {
	source := newFakeObservationSource()
	source.after[42] = Measurement{Samples: 60, AverageLatency: 140 * time.Millisecond,
		Buckets: 12}
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("regressed-outcome"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Outcome != OutcomeRegressed || verdict.ObservedPct == nil ||
		*verdict.ObservedPct < 39 || *verdict.ObservedPct > 41 {
		t.Fatalf("verdict = %#v, want regressed at +40%%", verdict)
	}
}

func TestWatchHardMaxWithoutSamplesIsInsufficientEvidence(t *testing.T) {
	source := newFakeObservationSource()
	source.after = map[int64]Measurement{}
	request := successfulWatchRequest("hard-max-outcome")
	request.Criterion.Window = 72 * time.Hour
	request.ExecutedAt = testVerificationNow().Add(-72 * time.Hour)
	source.executedAt = request.ExecutedAt
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), request,
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Outcome != OutcomeInsufficient {
		t.Fatalf("outcome = %q, want insufficient_evidence (never success)", verdict.Outcome)
	}
}

func TestWatchInvalidIndexIsUnverifiableOutcome(t *testing.T) {
	source := newFakeObservationSource()
	source.indexValid = false
	verdict, err := newTestEngine(t, source, newMemoryStateStore()).Watch(
		context.Background(), successfulWatchRequest("invalid-outcome"),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if verdict.Outcome != OutcomeUnverifiable {
		t.Fatalf("outcome = %q, want unverifiable", verdict.Outcome)
	}
}

func TestWatchRevertsWhenAnyTargetRegresses(t *testing.T) {
	source := newFakeObservationSource()
	source.before[43] = Measurement{Samples: 40, AverageLatency: 100 * time.Millisecond, Buckets: 12}
	source.after[43] = Measurement{Samples: 40, AverageLatency: 116 * time.Millisecond, Buckets: 12}
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
	source.after[42] = Measurement{Samples: 60, AverageLatency: 115 * time.Millisecond, Buckets: 12}
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
	source.after[42] = Measurement{Samples: 60, AverageLatency: 81 * time.Millisecond, Buckets: 12}
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
	source.after[42] = Measurement{Samples: 60, AverageLatency: 80 * time.Millisecond, Buckets: 12}
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
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 13 * time.Millisecond, Buckets: 12}
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
	source.writeAfter = Measurement{Samples: 60, AverageLatency: 12 * time.Millisecond, Buckets: 12}
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
	source.after[42] = Measurement{Samples: 29, AverageLatency: 60 * time.Millisecond, Buckets: 12}
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
			source.after[42] = Measurement{Samples: 2, AverageLatency: time.Millisecond, Buckets: 12}
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
