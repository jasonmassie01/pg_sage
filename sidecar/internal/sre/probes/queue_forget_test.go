package probes

import (
	"context"
	"sync"
	"testing"
	"time"
)

// sre-06: a database removed from the fleet takes its probe queue series
// with it, like every other per-database series on /metrics.

func hasQueueSeries(database string) bool {
	for _, s := range QueueStats() {
		if s.Database == database {
			return true
		}
	}
	return false
}

func TestForgetQueueStats_DropsOnlyThatDatabase(t *testing.T) {
	observeQueue("forget_gone", CallerInvestigation, LimiterSidecar, time.Second, true)
	observeQueue("forget_gone", CallerRunway, LimiterDatabase, time.Millisecond, false)
	observeQueue("forget_kept", CallerDetector, LimiterSidecar, 2*time.Second, true)
	ForgetQueueStats("forget_gone")
	if hasQueueSeries("forget_gone") {
		t.Fatalf("forgotten database still has series: %+v", QueueStats())
	}
	kept := statFor("forget_kept", CallerDetector, LimiterSidecar)
	if kept.Waits != 1 || kept.Refusals != 1 || kept.WaitSeconds != 2 {
		t.Fatalf("other database's series = %+v, want 1 wait, 1 refusal, 2 s", kept)
	}
}

func TestForgetQueueStats_ReAddedDatabaseStartsAtZero(t *testing.T) {
	observeQueue("forget_readd", CallerInvestigation, LimiterSidecar, time.Second, true)
	observeQueue("forget_readd", CallerInvestigation, LimiterSidecar, time.Second, true)
	ForgetQueueStats("forget_readd")
	observeQueue("forget_readd", CallerInvestigation, LimiterSidecar, time.Second, false)
	got := statFor("forget_readd", CallerInvestigation, LimiterSidecar)
	if got.Waits != 1 || got.Refusals != 0 || got.WaitSeconds != 1 {
		t.Fatalf("re-added database = %+v, want one granted 1 s wait", got)
	}
}

func TestForgetQueueStats_EmptyAndUnknownNamesChangeNothing(t *testing.T) {
	observeQueue("forget_untouched", CallerNarration, LimiterDatabase, time.Second, true)
	before := len(QueueStats())
	ForgetQueueStats("forget_never_seen")
	ForgetQueueStats("")
	if len(QueueStats()) != before {
		t.Fatalf("forgetting an unknown or empty name dropped series: %d -> %d", before,
			len(QueueStats()))
	}
	if got := statFor("forget_untouched", CallerNarration, LimiterDatabase); got.Refusals != 1 {
		t.Fatalf("untouched database = %+v, want its refusal", got)
	}
}

// Every series carries the database it belongs to: a runner with no name
// and no pool (an embedder's or a fixture's) is not exported, as the park
// counters do, so /metrics never shows database="".
func TestObserveQueue_UnnamedRunnerIsNotExported(t *testing.T) {
	observeQueue("", CallerOther, LimiterSidecar, time.Second, true)
	r := NewRunner(nil, Catalog(), NewLimiter(1))
	if err := r.WithSlot(context.Background(),
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("WithSlot = %v", err)
	}
	if hasQueueSeries("") {
		t.Fatalf("unnamed runner exported a series: %+v", QueueStats())
	}
}

func TestForgetQueueStats_ConcurrentWithObserve(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			observeQueue("forget_race", CallerRunway, LimiterSidecar, time.Millisecond, true)
		}()
		go func() {
			defer wg.Done()
			ForgetQueueStats("forget_race")
		}()
	}
	wg.Wait()
	ForgetQueueStats("forget_race")
	if hasQueueSeries("forget_race") {
		t.Fatal("series survived the final forget")
	}
}
