package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-02: the lifecycle around the real verification engine. A failed
// read, a failed state update or a shutdown after the durable watch exists
// keeps the created index; only a watch that was never persisted, or the
// hard deadline without any observation, reverts it.

var errHistoryRead = errors.New("history store: connection refused")

type retrySource struct {
	mu      sync.Mutex
	histErr error
	reads   int
}

func (s *retrySource) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.histErr = err
}

func (s *retrySource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *retrySource) QueryMeasurements(
	ctx context.Context, ids []int64, _, to time.Time,
) (map[int64]verify.Measurement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.histErr != nil {
		return nil, s.histErr
	}
	latency := 100 * time.Millisecond
	if to.After(retryClockStart()) {
		latency = 60 * time.Millisecond
	}
	result := map[int64]verify.Measurement{}
	for _, id := range ids {
		result[id] = verify.Measurement{Samples: 60, AverageLatency: latency, Buckets: 12}
	}
	return result, nil
}

// WriteCost reports no attributable table writes: insufficient write
// evidence, so the queries decide.
func (s *retrySource) WriteCost(
	context.Context, string, time.Time, time.Time,
) (verify.WriteCost, error) {
	return verify.WriteCost{}, nil
}

func (s *retrySource) IndexValid(context.Context, string) (bool, error) { return true, nil }

func (s *retrySource) LoadEvidence(context.Context) (verify.LoadEvidence, error) {
	return verify.LoadEvidence{}, verify.ErrLoadTelemetryUnavailable
}

type retryStore struct {
	mu        sync.Mutex
	states    map[int64]verify.WatchState
	createErr error
	updateErr error
	onCreate  func()
}

func newRetryStore() *retryStore { return &retryStore{states: map[int64]verify.WatchState{}} }

func (s *retryStore) Create(_ context.Context, state verify.WatchState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	s.states[state.ActionID] = state
	if s.onCreate != nil {
		s.onCreate()
	}
	return nil
}

func (s *retryStore) Update(_ context.Context, state verify.WatchState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	s.states[state.ActionID] = state
	return nil
}

func (s *retryStore) Get(_ context.Context, id string) (verify.WatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states {
		if state.ID == id {
			return state, nil
		}
	}
	return verify.WatchState{}, errors.New("watch not found")
}

func (s *retryStore) ListDue(_ context.Context, now time.Time) ([]verify.WatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []verify.WatchState
	for _, state := range s.states {
		if !state.Completed && !state.NextEvaluationAt.After(now) {
			due = append(due, state)
		}
	}
	return due, nil
}

func (s *retryStore) Claim(
	_ context.Context, listed verify.WatchState, now, leaseUntil time.Time,
) (verify.WatchState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[listed.ActionID]
	if !ok || state.Completed || state.NextEvaluationAt.After(now) {
		return verify.WatchState{}, false, nil
	}
	state.NextEvaluationAt = leaseUntil
	s.states[listed.ActionID] = state
	return state, true, nil
}

func (s *retryStore) row(actionID int64) verify.WatchState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[actionID]
}

// engineVerifier adapts the real engine to the lifecycle, as
// postgresIndexVerifier does, without the action_log rollback lookup.
type engineVerifier struct{ engine *verify.Engine }

func (v engineVerifier) OKToApplyNow(context.Context) (verify.Admission, error) {
	return verify.Admission{OK: true}, nil
}

func (v engineVerifier) Watch(
	ctx context.Context, request verify.WatchRequest,
) (verify.Verdict, error) {
	return v.engine.Watch(ctx, request)
}

func (v engineVerifier) ResumeDue(ctx context.Context) ([]resumedIndexVerification, error) {
	results, err := v.engine.ResumeDueResults(ctx)
	resumed := make([]resumedIndexVerification, 0, len(results))
	for _, result := range results {
		resumed = append(resumed, resumedIndexVerification{
			ActionID: result.ActionID, RollbackSQL: "DROP INDEX CONCURRENTLY idx_retry",
			Verdict: result.Verdict,
		})
	}
	return resumed, err
}

func retryClockStart() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) }

type retryClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *retryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *retryClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type retryHarness struct {
	source    *retrySource
	store     *retryStore
	clock     *retryClock
	actions   *fakeVerifiedIndexActions
	lifecycle *verifiedIndexLifecycle
}

func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()
	h := &retryHarness{source: &retrySource{}, store: newRetryStore(),
		clock: &retryClock{now: retryClockStart()}, actions: &fakeVerifiedIndexActions{}}
	options := verify.DefaultOptions()
	options.Now = h.clock.Now
	engine, err := verify.NewEngine(h.source, h.store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	h.lifecycle = newVerifiedIndexLifecycle(engineVerifier{engine: engine}, h.actions)
	h.lifecycle.now = h.clock.Now
	return h
}

func retryIndexAction() verifiedIndexAction {
	return verifiedIndexAction{
		RollbackSQL: "DROP INDEX CONCURRENTLY idx_retry", WatchID: "index-action-61",
		Table: "public.orders", IndexName: "public.idx_retry", QueryIDs: []int64{7},
		Criterion: verify.Criterion{Kind: "per_query_latency", Window: 2 * time.Hour,
			HardMax: 72 * time.Hour},
	}
}

func assertIndexKept(t *testing.T, actions *fakeVerifiedIndexActions) {
	t.Helper()
	if actions.revertCalls != 0 {
		t.Fatalf("revert calls = %d (reason %q), want the index kept",
			actions.revertCalls, actions.revertReason)
	}
}

