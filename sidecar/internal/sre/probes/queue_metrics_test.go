package probes

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sre-06: the probe slots record queue wait and refusals per database,
// caller class and limiter, so a fleet shows how often an investigation
// loses its slot to background sampling.

// lazyPool is a pool that never connects: a probe refused at the queue
// never reaches it.
func lazyPool(t *testing.T, db string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/"+db+
		"?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// fill takes n slots of l and returns their release.
func fill(l *Limiter, n int) func() {
	for range n {
		l.slots <- struct{}{}
	}
	return func() {
		for range n {
			l.release()
		}
	}
}

// statFor is the queue stat of one series (zero when absent).
func statFor(db string, c Caller, limiter string) QueueStat {
	for _, s := range QueueStats() {
		if s.Database == db && s.Caller == c && s.Limiter == limiter {
			return s
		}
	}
	return QueueStat{}
}

func shortQueue(r *Runner) *Runner {
	r.queueWait = 80 * time.Millisecond
	return r
}

func TestQueueRefusal_CountsCallerAndSidecarLimiter(t *testing.T) {
	for _, size := range []int{1, MaxSidecarConcurrency} {
		db := "refuse_sidecar_" + string(rune('a'+size))
		global := NewLimiter(size)
		defer fill(global, size)()
		r := shortQueue(NewRunner(lazyPool(t, db), Catalog(), global))
		ctx := WithCaller(context.Background(), CallerInvestigation)
		res := r.Run(ctx, LockGraph, Args{})
		if !QueueRefused(res) || res.Status != StatusError {
			t.Fatalf("size %d: result = %+v, want a queue refusal", size, res)
		}
		got := statFor(db, CallerInvestigation, LimiterSidecar)
		if got.Refusals != 1 || got.Waits != 1 || got.WaitSeconds < 0.07 {
			t.Fatalf("size %d: sidecar stat = %+v, want 1 refusal after ~80 ms", size, got)
		}
		local := statFor(db, CallerInvestigation, LimiterDatabase)
		if local.Refusals != 0 || local.Waits != 1 {
			t.Fatalf("size %d: database stat = %+v, want 1 granted wait", size, local)
		}
		if len(r.local.slots) != 0 || len(global.slots) != size {
			t.Fatalf("size %d: slots leaked: local %d global %d", size,
				len(r.local.slots), len(global.slots))
		}
	}
}

func TestQueueRefusal_DatabaseSlotIsTheDatabaseLimiter(t *testing.T) {
	const db = "refuse_database"
	r := shortQueue(NewRunner(lazyPool(t, db), Catalog(), NewLimiter(4)))
	defer fill(r.local, 1)()
	res := r.Run(WithCaller(context.Background(), CallerRunway), WALRunwayProbe, Args{})
	if !QueueRefused(res) {
		t.Fatalf("result = %+v, want a queue refusal", res)
	}
	if got := statFor(db, CallerRunway, LimiterDatabase); got.Refusals != 1 || got.Waits != 1 {
		t.Fatalf("database stat = %+v, want one refusal", got)
	}
	if got := statFor(db, CallerRunway, LimiterSidecar); got != (QueueStat{}) {
		t.Fatalf("sidecar stat = %+v, want none: the probe never reached it", got)
	}
	if len(r.global.slots) != 0 {
		t.Fatal("a refusal at the database slot took a sidecar slot")
	}
}

func TestQueueGranted_RecordsWaitWithoutRefusal(t *testing.T) {
	r := NewRunner(nil, Catalog(), NewLimiter(1))
	r.SetDatabaseName("granted_db")
	ctx := WithCaller(context.Background(), CallerNarration)
	if err := r.WithSlot(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("WithSlot = %v", err)
	}
	for _, l := range []string{LimiterDatabase, LimiterSidecar} {
		got := statFor("granted_db", CallerNarration, l)
		if got.Waits != 1 || got.Refusals != 0 || got.WaitSeconds < 0 {
			t.Fatalf("%s stat = %+v, want one granted wait", l, got)
		}
	}
}

func TestQueueStats_UntaggedCallerIsOther(t *testing.T) {
	r := shortQueue(NewRunner(nil, Catalog(), NewLimiter(1)))
	r.SetDatabaseName("untagged_db")
	defer fill(r.global, 1)()
	_ = r.WithSlot(context.Background(), func(context.Context) error { return nil })
	if got := statFor("untagged_db", CallerOther, LimiterSidecar); got.Refusals != 1 {
		t.Fatalf("untagged stat = %+v, want one refusal as caller other", got)
	}
	if CallerOf(context.Background()) != CallerOther {
		t.Fatal("an untagged context must read as caller other")
	}
	if CallerOf(WithCaller(context.Background(), "")) != CallerOther {
		t.Fatal("an empty caller must read as caller other")
	}
}

func TestQueueStats_DatabaseLabelFallsBackToPool(t *testing.T) {
	r := shortQueue(NewRunner(lazyPool(t, "pool_label_db"), Catalog(), NewLimiter(1)))
	defer fill(r.global, 1)()
	_ = r.Run(WithCaller(context.Background(), CallerDetector), CheckpointActivity, Args{})
	if got := statFor("pool_label_db", CallerDetector, LimiterSidecar); got.Refusals != 1 {
		t.Fatalf("stat = %+v, want the pool's database as the label", got)
	}
	r.SetDatabaseName("fleet-name")
	_ = r.Run(WithCaller(context.Background(), CallerDetector), CheckpointActivity, Args{})
	if got := statFor("fleet-name", CallerDetector, LimiterSidecar); got.Refusals != 1 {
		t.Fatalf("stat = %+v, want the set name as the label", got)
	}
}

func TestQueueRefused_OnlyQueueConcurrencyLimit(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"queue refusal", Result{Status: StatusError, Phase: PhaseQueue,
			Reason: "concurrency_limit"}, true},
		{"zero result", Result{}, false},
		{"pool acquire timeout", Result{Status: StatusError, Phase: PhasePoolAcquire,
			Reason: "concurrency_limit"}, false},
		{"other queue reason", Result{Status: StatusError, Phase: PhaseQueue,
			Reason: "canceled"}, false},
		{"ok", Result{Status: StatusOK, Phase: PhaseQueue,
			Reason: "concurrency_limit"}, false},
	}
	for _, tc := range cases {
		if got := QueueRefused(tc.res); got != tc.want {
			t.Errorf("%s: QueueRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Four background holders fill the sidecar slots; an investigation probe
// and concurrent background probes are each refused and counted exactly
// once per refusal.
func TestQueueRefusal_ConcurrentHoldersCountOncePerRefusal(t *testing.T) {
	global := NewLimiter(MaxSidecarConcurrency)
	holders := make([]*Runner, MaxSidecarConcurrency)
	held, release := make(chan struct{}, len(holders)), make(chan struct{})
	var wg sync.WaitGroup
	for i := range holders {
		holders[i] = NewRunner(nil, Catalog(), global)
		holders[i].SetDatabaseName("holder_db")
		wg.Add(1)
		go func(r *Runner) {
			defer wg.Done()
			_ = r.WithSlot(WithCaller(context.Background(), CallerRunway),
				func(context.Context) error { held <- struct{}{}; <-release; return nil })
		}(holders[i])
	}
	for range holders {
		<-held
	}
	const waiters = 6
	var refused sync.WaitGroup
	for i := range waiters {
		refused.Add(1)
		go func(i int) {
			defer refused.Done()
			r := shortQueue(NewRunner(nil, Catalog(), global))
			r.SetDatabaseName("contended_db")
			c := map[bool]Caller{true: CallerInvestigation, false: CallerDetector}[i == 0]
			_ = r.WithSlot(WithCaller(context.Background(), c),
				func(context.Context) error { return nil })
		}(i)
	}
	refused.Wait()
	close(release)
	wg.Wait()
	inv := statFor("contended_db", CallerInvestigation, LimiterSidecar)
	det := statFor("contended_db", CallerDetector, LimiterSidecar)
	if inv.Refusals != 1 || inv.Waits != 1 || det.Refusals != waiters-1 ||
		det.Waits != waiters-1 {
		t.Fatalf("refusals = investigation %+v detector %+v, want 1 and %d", inv, det,
			waiters-1)
	}
	if got := statFor("holder_db", CallerRunway, LimiterSidecar); got.Refusals != 0 ||
		got.Waits != MaxSidecarConcurrency {
		t.Fatalf("holders = %+v, want %d granted waits", got, MaxSidecarConcurrency)
	}
	if len(global.slots) != 0 {
		t.Fatalf("sidecar slots held after release = %d", len(global.slots))
	}
}

func TestQueueStats_SortedSnapshot(t *testing.T) {
	observeQueue("zz_sorted", CallerRunway, LimiterSidecar, time.Millisecond, true)
	observeQueue("aa_sorted", CallerRunway, LimiterSidecar, time.Millisecond, false)
	observeQueue("aa_sorted", CallerDetector, LimiterSidecar, time.Millisecond, false)
	stats := QueueStats()
	for i := 1; i < len(stats); i++ {
		a, b := stats[i-1], stats[i]
		if a.Database > b.Database || (a.Database == b.Database && a.Caller > b.Caller) ||
			(a.Database == b.Database && a.Caller == b.Caller && a.Limiter > b.Limiter) {
			t.Fatalf("stats not sorted at %d: %+v then %+v", i, a, b)
		}
	}
	stats[0].Refusals = 999 // a snapshot, not the live counters
	if statFor(stats[0].Database, stats[0].Caller, stats[0].Limiter).Refusals == 999 {
		t.Fatal("QueueStats returned the live counters")
	}
}

func TestRunnerQueueWait_DefaultAndNil(t *testing.T) {
	var nilRunner *Runner
	if nilRunner.QueueWait() != QueueWait {
		t.Fatalf("nil runner QueueWait = %s", nilRunner.QueueWait())
	}
	if got := NewRunner(nil, Catalog(), nil).QueueWait(); got != QueueWait ||
		QueueWait != 5*time.Second {
		t.Fatalf("QueueWait = %s (const %s), want 5 s", got, QueueWait)
	}
}
