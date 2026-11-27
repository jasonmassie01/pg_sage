package verify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// executor-02: a transient read error while observing a watch is not
// evidence. The watch stays open (observation_retry) with a backoff, and only
// the hard deadline without a successful observation reverts it.

// testClock is declared in io_monitor_integration_test.go.
func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func newClockedEngine(
	t *testing.T, source ObservationSource, store StateStore, clock *testClock,
) *Engine {
	t.Helper()
	options := DefaultOptions()
	options.Now = clock.Now
	engine, err := NewEngine(source, store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// seedRetryWatch stores an open watch already in observation_retry, due now.
func seedRetryWatch(
	t *testing.T, store *memoryStateStore, id string, actionID int64, failures int,
) WatchState {
	t.Helper()
	request := successfulWatchRequest(id)
	request.ActionID = actionID
	state := WatchStateFromRequest(request)
	state.Status, state.Reason = "extended", "observation_retry: earlier read failed"
	state.ObservationFailures = failures
	state.NextEvaluationAt = testVerificationNow()
	if err := store.Create(context.Background(), state); err != nil {
		t.Fatalf("seed retry watch: %v", err)
	}
	return state
}

func TestObservationRetryRecoversToVerdict(t *testing.T) {
	source := newFakeObservationSource()
	source.setQueryErr(errObservation)
	store := newMemoryStateStore()
	clock := &testClock{now: testVerificationNow()}
	engine := newClockedEngine(t, source, store, clock)
	request := successfulWatchRequest("recovers")

	verdict, err := engine.Watch(context.Background(), request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	assertObservationRetry(t, verdict, 1, testVerificationNow().Add(5*time.Minute))

	source.setQueryErr(nil)
	clock.Set(verdict.NextEvaluationAt)
	results, err := engine.ResumeDueResults(context.Background())
	if err != nil {
		t.Fatalf("ResumeDueResults: %v", err)
	}
	if len(results) != 1 || !results[0].Verdict.Retain || results[0].Verdict.Revert {
		t.Fatalf("recovered results = %#v, want one retained verdict", results)
	}
	state, err := store.Get(context.Background(), request.ID)
	if err != nil || !state.Completed || state.Status != "success" {
		t.Fatalf("recovered state = %#v, %v; want completed success", state, err)
	}
	if state.ObservationFailures != 0 {
		t.Fatalf("observation failures = %d after a successful observation, want 0",
			state.ObservationFailures)
	}
}

func TestObservationRetryBackoffGrowsToAnHour(t *testing.T) {
	source := newFakeObservationSource()
	source.setQueryErr(errObservation)
	store := newMemoryStateStore()
	clock := &testClock{now: testVerificationNow()}
	engine := newClockedEngine(t, source, store, clock)
	request := successfulWatchRequest("backoff")
	verdict, err := engine.Watch(context.Background(), request)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	assertObservationRetry(t, verdict, 1, clock.Now().Add(5*time.Minute))
	for i, wantDelay := range []time.Duration{15 * time.Minute, time.Hour, time.Hour} {
		clock.Set(verdict.NextEvaluationAt)
		results, resumeErr := engine.ResumeDueResults(context.Background())
		if resumeErr != nil || len(results) != 1 {
			t.Fatalf("resume %d = %#v, %v; want one result", i, results, resumeErr)
		}
		verdict = results[0].Verdict
		assertObservationRetry(t, verdict, i+2, clock.Now().Add(wantDelay))
		state, getErr := store.Get(context.Background(), request.ID)
		if getErr != nil || state.ObservationFailures != i+2 ||
			!state.NextEvaluationAt.Equal(verdict.NextEvaluationAt) {
			t.Fatalf("stored state %d = %#v, %v", i, state, getErr)
		}
	}
}

func TestObservationRetryRevertsOnlyAtHardDeadline(t *testing.T) {
	request := successfulWatchRequest("deadline")
	deadline := request.ExecutedAt.Add(request.Criterion.HardMax)
	tests := []struct {
		name       string
		now        time.Time
		wantRevert bool
		wantNext   time.Time
	}{
		{name: "an hour and a minute before", now: deadline.Add(-61 * time.Minute),
			wantNext: deadline.Add(-time.Minute)},
		{name: "one second before", now: deadline.Add(-time.Second), wantNext: deadline},
		{name: "exactly at", now: deadline, wantRevert: true},
		{name: "an hour after", now: deadline.Add(time.Hour), wantRevert: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeObservationSource()
			source.setQueryErr(errObservation)
			store := newMemoryStateStore()
			seedRetryWatch(t, store, request.ID, request.ActionID, 3)
			clock := &testClock{now: test.now}
			results, err := newClockedEngine(t, source, store, clock).
				ResumeDueResults(context.Background())
			if err != nil || len(results) != 1 {
				t.Fatalf("ResumeDueResults = %#v, %v", results, err)
			}
			verdict := results[0].Verdict
			if !test.wantRevert {
				assertObservationRetry(t, verdict, 4, test.wantNext)
				return
			}
			if !verdict.Revert || verdict.Status != "unverifiable" ||
				verdict.Reason != "observation_unavailable" {
				t.Fatalf("deadline verdict = %#v, want observation_unavailable revert", verdict)
			}
			state, _ := store.Get(context.Background(), request.ID)
			if state.Completed || state.Status != "unverifiable" {
				t.Fatalf("stored state = %#v, want an open revert decision", state)
			}
		})
	}
}

func TestObservationAtHardDeadlineStillDecidesOnEvidence(t *testing.T) {
	request := successfulWatchRequest("deadline-healthy")
	store := newMemoryStateStore()
	seedRetryWatch(t, store, request.ID, request.ActionID, 7)
	clock := &testClock{now: request.ExecutedAt.Add(request.Criterion.HardMax)}
	results, err := newClockedEngine(t, newFakeObservationSource(), store, clock).
		ResumeDueResults(context.Background())
	if err != nil || len(results) != 1 || !results[0].Verdict.Retain {
		t.Fatalf("ResumeDueResults = %#v, %v; want the improvement retained", results, err)
	}
}

func TestObservationRetrySurvivesRestart(t *testing.T) {
	source := newFakeObservationSource()
	source.setQueryErr(errObservation)
	store := newMemoryStateStore()
	clock := &testClock{now: testVerificationNow()}
	request := successfulWatchRequest("restart")
	if _, err := newClockedEngine(t, source, store, clock).Watch(
		context.Background(), request,
	); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	readsBefore := source.queries()
	source.setQueryErr(nil)
	clock.Set(clock.Now().Add(5 * time.Minute))

	restarted := newClockedEngine(t, source, store, clock)
	results, err := restarted.ResumeDueResults(context.Background())
	if err != nil || len(results) != 1 {
		t.Fatalf("ResumeDueResults after restart = %#v, %v", results, err)
	}
	if source.queries() <= readsBefore {
		t.Fatal("the restarted engine replayed a stored decision instead of observing")
	}
	if results[0].Verdict.Revert || !results[0].Verdict.Retain {
		t.Fatalf("restarted verdict = %#v, want the observed improvement", results[0].Verdict)
	}
}

func TestResumeCancellationNeverRevertsEvenPastDeadline(t *testing.T) {
	request := successfulWatchRequest("cancel-past-deadline")
	source := newFakeObservationSource()
	source.queryStarted = make(chan struct{}, 1)
	source.queryGate = make(chan struct{})
	store := newMemoryStateStore()
	seedRetryWatch(t, store, request.ID, request.ActionID, 2)
	clock := &testClock{now: request.ExecutedAt.Add(request.Criterion.HardMax + time.Hour)}
	engine := newClockedEngine(t, source, store, clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []ResumeResult, 1)
	go func() {
		results, _ := engine.ResumeDueResults(ctx)
		done <- results
	}()
	<-source.queryStarted
	cancel()
	results := <-done
	if len(results) != 1 {
		t.Fatalf("results = %#v, want the claimed watch", results)
	}
	if verdict := results[0].Verdict; verdict.Revert || verdict.Status != "extended" {
		t.Fatalf("canceled verdict = %#v: a shutdown is not missing evidence", verdict)
	}
}

type failClaimStore struct {
	*memoryStateStore
	failAction int64
}

func (s *failClaimStore) Claim(
	ctx context.Context, listed WatchState, now, leaseUntil time.Time,
) (WatchState, bool, error) {
	if listed.ActionID == s.failAction {
		return WatchState{}, false, errors.New("claim connection reset")
	}
	return s.memoryStateStore.Claim(ctx, listed, now, leaseUntil)
}

type failUpdateForStore struct {
	*memoryStateStore
	failAction int64
}

func (s *failUpdateForStore) Update(ctx context.Context, state WatchState) error {
	if state.ActionID == s.failAction {
		return errors.New("update connection reset")
	}
	return s.memoryStateStore.Update(ctx, state)
}

func TestResumeDueContinuesPastAClaimError(t *testing.T) {
	memory := newMemoryStateStore()
	seedRetryWatch(t, memory, "claim-fails", 1, 1)
	seedRetryWatch(t, memory, "claim-ok", 2, 1)
	store := &failClaimStore{memoryStateStore: memory, failAction: 1}
	results, err := newTestEngine(t, newFakeObservationSource(), store).
		ResumeDueResults(context.Background())
	if err == nil || !strings.Contains(err.Error(), "action 1") ||
		!strings.Contains(err.Error(), "claim connection reset") {
		t.Fatalf("ResumeDueResults error = %v, want the claim error for action 1", err)
	}
	if len(results) != 1 || results[0].ActionID != 2 || !results[0].Verdict.Retain {
		t.Fatalf("results = %#v, want action 2 evaluated and retained", results)
	}
}

func TestResumeDueContinuesPastAnUpdateError(t *testing.T) {
	memory := newMemoryStateStore()
	failing := seedRetryWatch(t, memory, "update-fails", 1, 2)
	seedRetryWatch(t, memory, "update-ok", 2, 2)
	store := &failUpdateForStore{memoryStateStore: memory, failAction: 1}
	source := newFakeObservationSource()
	clock := &testClock{now: testVerificationNow()}
	engine := newClockedEngine(t, source, store, clock)
	results, err := engine.ResumeDueResults(context.Background())
	if err == nil || !strings.Contains(err.Error(), "update connection reset") {
		t.Fatalf("ResumeDueResults error = %v, want the update error", err)
	}
	byAction := map[int64]Verdict{}
	for _, result := range results {
		byAction[result.ActionID] = result.Verdict
	}
	if got := byAction[1]; got.Revert || got.Retain || got.Reason != "state_update_failed" {
		t.Fatalf("failed-update verdict = %#v, want extended state_update_failed", got)
	}
	if !byAction[2].Retain {
		t.Fatalf("action 2 verdict = %#v, want retained", byAction[2])
	}
	stored, _ := memory.Get(context.Background(), failing.ID)
	if stored.Status != failing.Status || stored.ObservationFailures != 2 || stored.Completed {
		t.Fatalf("stored row = %#v, want it unchanged except the claim lease", stored)
	}
	clock.Set(stored.NextEvaluationAt)
	store.failAction = 0
	reads := source.queries()
	again, err := engine.ResumeDueResults(context.Background())
	if err != nil || len(again) != 1 || again[0].ActionID != 1 || !again[0].Verdict.Retain {
		t.Fatalf("next resume = %#v, %v; want action 1 observed again", again, err)
	}
	if source.queries() <= reads {
		t.Fatal("the next resume did not observe the watch again")
	}
}

func TestResumeDueWithoutDueWatchesDoesNoWork(t *testing.T) {
	source := newFakeObservationSource()
	results, err := newTestEngine(t, source, newMemoryStateStore()).
		ResumeDueResults(context.Background())
	if err != nil || len(results) != 0 {
		t.Fatalf("ResumeDueResults = %#v, %v; want nothing", results, err)
	}
	if source.queries() != 0 {
		t.Fatalf("source reads = %d with nothing due, want 0", source.queries())
	}
}

func TestConcurrentResumeOfRetryRowEvaluatesOnce(t *testing.T) {
	source := newFakeObservationSource()
	store := newMemoryStateStore()
	seedRetryWatch(t, store, "contended-retry", 9, 2)
	engine := newTestEngine(t, source, store)
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	evaluated := 0
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results, err := engine.ResumeDueResults(context.Background())
			if err != nil {
				t.Errorf("ResumeDueResults: %v", err)
			}
			mu.Lock()
			evaluated += len(results)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if evaluated != 1 {
		t.Fatalf("evaluations = %d across %d workers, want 1", evaluated, workers)
	}
	if source.queries() != 2 {
		t.Fatalf("query reads = %d, want 2 (before and after) for one evaluation",
			source.queries())
	}
}

func TestWatchWithoutDurableStateIsMarkedNotPersisted(t *testing.T) {
	store := newMemoryStateStore()
	store.err = errors.New("durable store down")
	verdict, err := newTestEngine(t, newFakeObservationSource(), store).Watch(
		context.Background(), successfulWatchRequest("create-fails"),
	)
	if !errors.Is(err, ErrStateNotPersisted) || !verdict.Revert {
		t.Fatalf("Create failure = %#v, %v; want a revert marked not persisted", verdict, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	healthy := newMemoryStateStore()
	verdict, err = newTestEngine(t, newFakeObservationSource(), healthy).Watch(
		ctx, successfulWatchRequest("canceled-before-create"),
	)
	if !errors.Is(err, ErrStateNotPersisted) || !errors.Is(err, context.Canceled) ||
		!verdict.Revert || verdict.Reason != "context_canceled" {
		t.Fatalf("canceled before Create = %#v, %v", verdict, err)
	}
	if healthy.created != 0 {
		t.Fatalf("created = %d, want no durable watch", healthy.created)
	}
}

func TestObservationRetryBackoffSchedule(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 5 * time.Minute}, {failures: 1, want: 5 * time.Minute},
		{failures: 2, want: 15 * time.Minute}, {failures: 3, want: time.Hour},
		{failures: 40, want: time.Hour}, {failures: -1, want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := observationRetryDelay(test.failures); got != test.want {
			t.Errorf("observationRetryDelay(%d) = %s, want %s", test.failures, got, test.want)
		}
	}
}
