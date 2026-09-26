package verify

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the auditor's concurrent-recovery finding: two workers
// that both listed one due watch must not both evaluate and finalize it.

// staleListStore returns a due list captured before another worker claimed,
// modelling a concurrent reader that listed at the same instant.
type staleListStore struct {
	*memoryStateStore
	listed []WatchState
}

func (s *staleListStore) ListDue(context.Context, time.Time) ([]WatchState, error) {
	return append([]WatchState(nil), s.listed...), nil
}

func TestResumeDueSkipsWatchClaimedByAnotherWorker(t *testing.T) {
	store := newMemoryStateStore()
	request := successfulWatchRequest("claim-1")
	state := WatchStateFromRequest(request)
	if err := store.Create(context.Background(), state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed, err := store.ListDue(context.Background(), testVerificationNow())
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListDue = %d, %v; want the one due watch", len(listed), err)
	}
	first := newTestEngine(t, regressingSource(), store)
	firstResults, err := first.ResumeDueResults(context.Background())
	if err != nil || len(firstResults) != 1 {
		t.Fatalf("first worker results = %#v, %v; want one claimed watch", firstResults, err)
	}
	second := newTestEngine(t, regressingSource(), &staleListStore{store, listed})

	results, err := second.ResumeDueResults(context.Background())

	if err != nil || len(results) != 0 {
		t.Fatalf("second worker results = %#v, %v; want the claimed watch skipped",
			results, err)
	}
}

func TestPendingRevertClaimLeasesUntilRetry(t *testing.T) {
	store := newMemoryStateStore()
	engine := newTestEngine(t, regressingSource(), store)
	if _, err := engine.Watch(context.Background(), successfulWatchRequest("lease-1")); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	options := DefaultOptions()
	later := testVerificationNow().Add(RevertRetryInterval + time.Second)
	options.Now = func() time.Time { return later }
	recovery, err := NewEngine(regressingSource(), store, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	results, err := recovery.ResumeDueResults(context.Background())
	again, againErr := recovery.ResumeDueResults(context.Background())

	if err != nil || len(results) != 1 || !results[0].Verdict.Revert {
		t.Fatalf("recovery = %#v, %v; want the owed revert", results, err)
	}
	if againErr != nil || len(again) != 0 {
		t.Fatalf("repeat within lease = %#v, %v; want nothing claimable", again, againErr)
	}
	stored, _ := store.Get(context.Background(), "lease-1")
	if stored.Completed || !stored.NextEvaluationAt.Equal(later.Add(RevertRetryInterval)) {
		t.Fatalf("stored = %#v; want open revert leased until %s", stored,
			later.Add(RevertRetryInterval))
	}
}

func TestPostgresClaimGrantsEachDueWatchOnce(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := WatchState{
		ID: fmt.Sprintf("claim-watch-%d", actionID), ActionID: actionID,
		ExecutedAt: now.Add(-time.Hour), Criterion: Criterion{
			Kind: "per_query_latency", TargetIDs: []int64{91},
			Window: time.Minute, HardMax: time.Hour,
		},
		Status: "pending", Window: time.Minute, NextEvaluationAt: now.Add(-time.Minute),
	}
	store := NewPostgresStateStore(pool, 1)
	if err := store.Create(ctx, state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var granted atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := store.Claim(ctx, state, now, now.Add(RevertRetryInterval))
			if err != nil {
				errs <- err
			}
			if ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Claim error: %v", err)
	}
	stored, err := store.Get(ctx, state.ID)
	if got := granted.Load(); got != 1 || err != nil {
		t.Fatalf("claims granted = %d (Get err %v); want exactly 1", got, err)
	}
	if !stored.NextEvaluationAt.Equal(now.Add(RevertRetryInterval)) {
		t.Fatalf("lease = %s, want %s", stored.NextEvaluationAt, now.Add(RevertRetryInterval))
	}
}