func TestLifecycleKeepsIndexWhenFirstHistoryReadFails(t *testing.T) {
	h := newRetryHarness(t)
	h.source.setErr(errHistoryRead)
	if err := h.lifecycle.WatchApplied(context.Background(), retryIndexAction(), 61); err != nil {
		t.Fatalf("WatchApplied: %v", err)
	}
	assertIndexKept(t, h.actions)
	row := h.store.row(61)
	if row.Status != "extended" || row.Completed ||
		!strings.HasPrefix(row.Reason, "observation_retry: ") ||
		!strings.Contains(row.Reason, "connection refused") {
		t.Fatalf("watch row = %#v, want an open observation_retry", row)
	}
}

func TestLifecycleKeepsIndexWhenResumeUpdateFails(t *testing.T) {
	h := newRetryHarness(t)
	h.source.setErr(errHistoryRead)
	if err := h.lifecycle.WatchApplied(context.Background(), retryIndexAction(), 61); err != nil {
		t.Fatalf("WatchApplied: %v", err)
	}
	h.source.setErr(nil)
	h.clock.Set(h.store.row(61).NextEvaluationAt)
	h.store.updateErr = errors.New("sage.verification: connection reset")
	err := h.lifecycle.ResumeDue(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("ResumeDue error = %v, want the update error", err)
	}
	assertIndexKept(t, h.actions)
	if h.actions.retainCalls != 0 {
		t.Fatalf("retain calls = %d before the verdict was stored, want 0",
			h.actions.retainCalls)
	}

	h.store.updateErr = nil
	h.clock.Set(h.store.row(61).NextEvaluationAt)
	reads := h.source.readCount()
	if err := h.lifecycle.ResumeDue(context.Background()); err != nil {
		t.Fatalf("next ResumeDue: %v", err)
	}
	if h.source.readCount() <= reads || h.actions.retainCalls != 1 {
		t.Fatalf("next resume reads %d->%d, retains %d; want observed and retained",
			reads, h.source.readCount(), h.actions.retainCalls)
	}
	assertIndexKept(t, h.actions)
}

func TestLifecycleRevertsWhenCreateFails(t *testing.T) {
	h := newRetryHarness(t)
	h.store.createErr = errors.New("sage.verification: permission denied")
	err := h.lifecycle.WatchApplied(context.Background(), retryIndexAction(), 61)
	if !errors.Is(err, verify.ErrStateNotPersisted) {
		t.Fatalf("WatchApplied error = %v, want ErrStateNotPersisted", err)
	}
	if h.actions.revertCalls != 1 || h.actions.revertReason != "state_persist_failed" {
		t.Fatalf("revert calls/reason = %d/%q, want 1/state_persist_failed",
			h.actions.revertCalls, h.actions.revertReason)
	}
}

func TestLifecycleCancellationAfterCreateKeepsIndex(t *testing.T) {
	h := newRetryHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.store.onCreate = cancel
	if err := h.lifecycle.WatchApplied(ctx, retryIndexAction(), 61); err != nil {
		t.Fatalf("WatchApplied: %v", err)
	}
	assertIndexKept(t, h.actions)
	if row := h.store.row(61); row.Completed || row.Status != "extended" {
		t.Fatalf("watch row = %#v, want open", row)
	}
}

func TestLifecycleCancellationBeforeCreateReverts(t *testing.T) {
	h := newRetryHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.lifecycle.WatchApplied(ctx, retryIndexAction(), 61)
	if !errors.Is(err, context.Canceled) || h.actions.revertCalls != 1 {
		t.Fatalf("WatchApplied = %v with %d reverts, want a revert", err, h.actions.revertCalls)
	}
	if len(h.store.states) != 0 {
		t.Fatalf("stored watches = %d, want none", len(h.store.states))
	}
}

func TestLifecycleRevertsAtHardDeadlineWithoutObservation(t *testing.T) {
	h := newRetryHarness(t)
	h.source.setErr(errHistoryRead)
	action := retryIndexAction()
	if err := h.lifecycle.WatchApplied(context.Background(), action, 61); err != nil {
		t.Fatalf("WatchApplied: %v", err)
	}
	deadline := retryClockStart().Add(action.Criterion.HardMax)
	h.clock.Set(deadline.Add(-time.Second))
	if err := h.lifecycle.ResumeDue(context.Background()); err != nil {
		t.Fatalf("ResumeDue before the deadline: %v", err)
	}
	assertIndexKept(t, h.actions)
	h.clock.Set(deadline)
	if err := h.lifecycle.ResumeDue(context.Background()); err != nil {
		t.Fatalf("ResumeDue at the deadline: %v", err)
	}
	if h.actions.revertCalls != 1 || h.actions.revertReason != "observation_unavailable" {
		t.Fatalf("revert calls/reason = %d/%q, want 1/observation_unavailable",
			h.actions.revertCalls, h.actions.revertReason)
	}
}

func TestLifecycleKeepsIndexOnAnyOtherWatchError(t *testing.T) {
	verifier := &fakeIndexVerifier{
		watchVerdict: verify.Verdict{Revert: true, Status: "unverifiable"},
		watchErr:     errors.New("evaluate: unexpected"),
	}
	actions := &fakeVerifiedIndexActions{}
	err := newVerifiedIndexLifecycle(verifier, actions).
		WatchApplied(context.Background(), testVerifiedIndexAction(), 52)
	if err == nil || !strings.Contains(err.Error(), "action 52") ||
		!strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("WatchApplied error = %v, want the error with the action id", err)
	}
	assertIndexKept(t, actions)
}
